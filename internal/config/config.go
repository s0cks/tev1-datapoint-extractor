// Package config holds the embedded default configuration and its overrides.
package config

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/s0cks/tev1-rank/internal/segment"
	"github.com/s0cks/tev1-rank/internal/tev1"
)

//go:embed default.json
var defaultJSON []byte

// DefaultJSON returns the embedded default configuration (for `--print-config`).
func DefaultJSON() []byte { return append([]byte(nil), defaultJSON...) }

// Question names with a fixed meaning in the pipeline. Each segment is
// classified in two separate requests: QKind first, then (only when the kind is
// not KindOther) QImportance. Any other question in the config is ignored.
const (
	QKind       = "kind"
	QImportance = "importance"
)

// KindOther is the kind option meaning "not a datapoint". A segment the model
// classifies as KindOther is not sent on to the importance request.
const KindOther = "other"

type Config struct {
	Model          string `json:"model"`
	OllamaURL      string `json:"ollama_url"`
	KeepAlive      string `json:"keep_alive"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Retries        int    `json:"retries"`
	BackoffMillis  int    `json:"backoff_ms"`
	Concurrency    int    `json:"concurrency"`

	Segmenter segment.Options      `json:"segmenter"`
	State     StateConfig          `json:"state"`
	Questions []tev1.NamedQuestion `json:"questions"`
	Ranking   RankingConfig        `json:"ranking"`
	Selection SelectionConfig      `json:"selection"`
	Eval      EvalConfig           `json:"eval"`
}

type StateConfig struct {
	IncludeFeatures bool `json:"include_features"`
	MaxTextBytes    int  `json:"max_text_bytes"`
	// NormalizeText collapses whitespace/line wraps and strips zero-width
	// characters in the text sent to the model. The reported segment text and
	// offsets are never changed.
	NormalizeText bool `json:"normalize_text"`
	// ContextTokens is the model's context window, used only to warn when a
	// request's reported input_tokens gets close to it. It does not affect
	// what is sent or the question_set_hash.
	ContextTokens int `json:"context_tokens"`
}

type RankingConfig struct {
	Weights           Weights            `json:"weights"`
	KindWeights       map[string]float64 `json:"kind_weights"`
	DefaultKindWeight float64            `json:"default_kind_weight"`
	FeatureWeights    map[string]float64 `json:"feature_weights"`
	FeatureSaturation float64            `json:"feature_saturation"`
	Review            ReviewConfig       `json:"review"`
}

type Weights struct {
	Datapoint  float64 `json:"datapoint"`
	Importance float64 `json:"importance"`
	Kind       float64 `json:"kind"`
	Features   float64 `json:"features"`
}

type ReviewConfig struct {
	KindMargin    float64 `json:"kind_margin"`    // flag when top-two kind probabilities are closer than this
	MinConfidence float64 `json:"min_confidence"` // flag when model-reported confidence is below this
	// ConfidenceQuestions lists the questions whose model-reported confidence
	// counts towards min_confidence. Ordinal questions (importance) report
	// inherently low confidence and would flag every segment, so by default only
	// kind is used.
	ConfidenceQuestions []string `json:"confidence_questions"`
}

type SelectionConfig struct {
	Top        int     `json:"top"`
	MinScore   float64 `json:"min_score"`
	MaxPerKind int     `json:"max_per_kind"`
	// ExcludeKinds are never selected. The default excludes "other", i.e. the
	// segments the model decided are not datapoints.
	ExcludeKinds []string `json:"exclude_kinds"`
}

type EvalConfig struct {
	K []int `json:"k"`
}

// Load returns the embedded defaults, overlaid with the JSON file at path if
// path is non-empty. Unknown fields are rejected so typos do not silently
// fall back to defaults. Objects merge key-by-key (e.g. kind_weights adds to
// the defaults); arrays (questions) are replaced wholesale.
func Load(path string) (Config, error) {
	var c Config
	if err := strictDecode(defaultJSON, &c); err != nil {
		return c, fmt.Errorf("embedded default config is invalid: %w", err)
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return c, fmt.Errorf("reading config: %w", err)
		}
		if err := strictDecode(b, &c); err != nil {
			return c, fmt.Errorf("parsing %s: %w", path, err)
		}
	}
	return c, c.Validate()
}

func strictDecode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Question returns the named question.
func (c Config) Question(name string) (tev1.NamedQuestion, bool) {
	for _, q := range c.Questions {
		if q.Name == name {
			return q, true
		}
	}
	return tev1.NamedQuestion{}, false
}

// QuestionSet returns the questions in request form.
func (c Config) QuestionSet() tev1.QuestionSet {
	return tev1.QuestionSet(c.Questions)
}

// Validate checks that the two required questions exist with usable types.
func (c Config) Validate() error {
	if c.Model == "" {
		return fmt.Errorf("config: model is empty")
	}
	if c.Concurrency < 1 {
		return fmt.Errorf("config: concurrency must be >= 1")
	}
	if len(c.Questions) > 64 {
		return fmt.Errorf("config: %d questions; the API allows at most 64", len(c.Questions))
	}
	seen := map[string]bool{}
	for _, q := range c.Questions {
		if q.Name == "" || seen[q.Name] {
			return fmt.Errorf("config: question names must be non-empty and unique (%q)", q.Name)
		}
		seen[q.Name] = true
	}
	k, ok := c.Question(QKind)
	if !ok || k.Type != tev1.TypeChoice {
		return fmt.Errorf("config: required question %q must be a choice question", QKind)
	}
	if opts := k.Options(); len(opts) < 2 || !has(opts, KindOther) {
		return fmt.Errorf("config: choice question %q needs 2+ options including %q (the \"not a datapoint\" option)", QKind, KindOther)
	}
	imp, ok := c.Question(QImportance)
	if !ok || imp.Type != tev1.TypeScore || imp.Levels() < 2 {
		return fmt.Errorf("config: required question %q must be a score question with 2+ levels", QImportance)
	}
	for _, name := range c.Ranking.Review.ConfidenceQuestions {
		if _, ok := c.Question(name); !ok {
			return fmt.Errorf("config: ranking.review.confidence_questions names unknown question %q", name)
		}
	}
	w := c.Ranking.Weights
	if w.Datapoint < 0 || w.Importance < 0 || w.Kind < 0 || w.Features < 0 || w.Datapoint+w.Importance+w.Kind+w.Features <= 0 {
		return fmt.Errorf("config: ranking weights must be non-negative and not all zero")
	}
	return nil
}

func has(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// StateFormat identifies the shape of the state sent to the model. It is part
// of the question-set hash so that changing it invalidates cached answers.
func (c Config) StateFormat() string {
	return fmt.Sprintf("state/v3;two-stage;features=%t;max_text_bytes=%d;normalize=%t", c.State.IncludeFeatures, c.State.MaxTextBytes, c.State.NormalizeText)
}

// QuestionSetHash is "sha256:<hex>" over the model name, the ordered questions
// and the state format. Any change to one of them changes the hash.
func (c Config) QuestionSetHash() string {
	b, _ := json.Marshal(struct {
		Model       string           `json:"model"`
		Questions   tev1.QuestionSet `json:"questions"`
		StateFormat string           `json:"state_format"`
	}{c.Model, c.QuestionSet(), c.StateFormat()})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Warnings returns non-fatal configuration problems worth showing the user.
func (c Config) Warnings() []string {
	var w []string
	for _, q := range c.Questions {
		if q.Name != QKind && q.Name != QImportance {
			w = append(w, fmt.Sprintf("question %q is not used: only %q and %q are sent (in separate requests)", q.Name, QKind, QImportance))
		}
	}
	kq, _ := c.Question(QKind)
	opts := kq.Options()
	var missing, stale []string
	for _, o := range opts {
		if _, ok := c.Ranking.KindWeights[o]; !ok {
			missing = append(missing, o)
		}
	}
	for k := range c.Ranking.KindWeights {
		if !has(opts, k) {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	if len(missing) > 0 {
		w = append(w, fmt.Sprintf("kind options %v have no ranking.kind_weights entry, so they all use default_kind_weight (%.2f) and the kind term cannot separate them", missing, c.Ranking.DefaultKindWeight))
	}
	if len(stale) > 0 {
		w = append(w, fmt.Sprintf("ranking.kind_weights entries %v match no kind option and are ignored (kind_weights are merged with the embedded defaults, not replaced)", stale))
	}
	return w
}
