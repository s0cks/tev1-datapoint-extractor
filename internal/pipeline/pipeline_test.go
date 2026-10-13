package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s0cks/tev1-rank/internal/config"
	"github.com/s0cks/tev1-rank/internal/rank"
	"github.com/s0cks/tev1-rank/internal/tev1"
)

const sampleDoc = `Revenue grew 14% to $2.3 billion in 2024, according to the annual report.

The office was quiet and the coffee was warm that morning.

In March 2023 the company opened its first plant in Ohio.

The office was quiet and the coffee was warm that morning.

Management believes the strategy will pay off over time.
`

// fakeOllama serves just enough of Ollama for the pipeline. Segments
// containing a digit look like stats; segments containing FAIL get a 500.
type fakeOllama struct {
	*httptest.Server
	calls   int32
	version string
	models  []string
	failAll bool
}

func newFake(t *testing.T) *fakeOllama {
	f := &fakeOllama{version: "0.35.2", models: []string{"tev1:0.8b"}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			json.NewEncoder(w).Encode(map[string]string{"version": f.version})
		case "/api/tags":
			var ms []map[string]string
			for _, m := range f.models {
				ms = append(ms, map[string]string{"name": m})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": ms})
		case "/v1/systemone":
			atomic.AddInt32(&f.calls, 1)
			var req struct {
				State struct {
					Segment string `json:"segment"`
				} `json:"state"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if f.failAll || strings.Contains(req.State.Segment, "FAIL") {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			stat := strings.ContainsAny(req.State.Segment, "0123456789")
			yes, kind, score := 0.05, "narrative", 0.5
			if stat {
				yes, kind, score = 0.92, "stat", 3.6
			}
			json.NewEncoder(w).Encode(map[string]any{"model": "tev1:0.8b", "answers": map[string]any{
				"is_datapoint": map[string]any{"choice": map[bool]string{true: "yes", false: "no"}[stat],
					"probabilities": map[string]float64{"yes": yes, "no": 0.93 - yes, "unknown": 0.07}, "confidence": 0.8},
				"kind":       map[string]any{"choice": kind, "probabilities": map[string]float64{kind: 0.85, "other": 0.15}, "confidence": 0.8},
				"importance": map[string]any{"score": score, "confidence": 0.7},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func writeInput(t *testing.T, dir, text string) string {
	p := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func opts(t *testing.T, f *fakeOllama, dir, input string) Options {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.OllamaURL = ""
	if f != nil {
		cfg.OllamaURL = f.URL
	}
	return Options{
		Config: cfg, InputPath: input, OutDir: filepath.Join(dir, "out"), CacheDir: filepath.Join(dir, "cache"),
		Select: rank.SelectOptions{Top: 3},
		Client: &tev1.Client{BaseURL: cfg.OllamaURL, Retries: 0, Timeout: 5 * time.Second},
	}
}

func readRecords(t *testing.T, dir string) []map[string]any {
	f, err := os.Open(filepath.Join(dir, "out", "scored.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestRunEndToEndAndCaching(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	in := writeInput(t, dir, sampleDoc)

	res, err := Run(context.Background(), opts(t, f, dir, in))
	if err != nil {
		t.Fatal(err)
	}
	// 5 segments, one text appears twice → 4 distinct hashes; the repeat is a cache hit.
	if got := atomic.LoadInt32(&f.calls); got != 4 {
		t.Errorf("model calls = %d, want 4", got)
	}
	c := res.Manifest.Counts
	if c.Segments != 5 || c.Duplicates != 1 || c.Ranked != 4 || c.Selected != 3 || c.CacheHits != 1 || c.ModelCalls != 4 {
		t.Errorf("counts = %+v", c)
	}
	if res.Manifest.Mode != "model" || res.Manifest.Model == nil || res.Manifest.Model.OllamaVersion != "0.35.2" ||
		!strings.HasPrefix(res.Manifest.Model.QuestionSetHash, "sha256:") || !strings.HasPrefix(res.Manifest.Input.Hash, "sha256:") {
		t.Errorf("manifest = %+v", res.Manifest)
	}

	// Offsets round-trip against the original file; stats outrank prose.
	recs := readRecords(t, dir)
	for _, r := range recs {
		s, e := int(r["start"].(float64)), int(r["end"].(float64))
		if sampleDoc[s:e] != r["text"].(string) {
			t.Errorf("offset invariant broken for %v", r["id"])
		}
		if !strings.HasPrefix(r["hash"].(string), "sha256:") || r["scored_by"] != "model" {
			t.Errorf("bad record: %v", r)
		}
	}
	if recs[0]["rank"].(float64) != 1 || recs[1]["rank"].(float64) <= recs[2]["rank"].(float64) {
		t.Errorf("expected stat segments to outrank prose: ranks %v %v %v", recs[0]["rank"], recs[1]["rank"], recs[2]["rank"])
	}
	if recs[3]["rank"] != nil || recs[3]["duplicate_of"] != recs[1]["id"] {
		t.Errorf("duplicate not flagged: %v", recs[3])
	}

	var top TopFile
	b, _ := os.ReadFile(filepath.Join(dir, "out", "top.json"))
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	if top.SchemaVersion != TopSchemaVersion || top.Count != 3 || len(top.Items) != 3 || top.Items[0].Rank != 1 || top.Items[0].Model == nil || *top.Items[0].Kind != "stat" {
		t.Errorf("top.json = %+v", top)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "run.json")); err != nil {
		t.Error("run.json missing")
	}

	// Second run: everything from cache, identical top.json.
	atomic.StoreInt32(&f.calls, 0)
	res2, err := Run(context.Background(), opts(t, f, dir, in))
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&f.calls) != 0 || res2.Manifest.Counts.CacheHits != 5 {
		t.Errorf("rerun: calls=%d hits=%d", f.calls, res2.Manifest.Counts.CacheHits)
	}
	b2, _ := os.ReadFile(filepath.Join(dir, "out", "top.json"))
	if string(b) != string(b2) {
		t.Error("top.json should be byte-identical across cached reruns")
	}

	// Edit one paragraph: only that segment hits the model.
	atomic.StoreInt32(&f.calls, 0)
	in2 := writeInput(t, dir, strings.Replace(sampleDoc, "Ohio", "Texas", 1))
	if _, err := Run(context.Background(), opts(t, f, dir, in2)); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&f.calls) != 1 {
		t.Errorf("after one edit, model calls = %d, want 1", f.calls)
	}

	// --no-cache bypasses the cache.
	atomic.StoreInt32(&f.calls, 0)
	o := opts(t, f, dir, in)
	o.NoCache = true
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&f.calls) != 5 {
		t.Errorf("--no-cache calls = %d, want 5", f.calls)
	}

	// Changing the model invalidates the cache.
	atomic.StoreInt32(&f.calls, 0)
	f.models = append(f.models, "tev1:4b")
	o = opts(t, f, dir, in)
	o.Config.Model = "tev1:4b"
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&f.calls) != 4 {
		t.Errorf("new model calls = %d, want 4", f.calls)
	}
}

func TestRunNoModel(t *testing.T) {
	dir := t.TempDir()
	in := writeInput(t, dir, sampleDoc)
	o := opts(t, nil, dir, in)
	o.NoModel = true
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Mode != "no-model" || res.Manifest.Model != nil {
		t.Errorf("manifest = %+v", res.Manifest)
	}
	for _, r := range readRecords(t, dir) {
		if r["scored_by"] != "features" || r["answers"] != nil || r["needs_review"] != false {
			t.Errorf("record = %v", r)
		}
	}
	if _, err := os.Stat(o.CacheDir); err == nil {
		t.Error("no-model mode must not touch the cache")
	}
	var top TopFile
	b, _ := os.ReadFile(filepath.Join(dir, "out", "top.json"))
	json.Unmarshal(b, &top)
	if top.Mode != "no-model" || top.Model != nil || top.Items[0].Model != nil || top.Items[0].Kind != nil {
		t.Errorf("top.json must mark the absence of the model: %+v", top)
	}
	// The revenue sentence is the most data-rich and must rank first.
	if !strings.HasPrefix(top.Items[0].Text, "Revenue grew") {
		t.Errorf("top item = %q", top.Items[0].Text)
	}
}

func TestRunPreflightFailures(t *testing.T) {
	dir := t.TempDir()
	in := writeInput(t, dir, sampleDoc)
	f := newFake(t)
	f.version = "0.34.0"
	if _, err := Run(context.Background(), opts(t, f, dir, in)); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Errorf("old version: err = %v", err)
	}
	f.version, f.models = "0.35.0", []string{"other:1b"}
	if _, err := Run(context.Background(), opts(t, f, dir, in)); err == nil || !strings.Contains(err.Error(), "ollama pull tev1:0.8b") {
		t.Errorf("missing model: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); err == nil {
		t.Error("no outputs should be written when preflight fails")
	}
	// --skip-preflight proceeds (the fake still serves /v1/systemone).
	o := opts(t, f, dir, in)
	o.SkipPreflight = true
	if _, err := Run(context.Background(), o); err != nil {
		t.Errorf("skip-preflight: %v", err)
	}
}

func TestRunFallbackOnSegmentError(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	doc := sampleDoc + "\nThis one will FAIL on the server with 3 numbers 1 2 3.\n"
	res, err := Run(context.Background(), opts(t, f, dir, writeInput(t, dir, doc)))
	if err != nil {
		t.Fatal(err)
	}
	if res.ModelErrors != 1 || res.Manifest.Counts.ModelErrors != 1 {
		t.Fatalf("model errors = %d", res.ModelErrors)
	}
	last := readRecords(t, dir)[5]
	if last["scored_by"] != "features_fallback" || last["needs_review"] != true || last["model_error"] == nil {
		t.Errorf("fallback record = %v", last)
	}
}

func TestRunAbortsWhenModelNeverWorks(t *testing.T) {
	f := newFake(t)
	f.failAll = true
	dir := t.TempDir()
	doc := strings.Repeat("Unique paragraph number one two three.\n\n", 1)
	for i := 0; i < 12; i++ {
		doc += "Paragraph " + strings.Repeat("x", i+1) + " is here.\n\n"
	}
	_, err := Run(context.Background(), opts(t, f, dir, writeInput(t, dir, doc)))
	if err == nil || !strings.Contains(err.Error(), "all failed") {
		t.Fatalf("err = %v", err)
	}
	if got := atomic.LoadInt32(&f.calls); got > 8 {
		t.Errorf("kept calling the model after repeated failure: %d calls", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "top.json")); err == nil {
		t.Error("aborted run must not write outputs")
	}
}

func TestRunCancel(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, opts(t, f, dir, writeInput(t, dir, sampleDoc)))
	if err == nil || !IsCanceled(err) {
		t.Fatalf("err = %v, want cancellation", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "out", "top.json")); statErr == nil {
		t.Error("cancelled run must not write outputs")
	}
}

func TestRunEmptyAndMissingInput(t *testing.T) {
	dir := t.TempDir()
	o := opts(t, nil, dir, writeInput(t, dir, ""))
	o.NoModel = true
	res, err := Run(context.Background(), o)
	if err != nil || len(res.Records) != 0 {
		t.Fatalf("empty input: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "out", "top.json"))
	if !strings.Contains(string(b), `"items": []`) {
		t.Errorf("empty items must serialise as []: %s", b)
	}
	o.InputPath = filepath.Join(dir, "nope.txt")
	if _, err := Run(context.Background(), o); err == nil {
		t.Error("missing input accepted")
	}
}

func TestStateTruncationAndConcurrency(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	o := opts(t, f, dir, writeInput(t, dir, "Short 12.\n\n"+strings.Repeat("日本語 ", 300)+"\n\nAnother 34 fact.\n"))
	o.Config.State.MaxTextBytes = 100
	o.Config.Concurrency = 3
	o.Config.Segmenter.MaxParagraphBytes, o.Config.Segmenter.HardMaxBytes = 0, 0
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	recs := readRecords(t, dir)
	if recs[0]["state_truncated"] != nil || recs[1]["state_truncated"] != true {
		t.Errorf("truncation flags wrong: %v %v", recs[0]["state_truncated"], recs[1]["state_truncated"])
	}
}
