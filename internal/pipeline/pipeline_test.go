package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
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

// fakeOllama serves just enough of Ollama for the pipeline. Segments containing
// a digit look like stats (kind "stats", then an importance request); all other
// segments are "other" and must never get an importance request. Segments
// containing FAIL get a 500. Every request must carry exactly one question.
type fakeOllama struct {
	*httptest.Server
	calls      int32 // all /v1/systemone requests
	kindCalls  int32
	impCalls   int32
	version    string
	models     []string
	failAll    bool
	tokens     int // input_tokens to report per request (0 = omit usage)
	badRequest atomic.Value
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
					Kind    string `json:"kind"`
				} `json:"state"`
				Questions map[string]json.RawMessage `json:"questions"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if f.failAll || strings.Contains(req.State.Segment, "FAIL") {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			if len(req.Questions) != 1 {
				f.badRequest.Store("request must carry exactly one question")
			}
			stat := strings.ContainsAny(req.State.Segment, "0123456789")
			answers := map[string]any{}
			if _, ok := req.Questions["kind"]; ok {
				atomic.AddInt32(&f.kindCalls, 1)
				if req.State.Kind != "" {
					f.badRequest.Store("the kind request must not carry a kind")
				}
				if stat {
					answers["kind"] = map[string]any{"choice": "stats", "probabilities": map[string]float64{"stats": 0.85, "other": 0.15}, "confidence": 0.8}
				} else {
					answers["kind"] = map[string]any{"choice": "other", "probabilities": map[string]float64{"other": 0.9, "stats": 0.1}, "confidence": 0.8}
				}
			}
			if _, ok := req.Questions["importance"]; ok {
				atomic.AddInt32(&f.impCalls, 1)
				if req.State.Kind != "stats" || !stat {
					f.badRequest.Store("importance requested for a non-datapoint, or without its kind")
				}
				answers["importance"] = map[string]any{"score": 3.6, "confidence": 0.7}
			}
			resp := map[string]any{"model": "tev1:0.8b", "answers": answers}
			if f.tokens > 0 {
				resp["usage"] = map[string]int{"input_tokens": f.tokens, "output_tokens": 4}
			}
			json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// checkFake fails the test if the fake saw a request that breaks the two-request protocol.
func (f *fakeOllama) check(t *testing.T) {
	t.Helper()
	if v := f.badRequest.Load(); v != nil {
		t.Errorf("fake ollama: %v", v)
	}
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
	// Of the 4, two are stats (kind + importance = 2 requests each) and two are
	// "other" (kind only = 1 request each): 6 requests, 2 importance requests skipped.
	if got := atomic.LoadInt32(&f.calls); got != 6 {
		t.Errorf("model calls = %d, want 6", got)
	}
	if k, i := atomic.LoadInt32(&f.kindCalls), atomic.LoadInt32(&f.impCalls); k != 4 || i != 2 {
		t.Errorf("kind requests = %d (want 4), importance requests = %d (want 2)", k, i)
	}
	f.check(t)
	c := res.Manifest.Counts
	if c.Segments != 5 || c.Duplicates != 1 || c.Ranked != 4 || c.Selected != 3 || c.CacheHits != 1 || c.ModelCalls != 6 || c.ImportanceSkipped != 2 {
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
	// recs[0] and recs[2] are stats, recs[1] is prose ("other"): both stats must outrank it,
	// whatever the feature weights say about their order.
	if r0, r1, r2 := recs[0]["rank"].(float64), recs[1]["rank"].(float64), recs[2]["rank"].(float64); r1 <= r0 || r1 <= r2 {
		t.Errorf("expected stat segments to outrank prose: ranks %v %v %v", r0, r1, r2)
	}
	// "other" segments carry only the kind answer; datapoints carry both.
	if ans := recs[1]["answers"].(map[string]any); ans["kind"] == nil || ans["importance"] != nil {
		t.Errorf("other segment answers = %v", ans)
	}
	if ans := recs[0]["answers"].(map[string]any); ans["kind"] == nil || ans["importance"] == nil {
		t.Errorf("datapoint answers = %v", ans)
	}
	if recs[3]["rank"] != nil || recs[3]["duplicate_of"] != recs[1]["id"] {
		t.Errorf("duplicate not flagged: %v", recs[3])
	}

	var top TopFile
	b, _ := os.ReadFile(filepath.Join(dir, "out", "top.json"))
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	if top.SchemaVersion != TopSchemaVersion || top.Count != 3 || len(top.Items) != 3 || top.Items[0].Rank != 1 || top.Items[0].Model == nil || *top.Items[0].Kind != "stats" {
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

	// Edit one stats paragraph: only that segment hits the model (kind + importance).
	atomic.StoreInt32(&f.calls, 0)
	in2 := writeInput(t, dir, strings.Replace(sampleDoc, "Ohio", "Texas", 1))
	if _, err := Run(context.Background(), opts(t, f, dir, in2)); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&f.calls) != 2 {
		t.Errorf("after one edit, model calls = %d, want 2", f.calls)
	}

	// --no-cache bypasses the cache.
	atomic.StoreInt32(&f.calls, 0)
	o := opts(t, f, dir, in)
	o.NoCache = true
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	// 5 segments (the repeat is no longer a cache hit): 2 stats x 2 requests + 3 other x 1 request.
	if atomic.LoadInt32(&f.calls) != 7 {
		t.Errorf("--no-cache calls = %d, want 7", f.calls)
	}

	// Changing the model invalidates the cache.
	atomic.StoreInt32(&f.calls, 0)
	f.models = append(f.models, "tev1:4b")
	o = opts(t, f, dir, in)
	o.Config.Model = "tev1:4b"
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&f.calls) != 6 {
		t.Errorf("new model calls = %d, want 6", f.calls)
	}
	f.check(t)
}

// Segments the model classifies as "other" are cached too (kind answer only), so
// a rerun does not ask about them again, and they are not selected.
func TestOtherIsCachedAndNotSelected(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	in := writeInput(t, dir, sampleDoc)
	o := opts(t, f, dir, in)
	o.Select = rank.SelectOptions{Top: 10, ExcludeKinds: []string{"other"}}
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Counts.Selected != 2 {
		t.Errorf("selected = %d, want only the 2 stats segments", res.Manifest.Counts.Selected)
	}
	for _, i := range res.Selected {
		if res.Records[i].Kind() == "other" {
			t.Errorf("other segment selected: %q", res.Records[i].Text)
		}
	}
	atomic.StoreInt32(&f.calls, 0)
	res2, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 || res2.Manifest.Counts.CacheHits != 5 {
		t.Errorf("rerun: calls=%d hits=%d (other segments must be cached)", f.calls, res2.Manifest.Counts.CacheHits)
	}
	// No exclusion: the other segments are ranked below the stats ones.
	o.Select = rank.SelectOptions{Top: 10}
	res3, _ := Run(context.Background(), o)
	if res3.Manifest.Counts.Selected != 4 {
		t.Errorf("without exclude_kinds selected = %d, want 4", res3.Manifest.Counts.Selected)
	}
}

// A failure of the second request falls back for that segment (kind alone is
// not cached as if it were complete).
func TestImportanceRequestFailureFallsBack(t *testing.T) {
	var kindOnly int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req.Questions["importance"]; ok {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		atomic.AddInt32(&kindOnly, 1)
		json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{
			"kind": map[string]any{"choice": "stats", "probabilities": map[string]float64{"stats": 1}}}})
	}))
	defer srv.Close()
	dir := t.TempDir()
	o := opts(t, nil, dir, writeInput(t, dir, "Revenue grew 14% in 2024.\n"))
	o.Config.OllamaURL = srv.URL
	o.Client = &tev1.Client{BaseURL: srv.URL, Timeout: 5 * time.Second}
	o.SkipPreflight = true
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.ModelErrors != 1 {
		t.Fatalf("model errors = %d", res.ModelErrors)
	}
	if r := readRecords(t, dir)[0]; r["scored_by"] != "features_fallback" || r["needs_review"] != true {
		t.Errorf("record = %v", r)
	}
	// Rerun: nothing was cached from the half-finished segment.
	before := atomic.LoadInt32(&kindOnly)
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&kindOnly) == before {
		t.Error("a segment with only its kind answer must not be served from the cache")
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
	// A data-rich sentence (not the prose) must rank first; which of the two depends on feature_weights.
	if !strings.ContainsAny(top.Items[0].Text, "0123456789") {
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

// The model sees normalised text; the record keeps the original bytes.
func TestStateIsNormalisedButRecordIsVerbatim(t *testing.T) {
	var got struct{ Seg, Feat string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/systemone":
			var req struct {
				State struct{ Segment, Features string } `json:"state"`
			}
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &req)
			if got.Seg == "" { // the kind request is the first one
				got.Seg, got.Feat = req.State.Segment, req.State.Features
			}
			var q struct {
				Questions map[string]json.RawMessage `json:"questions"`
			}
			json.Unmarshal(body, &q)
			answers := map[string]any{}
			if _, ok := q.Questions["kind"]; ok {
				answers["kind"] = map[string]any{"choice": "stats", "probabilities": map[string]float64{"stats": 1}}
			}
			if _, ok := q.Questions["importance"]; ok {
				answers["importance"] = map[string]any{"score": 2.0}
			}
			json.NewEncoder(w).Encode(map[string]any{"answers": answers})
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	raw := "In 1960, the Agency\u202f(\u200dCIA\u200d) Station Chief\nreceived a cable in the Congo ."
	o := opts(t, nil, dir, writeInput(t, dir, raw+"\n"))
	o.Config.OllamaURL = srv.URL
	o.Client = &tev1.Client{BaseURL: srv.URL, Timeout: 5 * time.Second}
	o.SkipPreflight = true
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if want := "In 1960, the Agency (CIA) Station Chief received a cable in the Congo."; got.Seg != want {
		t.Errorf("model saw %q, want %q", got.Seg, want)
	}
	recs := readRecords(t, dir)
	if recs[0]["text"] != raw {
		t.Errorf("record text altered: %q", recs[0]["text"])
	}
	// turning normalisation off sends the raw text and changes the cache key
	o2 := opts(t, nil, t.TempDir(), writeInput(t, dir, raw+"\n"))
	o2.Config.OllamaURL = srv.URL
	o2.Config.State.NormalizeText = false
	if o2.Config.QuestionSetHash() == o.Config.QuestionSetHash() {
		t.Error("normalize_text must be part of the question set hash")
	}
}

func TestContextExceededIsRecordedFlaggedAndCached(t *testing.T) {
	f := newFake(t)
	f.tokens = 2166 // above the default 2048 window
	dir := t.TempDir()
	in := writeInput(t, dir, sampleDoc)
	res, err := Run(context.Background(), opts(t, f, dir, in))
	if err != nil {
		t.Fatal(err)
	}
	// 6 requests of 2166 tokens each in total.
	if res.Manifest.Counts.ContextExceeded == 0 || res.Manifest.Counts.InputTokens != 2166*6 {
		t.Errorf("counts = %+v", res.Manifest.Counts)
	}
	for _, r := range readRecords(t, dir) {
		// input_tokens sums the requests of a segment; the window check uses the largest single one.
		wantTotal := 2166.0 * 2 // stats: kind + importance
		if _, hasImp := r["answers"].(map[string]any)["importance"]; !hasImp {
			wantTotal = 2166
		}
		if r["context_exceeded"] != true || r["input_tokens"].(float64) != wantTotal || r["peak_input_tokens"].(float64) != 2166 || r["needs_review"] != true {
			t.Errorf("record not flagged: %v", r)
		}
		if !strings.Contains(strings.Join(toStrings(r["review_reasons"]), ","), "context_window_exceeded") {
			t.Errorf("reason missing: %v", r["review_reasons"])
		}
	}
	// A cached rerun must still know the original token count.
	atomic.StoreInt32(&f.calls, 0)
	if _, err := Run(context.Background(), opts(t, f, dir, in)); err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 {
		t.Fatalf("expected cache hits, got %d calls", f.calls)
	}
	for _, r := range readRecords(t, dir) {
		if r["context_exceeded"] != true || r["cached"] != true {
			t.Errorf("cached record lost the flag: %v", r)
		}
	}
	// Under the window: no flag.
	f.tokens = 1500
	dir2 := t.TempDir()
	if _, err := Run(context.Background(), opts(t, f, dir2, writeInput(t, dir2, sampleDoc))); err != nil {
		t.Fatal(err)
	}
	for _, r := range readRecords(t, dir2) {
		if r["context_exceeded"] != nil {
			t.Errorf("flagged under the window: %v", r)
		}
	}
}

func toStrings(v any) []string {
	var out []string
	if arr, ok := v.([]any); ok {
		for _, x := range arr {
			out = append(out, x.(string))
		}
	}
	return out
}
