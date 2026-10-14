package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAreValid(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "tev1:0.8b" || c.Concurrency != 1 {
		t.Errorf("unexpected defaults: %+v", c.Model)
	}
	kind, _ := c.Question(QKind)
	for _, opt := range kind.Options() {
		if _, ok := c.Ranking.KindWeights[opt]; !ok {
			t.Errorf("default kind %q has no weight", opt)
		}
	}
	if n := len(kind.Options()); n != 9 {
		t.Errorf("kind options = %d, want 9", n)
	}
}

func TestOverrides(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "c.json")
		os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	tests := []struct {
		name, json, wantErr string
	}{
		{"model and weight merge", `{"model":"tev1:4b","ranking":{"kind_weights":{"stat":0.5,"extra":0.1}}}`, ""},
		{"unknown field rejected", `{"modle":"x"}`, "unknown field"},
		{"missing required question", `{"questions":[{"name":"kind","type":"choice","instructions":"x","criteria":{"a":null,"b":null}}]}`, "is_datapoint"},
		{"bad datapoint options", `{"questions":[{"name":"is_datapoint","type":"choice","instructions":"x","criteria":{"a":"1","b":"2"}},{"name":"kind","type":"choice","instructions":"x","criteria":{"a":null,"b":null}},{"name":"importance","type":"score","instructions":"x","criteria":["lo","hi"]}]}`, "yes"},
		{"noul datapoint ok", `{"questions":[{"name":"is_datapoint","type":"noul","instructions":"x"},{"name":"kind","type":"choice","instructions":"x","criteria":{"a":null,"b":null}},{"name":"importance","type":"score","instructions":"x","criteria":["lo","hi"]}]}`, ""},
		{"zero concurrency", `{"concurrency":0}`, "concurrency"},
		{"all-zero weights", `{"ranking":{"weights":{"datapoint":0,"importance":0,"kind":0,"features":0}}}`, "weights"},
		{"not json", `{`, "parsing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(write(tt.json))
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
	c, _ := Load(write(`{"ranking":{"kind_weights":{"stat":0.5}}}`))
	if c.Ranking.KindWeights["stat"] != 0.5 || c.Ranking.KindWeights["quote"] != 0.5 {
		t.Errorf("kind_weights should merge with defaults: %v", c.Ranking.KindWeights)
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestQuestionSetHash(t *testing.T) {
	a, _ := Load("")
	b, _ := Load("")
	if a.QuestionSetHash() != b.QuestionSetHash() || !strings.HasPrefix(a.QuestionSetHash(), "sha256:") {
		t.Fatal("hash must be deterministic and algorithm-prefixed")
	}
	b.Model = "tev1:4b"
	if a.QuestionSetHash() == b.QuestionSetHash() {
		t.Error("model change must change the hash")
	}
	c, _ := Load("")
	c.Questions[0].Instructions += "!"
	if a.QuestionSetHash() == c.QuestionSetHash() {
		t.Error("question change must change the hash")
	}
	d, _ := Load("")
	d.State.IncludeFeatures = false
	if a.QuestionSetHash() == d.QuestionSetHash() {
		t.Error("state format change must change the hash")
	}
	// Non-question settings must not invalidate caches.
	e, _ := Load("")
	e.Concurrency, e.Retries = 4, 9
	e.Ranking.Weights.Datapoint = 9
	if a.QuestionSetHash() != e.QuestionSetHash() {
		t.Error("ranking/concurrency changes must not change the hash")
	}
}

func TestWarningsForMismatchedKindWeights(t *testing.T) {
	c, _ := Load("")
	if w := c.Warnings(); len(w) != 0 {
		t.Fatalf("defaults must be warning-free: %v", w)
	}
	// What happens when a user replaces the kind options but not kind_weights:
	// the embedded default weights are merged in, and nothing matches.
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	os.WriteFile(p, []byte(`{"questions":[
	  {"name":"is_datapoint","type":"noul","instructions":"x"},
	  {"name":"kind","type":"choice","instructions":"x","criteria":{"stats":"s","timeline":"t","other":"o"}},
	  {"name":"importance","type":"score","instructions":"x","criteria":["a","b"]}]}`), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(c.Warnings(), "\n")
	if !strings.Contains(w, "stats") || !strings.Contains(w, "timeline") || !strings.Contains(w, "default_kind_weight") {
		t.Errorf("missing-weight warning absent:\n%s", w)
	}
	if !strings.Contains(w, "stat") || !strings.Contains(w, "timeline_event") || !strings.Contains(w, "ignored") {
		t.Errorf("stale-weight warning absent:\n%s", w)
	}
	// Supplying matching weights silences both.
	os.WriteFile(p, []byte(`{"questions":[
	  {"name":"is_datapoint","type":"noul","instructions":"x"},
	  {"name":"kind","type":"choice","instructions":"x","criteria":{"stats":"s","other":"o"}},
	  {"name":"importance","type":"score","instructions":"x","criteria":["a","b"]}],
	  "ranking":{"kind_weights":{"stats":1,"other":0.1}}}`), 0o644)
	c, _ = Load(p)
	if w := c.Warnings(); len(w) == 0 || strings.Contains(strings.Join(w, ""), "no ranking.kind_weights") {
		// stale default keys (quote, claim, ...) still warn; missing ones must not
		t.Logf("warnings: %v", w)
	}
	for _, x := range c.Warnings() {
		if strings.Contains(x, "no ranking.kind_weights") {
			t.Errorf("unexpected missing-weight warning: %s", x)
		}
	}
}

func TestConfidenceQuestionsMustExist(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"ranking":{"review":{"confidence_questions":["nope"]}}}`), 0o644)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}
