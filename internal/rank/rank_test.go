package rank

import (
	"math"
	"reflect"
	"testing"

	"github.com/s0cks/tev1-rank/internal/config"
	"github.com/s0cks/tev1-rank/internal/input"
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
		f    input.Features
		want func(float64) bool
	}{
		{"nothing", input.Features{}, func(b float64) bool { return b == 0 }},
		{"saturated", input.Features{Numerics: 9, Entities: 9}, func(b float64) bool { return math.Abs(b-1) < 1e-9 }},
		{"one numeric only", input.Features{Numerics: 1}, func(b float64) bool { return b > 0 && b < 0.4 }},
		{"words and position carry no weight", input.Features{Words: 500, Position: 0.9}, func(b float64) bool { return b == 0 }},
		{"monotonic: more numerics → higher", input.Features{Numerics: 2}, func(b float64) bool { return b > FeatureBonus(cfg, input.Features{Numerics: 1}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if b := FeatureBonus(cfg, tt.f); !tt.want(b) {
				t.Errorf("bonus = %v", b)
			}
		})
	}
	if FeatureBonus(config.RankingConfig{}, input.Features{Numerics: 3}) != 0 {
		t.Error("no feature weights should give 0")
	}
	// Names left over from the old regex features are ignored, not an error.
	old := config.RankingConfig{FeatureWeights: map[string]float64{"years": 1, "term_weight": 1}}
	if FeatureBonus(old, input.Features{Numerics: 3, Entities: 3}) != 0 {
		t.Error("unknown feature names must not contribute")
	}
}
