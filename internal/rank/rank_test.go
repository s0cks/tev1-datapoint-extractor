package rank

import (
	"math"
	"reflect"
	"testing"

	"github.com/s0cks/tev1-rank/internal/config"
	"github.com/s0cks/tev1-rank/internal/features"
	"github.com/s0cks/tev1-rank/internal/tev1"
)

func f(v float64) *float64 { return &v }

func mustCfg(t *testing.T) config.Config {
	t.Helper()
	c, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// answers builds the merged answers of the two requests. A nil score means the
// importance request was skipped (kind = other).
func answers(kind string, kp map[string]float64, kindConf float64, score *float64, impConf float64) map[string]tev1.Answer {
	a := map[string]tev1.Answer{"kind": {Choice: kind, Probabilities: kp, Confidence: f(kindConf)}}
	if score != nil {
		a["importance"] = tev1.Answer{Score: score, Confidence: f(impConf)}
	}
	return a
}

func TestExtractSignals(t *testing.T) {
	cfg := mustCfg(t)
	s, err := ExtractSignals(cfg, answers("stats", map[string]float64{"stats": 0.8, "chart": 0.1, "other": 0.1}, 0.8, f(3), 0.5))
	if err != nil {
		t.Fatal(err)
	}
	near := func(name string, got, want float64) {
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	near("p_datapoint", s.PDatapoint, 0.9) // 1 - P(other)
	near("importance", s.Importance, 4)    // score 3 on a 0-based scale → 4 on 1..5
	near("importance_norm", s.ImportanceNorm, 0.75)
	near("kind_weight", s.KindWeight, 0.8*1.0+0.1*0.9+0.1*0.0)
	near("kind_margin", s.KindMargin, 0.7)
	if s.Kind != "stats" || !s.IsDatapoint() || s.Confidence == nil || *s.Confidence != 0.8 {
		t.Errorf("unexpected signals: %+v", s)
	}

	t.Run("other is not a datapoint and has no importance", func(t *testing.T) {
		s, err := ExtractSignals(cfg, answers("other", map[string]float64{"other": 0.7, "stats": 0.3}, 0.7, nil, 0))
		if err != nil {
			t.Fatal(err)
		}
		near("p_datapoint", s.PDatapoint, 0.3)
		if s.IsDatapoint() || s.Importance != 0 || s.ImportanceNorm != 0 {
			t.Errorf("%+v", s)
		}
	})
	t.Run("no probabilities falls back to one-hot", func(t *testing.T) {
		s, err := ExtractSignals(cfg, answers("quote", nil, 0.9, f(0), 0.9))
		if err != nil || s.PDatapoint != 1 || s.KindProbs["quote"] != 1 || s.KindMargin != 1 {
			t.Fatalf("%+v %v", s, err)
		}
		s, _ = ExtractSignals(cfg, answers("other", nil, 0.9, nil, 0))
		if s.PDatapoint != 0 {
			t.Errorf("one-hot other: p_datapoint = %v", s.PDatapoint)
		}
	})
	t.Run("unnormalised probabilities are rescaled", func(t *testing.T) {
		s, _ := ExtractSignals(cfg, answers("stats", map[string]float64{"stats": 0.3, "chart": 0.3}, 0.9, f(1), 0.9))
		if math.Abs(s.KindProbs["stats"]-0.5) > 1e-9 {
			t.Errorf("kind probs = %v", s.KindProbs)
		}
	})
	t.Run("unknown kind uses default weight", func(t *testing.T) {
		s, _ := ExtractSignals(cfg, answers("mystery", map[string]float64{"mystery": 1}, 0.9, f(1), 0.9))
		if s.KindWeight != cfg.Ranking.DefaultKindWeight {
			t.Errorf("kind weight = %v", s.KindWeight)
		}
	})
	t.Run("missing kind answer", func(t *testing.T) {
		if _, err := ExtractSignals(cfg, map[string]tev1.Answer{}); err == nil {
			t.Error("expected error")
		}
	})
}

func TestReview(t *testing.T) {
	cfg := config.ReviewConfig{KindMargin: 0.15, MinConfidence: 0.3}
	tests := []struct {
		name string
		s    Signals
		want []string
	}{
		{"confident", Signals{PDatapoint: 0.95, KindMargin: 0.6, Confidence: f(0.8)}, nil},
		{"datapoint coin flip", Signals{PDatapoint: 0.5, KindMargin: 0.6, Confidence: f(0.8)}, []string{"datapoint_uncertain"}},
		{"datapoint just outside the band", Signals{PDatapoint: 0.61, KindMargin: 0.6}, nil},
		{"kind close", Signals{PDatapoint: 0.95, KindMargin: 0.05, Confidence: f(0.8)}, []string{"kind_top_two_close"}},
		{"low confidence", Signals{PDatapoint: 0.95, KindMargin: 0.6, Confidence: f(0.1)}, []string{"low_confidence"}},
		{"no confidence reported", Signals{PDatapoint: 0.95, KindMargin: 0.6}, nil},
		{"everything", Signals{PDatapoint: 0.5, KindMargin: 0, Confidence: f(0.01)}, []string{"datapoint_uncertain", "kind_top_two_close", "low_confidence"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Review(cfg, tt.s); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Review = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFeatureBonus(t *testing.T) {
	cfg := mustCfg(t).Ranking
	tests := []struct {
		name string
		f    features.Features
		want func(float64) bool
	}{
		{"nothing", features.Features{}, func(b float64) bool { return b == 0 }},
		{"saturated", features.Features{Numbers: 9, Percentages: 9, Currency: 9, Years: 9, Dates: 9, Entities: 9, TermWeight: 1}, func(b float64) bool { return math.Abs(b-1) < 1e-9 }},
		{"one number only", features.Features{Numbers: 1}, func(b float64) bool { return b > 0 && b < 0.2 }},
		{"monotonic: more numbers → higher", features.Features{Numbers: 2}, func(b float64) bool { return b > FeatureBonus(cfg, features.Features{Numbers: 1}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if b := FeatureBonus(cfg, tt.f); !tt.want(b) {
				t.Errorf("bonus = %v", b)
			}
		})
	}
	if FeatureBonus(config.RankingConfig{}, features.Features{Numbers: 3}) != 0 {
		t.Error("no feature weights should give 0")
	}
}

func TestScore(t *testing.T) {
	cfg := mustCfg(t).Ranking
	best := Signals{PDatapoint: 1, ImportanceNorm: 1, KindWeight: 1}
	worst := Signals{}
	if s := Score(cfg, best, 1); math.Abs(s-1) > 1e-9 {
		t.Errorf("best = %v", s)
	}
	if s := Score(cfg, worst, 0); s != 0 {
		t.Errorf("worst = %v", s)
	}
	w := cfg.Weights
	mid := Signals{PDatapoint: 1} // only the datapoint term contributes
	want := w.Datapoint / (w.Datapoint + w.Importance + w.Kind + w.Features)
	if s := Score(cfg, mid, 0); math.Abs(s-want) > 1e-9 {
		t.Errorf("datapoint-only = %v, want %v", s, want)
	}
	// Weights live in config: zeroing everything but features makes bonus the score.
	only := cfg
	only.Weights = config.Weights{Features: 1}
	if s := Score(only, best, 0.3); math.Abs(s-0.3) > 1e-9 {
		t.Errorf("features-only = %v", s)
	}
}

func TestNormalizedHash(t *testing.T) {
	a := NormalizedHash("Revenue grew 14%,  to $2.3 billion!")
	b := NormalizedHash("revenue GREW 14 to 2 3 billion")
	if a != b {
		t.Error("case/punctuation/spacing should normalise away")
	}
	if a == NormalizedHash("Revenue grew 15% to $2.3 billion") {
		t.Error("different numbers must not collide")
	}
	if NormalizedHash("日本語。") != NormalizedHash("日本語") {
		t.Error("unicode letters should be kept and punctuation dropped")
	}
}

func TestRankAndSelect(t *testing.T) {
	items := []Item{
		{NormHash: "a", Score: 0.5, Kind: "stats"},    // 0
		{NormHash: "b", Score: 0.9, Kind: "stats"},    // 1
		{NormHash: "a", Score: 0.5, Kind: "stats"},    // 2 duplicate of 0
		{NormHash: "c", Score: 0.9, Kind: "stats"},    // 3 ties with 1 → document order
		{NormHash: "d", Score: 0.7, Kind: "timeline"}, // 4
		{NormHash: "e", Score: 0.1, Kind: "other"},    // 5
	}
	ranks, dup := Rank(items)
	if want := []int{4, 1, 0, 2, 3, 5}; !reflect.DeepEqual(ranks, want) {
		t.Errorf("ranks = %v, want %v", ranks, want)
	}
	if want := []int{-1, -1, 0, -1, -1, -1}; !reflect.DeepEqual(dup, want) {
		t.Errorf("dupOf = %v, want %v", dup, want)
	}
	tests := []struct {
		name string
		opt  SelectOptions
		want []int
	}{
		{"all", SelectOptions{}, []int{1, 3, 4, 0, 5}},
		{"top 2", SelectOptions{Top: 2}, []int{1, 3}},
		{"min score", SelectOptions{MinScore: 0.6}, []int{1, 3, 4}},
		{"max per kind skips and continues", SelectOptions{Top: 3, MaxPerKind: 1}, []int{1, 4, 5}},
		{"exclude other", SelectOptions{ExcludeKinds: []string{"other"}}, []int{1, 3, 4, 0}},
		{"excluded kinds do not use up top", SelectOptions{Top: 5, ExcludeKinds: []string{"stats"}}, []int{4, 5}},
		{"top larger than available", SelectOptions{Top: 99}, []int{1, 3, 4, 0, 5}},
		{"min score above all", SelectOptions{MinScore: 2}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Select(items, ranks, tt.opt); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Select = %v, want %v", got, tt.want)
			}
		})
	}
	t.Run("unclassified items are not capped or excluded", func(t *testing.T) {
		its := []Item{{NormHash: "x", Score: 0.9}, {NormHash: "y", Score: 0.8}, {NormHash: "z", Score: 0.7}}
		r, _ := Rank(its)
		if got := Select(its, r, SelectOptions{MaxPerKind: 1, ExcludeKinds: []string{"other"}}); len(got) != 3 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("empty", func(t *testing.T) {
		r, d := Rank(nil)
		if len(r) != 0 || len(d) != 0 || len(Select(nil, r, SelectOptions{Top: 5})) != 0 {
			t.Error("empty input should yield empty output")
		}
	})
}

// Regression: importance confidence is inherently low (flat ordinal distribution)
// and used to flag every segment as low_confidence.
func TestReviewIgnoresLowConfidenceOfOrdinalQuestions(t *testing.T) {
	cfg := mustCfg(t)
	a := answers("stats", map[string]float64{"stats": 0.9, "chart": 0.1}, 0.9, f(3), 0.1)
	s, err := ExtractSignals(cfg, a)
	if err != nil {
		t.Fatal(err)
	}
	if s.Confidence == nil || *s.Confidence != 0.9 {
		t.Fatalf("confidence = %v, want 0.9 (kind only)", s.Confidence)
	}
	if r := Review(cfg.Ranking.Review, s); len(r) != 0 {
		t.Errorf("review = %v, want none", r)
	}
	// Opting importance in restores the old behaviour.
	cfg.Ranking.Review.ConfidenceQuestions = []string{"kind", "importance"}
	s, _ = ExtractSignals(cfg, a)
	if s.Confidence == nil || *s.Confidence != 0.1 {
		t.Fatalf("confidence = %v, want 0.1", s.Confidence)
	}
	if r := Review(cfg.Ranking.Review, s); len(r) != 1 || r[0] != "low_confidence" {
		t.Errorf("review = %v", r)
	}
	// A diffuse kind answer (stats vs other) is still caught, by two flags.
	cfg = mustCfg(t)
	a = answers("stats", map[string]float64{"stats": 0.43, "other": 0.45, "chart": 0.12}, 0.1, f(3), 0.9)
	s, _ = ExtractSignals(cfg, a)
	if r := Review(cfg.Ranking.Review, s); len(r) != 3 {
		t.Errorf("review = %v, want datapoint_uncertain + kind_top_two_close + low_confidence", r)
	}
}
