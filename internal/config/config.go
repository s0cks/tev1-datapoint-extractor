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

	"github.com/s0cks/tev1-rank/internal/segment"
	"github.com/s0cks/tev1-rank/internal/tev1"
)

//go:embed default.json
var defaultJSON []byte

// DefaultJSON returns the embedded default configuration (for `--print-config`).
func DefaultJSON() []byte { return append([]byte(nil), defaultJSON...) }

// Question names with a fixed meaning in the pipeline.
const (
	QIsDatapoint = "is_datapoint"
	QKind        = "kind"
	QImportance  = "importance"
)

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
	UnknownProb   float64 `json:"unknown_prob"`   // flag when P(unknown) >= this
	KindMargin    float64 `json:"kind_margin"`    // flag when top-two kind probabilities are closer than this
	MinConfidence float64 `json:"min_confidence"` // flag when model-reported confidence is below this
}

type SelectionConfig struct {
	Top        int     `json:"top"`
	MinScore   float64 `json:"min_score"`
	MaxPerKind int     `json:"max_per_kind"`
}

type EvalConfig struct {
	DatapointThreshold float64 `json:"datapoint_threshold"`
	K                  []int   `json:"k"`
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
func (c Config) QuestionSet() tev1.QuestionSet { return tev1.QuestionSet(c.Questions) }

// Validate checks that the three required questions exist with usable types.
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
	dp, ok := c.Question(QIsDatapoint)
	if !ok {
		return fmt.Errorf("config: required question %q is missing", QIsDatapoint)
	}
	switch dp.Type {
	case tev1.TypeNoul:
	case tev1.TypeChoice:
		opts := dp.Options()
		if !has(opts, "yes") || !has(opts, "no") {
			return fmt.Errorf("config: choice question %q needs options \"yes\" and \"no\" (optionally \"unknown\")", QIsDatapoint)
		}
	default:
		return fmt.Errorf("config: %q must be type noul or choice, not %q", QIsDatapoint, dp.Type)
	}
	k, ok := c.Question(QKind)
	if !ok || k.Type != tev1.TypeChoice || len(k.Options()) < 2 {
		return fmt.Errorf("config: required question %q must be a choice question with 2+ options", QKind)
	}
	imp, ok := c.Question(QImportance)
	if !ok || imp.Type != tev1.TypeScore || imp.Levels() < 2 {
		return fmt.Errorf("config: required question %q must be a score question with 2+ levels", QImportance)
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
	return fmt.Sprintf("state/v1;features=%t;max_text_bytes=%d", c.State.IncludeFeatures, c.State.MaxTextBytes)
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
