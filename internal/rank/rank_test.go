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

func answers(pyes, punk float64, kind string, kp map[string]float64, score float64, conf float64) map[string]tev1.Answer {
	return map[string]tev1.Answer{
		"is_datapoint": {Choice: "yes", Probabilities: map[string]float64{"yes": pyes, "no": 1 - pyes - punk, "unknown": punk}, Confidence: f(conf)},
		"kind":         {Choice: kind, Probabilities: kp, Confidence: f(conf)},
		"importance":   {Score: f(score), Confidence: f(conf)},
	}
}

func TestExtractSignals(t *testing.T) {
	cfg := mustCfg(t)
	s, err := ExtractSignals(cfg, answers(0.9, 0.05, "stat", map[string]float64{"stat": 0.8, "claim": 0.2}, 3, 0.8))
	if err != nil {
		t.Fatal(err)
	}
	near := func(name string, got, want float64) {
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	near("p_datapoint", s.PDatapoint, 0.9)
	near("p_unknown", s.PUnknown, 0.05)
	near("importance", s.Importance, 4) // score 3 on a 0-based scale → 4 on 1..5
	near("importance_norm", s.ImportanceNorm, 0.75)
	near("kind_weight", s.KindWeight, 0.8*1.0+0.2*0.6)
	near("kind_margin", s.KindMargin, 0.6)
	if s.Kind != "stat" || s.Confidence == nil || *s.Confidence != 0.8 {
		t.Errorf("unexpected signals: %+v", s)
	}

	t.Run("noul datapoint", func(t *testing.T) {
		c := mustCfg(t)
		c.Questions[0] = tev1.NamedQuestion{Name: "is_datapoint", Type: tev1.TypeNoul, Instructions: "x"}
		a := answers(0, 0, "stat", nil, 4, 0.9)
		a["is_datapoint"] = tev1.Answer{Noul: f(0.45)}
		s, err := ExtractSignals(c, a)
		if err != nil || s.PDatapoint != 0.45 || s.PUnknown != 0 {
			t.Fatalf("%+v %v", s, err)
		}
		if r := Review(c.Ranking.Review, s); !reflect.DeepEqual(r, []string{"datapoint_uncertain"}) {
			t.Errorf("review = %v", r)
		}
	})
	t.Run("no probabilities falls back to one-hot", func(t *testing.T) {
		a := answers(0, 0, "quote", nil, 0, 0.9)
		a["is_datapoint"] = tev1.Answer{Choice: "yes"}
		s, err := ExtractSignals(cfg, a)
		if err != nil || s.PDatapoint != 1 || s.KindProbs["quote"] != 1 || s.KindMargin != 1 {
			t.Fatalf("%+v %v", s, err)
		}
	})
	t.Run("unnormalised probabilities are rescaled", func(t *testing.T) {
		s, _ := ExtractSignals(cfg, answers(0.5, 0, "stat", map[string]float64{"stat": 0.3, "claim": 0.3}, 1, 0.9))
		if math.Abs(s.KindProbs["stat"]-0.5) > 1e-9 {
			t.Errorf("kind probs = %v", s.KindProbs)
		}
	})
	t.Run("unknown kind uses default weight", func(t *testing.T) {
		s, _ := ExtractSignals(cfg, answers(0.5, 0, "mystery", map[string]float64{"mystery": 1}, 1, 0.9))
		if s.KindWeight != cfg.Ranking.DefaultKindWeight {
			t.Errorf("kind weight = %v", s.KindWeight)
		}
	})
	t.Run("missing answers", func(t *testing.T) {
		if _, err := ExtractSignals(cfg, map[string]tev1.Answer{}); err == nil {
			t.Error("expected error")
		}
	})
}

func TestReview(t *testing.T) {
	cfg := mustCfg(t).Ranking.Review // unknown 0.25, margin 0.15, confidence 0.30
	tests := []struct {
		name string
		s    Signals
		want []string
	}{
		{"confident", Signals{PUnknown: 0.02, KindMargin: 0.6, Confidence: f(0.8)}, nil},
		{"high unknown", Signals{PUnknown: 0.4, KindMargin: 0.6, Confidence: f(0.8)}, []string{"high_unknown_probability"}},
		{"unknown exactly at threshold", Signals{PUnknown: 0.25, KindMargin: 0.6}, []string{"high_unknown_probability"}},
		{"kind close", Signals{KindMargin: 0.05, Confidence: f(0.8)}, []string{"kind_top_two_close"}},
		{"low confidence", Signals{KindMargin: 0.6, Confidence: f(0.1)}, []string{"low_confidence"}},
		{"no confidence reported", Signals{KindMargin: 0.6}, nil},
		{"everything", Signals{PUnknown: 0.9, KindMargin: 0, Confidence: f(0.01)}, []string{"high_unknown_probability", "kind_top_two_close", "low_confidence"}},
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
	mid := Signals{PDatapoint: 1} // only the datapoint term contributes
	if s := Score(cfg, mid, 0); math.Abs(s-0.40) > 1e-9 {
		t.Errorf("datapoint-only = %v, want 0.40", s)
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
		{NormHash: "a", Score: 0.5, Kind: "stat"},      // 0
		{NormHash: "b", Score: 0.9, Kind: "stat"},      // 1
		{NormHash: "a", Score: 0.5, Kind: "stat"},      // 2 duplicate of 0
		{NormHash: "c", Score: 0.9, Kind: "stat"},      // 3 ties with 1 → document order
		{NormHash: "d", Score: 0.7, Kind: "narrative"}, // 4
		{NormHash: "e", Score: 0.1, Kind: "other"},     // 5
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
	t.Run("unclassified items are not capped", func(t *testing.T) {
		its := []Item{{NormHash: "x", Score: 0.9}, {NormHash: "y", Score: 0.8}, {NormHash: "z", Score: 0.7}}
		r, _ := Rank(its)
		if got := Select(its, r, SelectOptions{MaxPerKind: 1}); len(got) != 3 {
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

// Regression: importance/kind confidences are inherently low (flat ordinal and
// many-way distributions). They used to flag every segment as low_confidence.
func TestReviewIgnoresLowConfidenceOfOrdinalQuestions(t *testing.T) {
	cfg := mustCfg(t)
	a := answers(0.9, 0.02, "stat", map[string]float64{"stat": 0.9, "claim": 0.1}, 3, 0.1)
	a["is_datapoint"] = tev1.Answer{Choice: "yes", Probabilities: map[string]float64{"yes": 0.9, "no": 0.08, "unknown": 0.02}, Confidence: f(0.9)}
	s, err := ExtractSignals(cfg, a)
	if err != nil {
		t.Fatal(err)
	}
	if s.Confidence == nil || *s.Confidence != 0.9 {
		t.Fatalf("confidence = %v, want 0.9 (is_datapoint only)", s.Confidence)
	}
	if r := Review(cfg.Ranking.Review, s); len(r) != 0 {
		t.Errorf("review = %v, want none", r)
	}
	// Opting everything back in restores the old behaviour.
	cfg.Ranking.Review.ConfidenceQuestions = []string{"is_datapoint", "kind", "importance"}
	s, _ = ExtractSignals(cfg, a)
	if s.Confidence == nil || *s.Confidence != 0.1 {
		t.Fatalf("confidence = %v, want 0.1", s.Confidence)
	}
	if r := Review(cfg.Ranking.Review, s); len(r) != 1 || r[0] != "low_confidence" {
		t.Errorf("review = %v", r)
	}
	// An uncertain is_datapoint is still caught.
	cfg = mustCfg(t)
	a["is_datapoint"] = tev1.Answer{Choice: "yes", Probabilities: map[string]float64{"yes": 0.4, "no": 0.35, "unknown": 0.25}, Confidence: f(0.1)}
	s, _ = ExtractSignals(cfg, a)
	if r := Review(cfg.Ranking.Review, s); len(r) != 2 {
		t.Errorf("review = %v, want high_unknown_probability + low_confidence", r)
	}
}
