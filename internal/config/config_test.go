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
	if n := len(kind.Options()); n != 10 {
		t.Errorf("kind options = %d, want 10", n)
	}
	if !has(kind.Options(), KindOther) {
		t.Errorf("kind needs the %q option", KindOther)
	}
	if _, ok := c.Question("is_datapoint"); ok {
		t.Error("is_datapoint was replaced by kind = other and must not be a default question")
	}
	// Every block type the card schema can derive from prose is a kind.
	for _, want := range []string{"stats", "chart", "timeline", "quote", "findings", "threads", "callout", "profile", "sources"} {
		if !has(kind.Options(), want) {
			t.Errorf("kind option %q missing", want)
		}
	}
	if !has(c.Selection.ExcludeKinds, KindOther) {
		t.Error("other must be excluded from selection by default")
	}
}

func TestOverrides(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "c.json")
		os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	const imp = `{"name":"importance","type":"score","instructions":"x","criteria":["lo","hi"]}`
	const kind = `{"name":"kind","type":"choice","instructions":"x","criteria":{"a":null,"other":null}}`
	tests := []struct {
		name, json, wantErr string
	}{
		{"model and weight merge", `{"model":"tev1:4b","ranking":{"kind_weights":{"stats":0.5,"extra":0.1}}}`, ""},
		{"unknown field rejected", `{"modle":"x"}`, "unknown field"},
		{"removed field rejected", `{"ranking":{"review":{"unknown_prob":0.3}}}`, "unknown field"},
		{"missing kind", `{"questions":[` + imp + `]}`, "kind"},
		{"missing importance", `{"questions":[` + kind + `]}`, "importance"},
		{"kind needs other", `{"questions":[{"name":"kind","type":"choice","instructions":"x","criteria":{"a":null,"b":null}},` + imp + `]}`, "other"},
		{"kind must be a choice", `{"questions":[{"name":"kind","type":"noul","instructions":"x"},` + imp + `]}`, "choice"},
		{"importance must be a score", `{"questions":[` + kind + `,{"name":"importance","type":"noul","instructions":"x"}]}`, "score"},
		{"minimal valid", `{"questions":[` + kind + `,` + imp + `]}`, ""},
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
	c, _ := Load(write(`{"ranking":{"kind_weights":{"stats":0.5}}}`))
	if c.Ranking.KindWeights["stats"] != 0.5 || c.Ranking.KindWeights["findings"] != 0.8 {
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
	  {"name":"kind","type":"choice","instructions":"x","criteria":{"figures":"s","dates":"t","other":"o"}},
	  {"name":"importance","type":"score","instructions":"x","criteria":["a","b"]}]}`), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(c.Warnings(), "\n")
	if !strings.Contains(w, "figures") || !strings.Contains(w, "dates") || !strings.Contains(w, "default_kind_weight") {
		t.Errorf("missing-weight warning absent:\n%s", w)
	}
	if !strings.Contains(w, "stats") || !strings.Contains(w, "timeline") || !strings.Contains(w, "ignored") {
		t.Errorf("stale-weight warning absent:\n%s", w)
	}
	// Supplying matching weights silences the missing-weight warning.
	os.WriteFile(p, []byte(`{"questions":[
	  {"name":"kind","type":"choice","instructions":"x","criteria":{"figures":"s","other":"o"}},
	  {"name":"importance","type":"score","instructions":"x","criteria":["a","b"]}],
	  "ranking":{"kind_weights":{"figures":1,"other":0}}}`), 0o644)
	c, _ = Load(p)
	for _, x := range c.Warnings() {
		if strings.Contains(x, "no ranking.kind_weights") {
			t.Errorf("unexpected missing-weight warning: %s", x)
		}
	}
}

// Questions other than kind and importance are never sent, so say so.
func TestWarningForUnusedQuestion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"questions":[
	  {"name":"is_datapoint","type":"noul","instructions":"x"},
	  {"name":"kind","type":"choice","instructions":"x","criteria":{"stats":"s","other":"o"}},
	  {"name":"importance","type":"score","instructions":"x","criteria":["a","b"]}]}`), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(c.Warnings(), "\n"); !strings.Contains(w, `"is_datapoint" is not used`) {
		t.Errorf("warnings:\n%s", w)
	}
}

func TestConfidenceQuestionsMustExist(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"ranking":{"review":{"confidence_questions":["nope"]}}}`), 0o644)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}

// The segmenter and text-normalisation settings are gone; a config file that
// still has them must fail loudly rather than be silently half-applied.
func TestRemovedSettingsAreRejected(t *testing.T) {
	for name, body := range map[string]string{
		"segmenter":      `{"segmenter":{"max_paragraph_bytes":600}}`,
		"normalize_text": `{"state":{"normalize_text":true}}`,
	} {
		p := filepath.Join(t.TempDir(), "c.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("%s: err = %v, want unknown field", name, err)
		}
	}
}

// feature_weights keys from the old regex features merge into the defaults but
// mean nothing now; that must be reported.
func TestOldFeatureWeightsWarn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(`{"ranking":{"feature_weights":{"years":0.3,"term_weight":0.1}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range c.Warnings() {
		found = found || (strings.Contains(w, "feature_weights") && strings.Contains(w, "term_weight") && strings.Contains(w, "years"))
	}
	if !found {
		t.Errorf("no warning for stale feature weights: %v", c.Warnings())
	}
	d, _ := Load("")
	for _, w := range d.Warnings() {
		if strings.Contains(w, "feature_weights") {
			t.Errorf("defaults warn about feature weights: %s", w)
		}
	}
}
