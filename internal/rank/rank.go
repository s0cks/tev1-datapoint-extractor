// Package rank turns model answers and deterministic features into a score,
// review flags, a ranking with near-duplicate removal, and a top-N selection.
package rank

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/s0cks/tev1-rank/internal/config"
	"github.com/s0cks/tev1-rank/internal/features"
	"github.com/s0cks/tev1-rank/internal/tev1"
)

// Signals are the model-derived quantities that feed the final score.
type Signals struct {
	// PDatapoint is 1 - P(kind = other): the chance the segment is a datapoint
	// at all. There is no separate is_datapoint question; "other" is the model
	// saying "not a datapoint".
	PDatapoint     float64            `json:"p_datapoint"`
	Importance     float64            `json:"importance"`      // expected importance on the configured 1..N scale; 0 when not asked (kind = other)
	ImportanceNorm float64            `json:"importance_norm"` // importance rescaled to [0,1]; 0 when not asked
	Kind           string             `json:"kind"`            // arg-max kind
	KindProbs      map[string]float64 `json:"kind_probs"`
	KindWeight     float64            `json:"kind_weight"`          // probability-weighted per-kind weight
	KindMargin     float64            `json:"kind_margin"`          // gap between the two most likely kinds
	Confidence     *float64           `json:"confidence,omitempty"` // lowest model confidence among ranking.review.confidence_questions
}

// IsDatapoint reports whether the model's chosen kind is anything but "other".
// This is the same test the pipeline uses to decide whether to ask for importance.
func (s Signals) IsDatapoint() bool { return s.Kind != config.KindOther }

// ExtractSignals derives Signals from raw answers. The kind answer is required;
// the importance answer is absent for segments classified as "other", which are
// not asked for it.
func ExtractSignals(cfg config.Config, ans map[string]tev1.Answer) (Signals, error) {
	var s Signals
	confQ := map[string]bool{}
	for _, q := range cfg.Ranking.Review.ConfidenceQuestions {
		confQ[q] = true
	}

	minConf := func(question string, c *float64) {
		if confQ[question] && c != nil && (s.Confidence == nil || *c < *s.Confidence) {
			v := *c
			s.Confidence = &v
		}
	}

	ka, ok := ans[config.QKind]
	if !ok {
		return s, fmt.Errorf("no answer for %q", config.QKind)
	}
	s.Kind = ka.Choice
	s.KindProbs = normalise(ka.Probabilities, ka.Choice)
	top1, top2 := 0.0, 0.0
	for k, p := range s.KindProbs {
		w, ok := cfg.Ranking.KindWeights[k]
		if !ok {
			w = cfg.Ranking.DefaultKindWeight
		}

		s.KindWeight += p * w
		if p > top1 {
			top1, top2 = p, top1
		} else if p > top2 {
			top2 = p
		}
	}
	s.KindMargin = top1 - top2
	s.PDatapoint = clamp01(1 - s.KindProbs[config.KindOther])
	minConf(config.QKind, ka.Confidence)

	iq, _ := cfg.Question(config.QImportance)
	ia, ok := ans[config.QImportance]
	if ok && ia.Score != nil {
		levels := iq.Levels()
		s.Importance = *ia.Score + 1
		s.ImportanceNorm = clamp01(*ia.Score / float64(levels-1))
		minConf(config.QImportance, ia.Confidence)
	}

	return s, nil
}

// normalise returns probs scaled to sum to 1, or a one-hot map on choice when
// the server returned no probabilities.
func normalise(probs map[string]float64, choice string) map[string]float64 {
	sum := 0.0
	for _, p := range probs {
		sum += p
	}
	if len(probs) == 0 || sum <= 0 {
		return map[string]float64{choice: 1}
	}
	out := make(map[string]float64, len(probs))
	for k, p := range probs {
		out[k] = p / sum
	}
	return out
}

// FeatureBonus is a weighted average, in [0,1], of saturating feature values.
func FeatureBonus(cfg config.RankingConfig, f features.Features) float64 {
	sat := cfg.FeatureSaturation
	if sat <= 0 {
		sat = 2
	}
	vals := map[string]float64{
		"numbers": float64(f.Numbers), "percentages": float64(f.Percentages),
		"currency": float64(f.Currency), "years": float64(f.Years),
		"dates": float64(f.Dates), "entities": float64(f.Entities),
	}
	var num, den float64
	for name, w := range cfg.FeatureWeights {
		if w <= 0 {
			continue
		}
		var v float64
		if name == "term_weight" {
			v = clamp01(f.TermWeight)
		} else if c, ok := vals[name]; ok {
			v = math.Min(1, c/sat)
		} else {
			continue
		}
		num += w * v
		den += w
	}
	if den == 0 {
		return 0
	}
	return clamp01(num / den)
}

// Score combines the model signals and the feature bonus into final_score in [0,1].
func Score(cfg config.RankingConfig, s Signals, bonus float64) float64 {
	w := cfg.Weights
	num := w.Datapoint*s.PDatapoint + w.Importance*s.ImportanceNorm + w.Kind*s.KindWeight + w.Features*bonus
	return clamp01(num / (w.Datapoint + w.Importance + w.Kind + w.Features))
}

// Review lists the reasons a human should look at this segment (empty = fine).
func Review(cfg config.ReviewConfig, s Signals) []string {
	var r []string
	if math.Abs(s.PDatapoint-0.5) < 0.1 {
		r = append(r, "datapoint_uncertain")
	}
	if s.KindMargin < cfg.KindMargin {
		r = append(r, "kind_top_two_close")
	}
	if s.Confidence != nil && *s.Confidence < cfg.MinConfidence {
		r = append(r, "low_confidence")
	}
	return r
}

// NormalizedHash hashes text after lower-casing and collapsing everything
// that is not a letter or digit, so near-identical segments (case, spacing,
// punctuation) collide.
func NormalizedHash(text string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(b.String())))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Item is the minimal view of a segment needed for ranking.
type Item struct {
	NormHash string
	Score    float64
	Kind     string // "" when unclassified (no-model mode)
}

// Rank orders items by descending score (ties broken by document order) and
// removes near-duplicates. ranks[i] is the 1-based rank of item i, or 0 for a
// duplicate; dupOf[i] is the index of the item it duplicates, or -1.
func Rank(items []Item) (ranks []int, dupOf []int) {
	ranks = make([]int, len(items))
	dupOf = make([]int, len(items))
	first := map[string]int{}
	var canon []int
	for i, it := range items {
		dupOf[i] = -1
		if j, ok := first[it.NormHash]; ok {
			dupOf[i] = j
			continue
		}
		first[it.NormHash] = i
		canon = append(canon, i)
	}
	sort.SliceStable(canon, func(a, b int) bool {
		if items[canon[a]].Score != items[canon[b]].Score {
			return items[canon[a]].Score > items[canon[b]].Score
		}
		return canon[a] < canon[b]
	})
	for r, i := range canon {
		ranks[i] = r + 1
	}
	return ranks, dupOf
}

// SelectOptions controls Select.
type SelectOptions struct {
	Top        int // 0 = no limit
	MinScore   float64
	MaxPerKind int // 0 = no cap; items without a kind are never capped
	// ExcludeKinds are never selected (default config: "other", the segments
	// the model decided are not datapoints). Items without a kind are never excluded.
	ExcludeKinds []string
}

// Select returns indices of selected items in rank order.
func Select(items []Item, ranks []int, opt SelectOptions) []int {
	var order []int
	for i, r := range ranks {
		if r > 0 {
			order = append(order, i)
		}
	}
	sort.Slice(order, func(a, b int) bool { return ranks[order[a]] < ranks[order[b]] })
	var out []int
	perKind := map[string]int{}
	excluded := map[string]bool{}
	for _, k := range opt.ExcludeKinds {
		excluded[k] = true
	}
	for _, i := range order {
		if opt.Top > 0 && len(out) >= opt.Top {
			break
		}
		if items[i].Score < opt.MinScore {
			break // sorted by score: nothing later can pass
		}
		if k := items[i].Kind; k != "" && excluded[k] {
			continue
		}
		if k := items[i].Kind; opt.MaxPerKind > 0 && k != "" {
			if perKind[k] >= opt.MaxPerKind {
				continue
			}
			perKind[k]++
		}
		out = append(out, i)
	}
	return out
}

func clamp01(x float64) float64 {
	switch {
	case math.IsNaN(x), x < 0:
		return 0
	case x > 1:
		return 1
	}
	return x
}
