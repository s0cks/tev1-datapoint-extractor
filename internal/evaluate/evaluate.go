// Package evaluate scores the classifier against a hand-labeled JSONL set.
package evaluate

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/s0cks/tev1-rank/internal/features"
	"github.com/s0cks/tev1-rank/internal/pipeline"
	"github.com/s0cks/tev1-rank/internal/rank"
	"github.com/s0cks/tev1-rank/internal/segment"
)

// Label is one line of the labels file:
//
//	{"text": "...", "is_datapoint": true, "kind": "stat"}
//
// "kind" is optional; unlabeled kinds are left out of the kind metrics.
type Label struct {
	Text        string `json:"text"`
	IsDatapoint bool   `json:"is_datapoint"`
	Kind        string `json:"kind,omitempty"`
}

// LoadLabels reads a JSONL label file. Blank lines are skipped.
func LoadLabels(path string) ([]Label, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadLabels(f)
}

func ReadLabels(r io.Reader) ([]Label, error) {
	var out []Label
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var l Label
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&l); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if strings.TrimSpace(l.Text) == "" {
			return nil, fmt.Errorf("line %d: empty text", n)
		}
		out = append(out, l)
	}
	return out, sc.Err()
}

// Prediction is what the classifier produced for one label.
type Prediction struct {
	OK         bool    // false when the model call failed or the run was feature-only
	PDatapoint float64 // P(is_datapoint = yes)
	Kind       string
	Rank       int // 1-based rank; 0 = removed as a near-duplicate
}

type KindMetric struct {
	Kind      string   `json:"kind"`
	Support   int      `json:"support"`   // labeled items of this kind
	Predicted int      `json:"predicted"` // items predicted as this kind (among kind-labeled items)
	TP        int      `json:"tp"`
	Precision *float64 `json:"precision"` // null when nothing was predicted
	Recall    *float64 `json:"recall"`    // null when there is no support
}

type Confusion struct{ TP, FP, TN, FN int }

type Report struct {
	Model             string             `json:"model,omitempty"`
	Mode              string             `json:"mode"`
	Labels            int                `json:"labels"`
	Scored            int                `json:"scored"` // labels with a usable model prediction
	Errors            int                `json:"errors"`
	Threshold         float64            `json:"datapoint_threshold"`
	DatapointAccuracy *float64           `json:"datapoint_accuracy"`
	Confusion         *Confusion         `json:"datapoint_confusion"`
	KindAccuracy      *float64           `json:"kind_accuracy"`
	Kinds             []KindMetric       `json:"kinds"`
	PrecisionAtK      map[string]float64 `json:"precision_at_k"` // key: "5", "10", ...
	Positives         int                `json:"positives"`      // labeled is_datapoint=true (the P@K baseline rate is Positives/Labels)
}

func ratio(num, den int) *float64 {
	if den == 0 {
		return nil
	}
	v := float64(num) / float64(den)
	return &v
}

// Compute is the pure metrics function (no I/O, no model).
func Compute(labels []Label, preds []Prediction, threshold float64, ks []int) Report {
	r := Report{Labels: len(labels), Threshold: threshold, PrecisionAtK: map[string]float64{}, Kinds: []KindMetric{}}
	var c Confusion
	kindCorrect, kindTotal := 0, 0
	type counts struct{ support, predicted, tp int }
	per := map[string]*counts{}
	get := func(k string) *counts {
		if per[k] == nil {
			per[k] = &counts{}
		}
		return per[k]
	}
	for i, l := range labels {
		if l.IsDatapoint {
			r.Positives++
		}
		p := preds[i]
		if !p.OK {
			r.Errors++
			continue
		}
		r.Scored++
		yes := p.PDatapoint >= threshold
		switch {
		case yes && l.IsDatapoint:
			c.TP++
		case yes && !l.IsDatapoint:
			c.FP++
		case !yes && !l.IsDatapoint:
			c.TN++
		default:
			c.FN++
		}
		if l.Kind != "" {
			kindTotal++
			get(l.Kind).support++
			get(p.Kind).predicted++
			if p.Kind == l.Kind {
				kindCorrect++
				get(l.Kind).tp++
			}
		}
	}
	if r.Scored > 0 {
		r.DatapointAccuracy = ratio(c.TP+c.TN, r.Scored)
		r.Confusion = &c
	}
	r.KindAccuracy = ratio(kindCorrect, kindTotal)
	names := make([]string, 0, len(per))
	for k := range per {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		n := per[k]
		r.Kinds = append(r.Kinds, KindMetric{Kind: k, Support: n.support, Predicted: n.predicted, TP: n.tp,
			Precision: ratio(n.tp, n.predicted), Recall: ratio(n.tp, n.support)})
	}

	// precision@K over the ranking (duplicates removed).
	var order []int
	for i, p := range preds {
		if p.Rank > 0 {
			order = append(order, i)
		}
	}
	sort.Slice(order, func(a, b int) bool { return preds[order[a]].Rank < preds[order[b]].Rank })
	for _, k := range ks {
		if k <= 0 || len(order) == 0 {
			continue
		}
		kk := k
		if kk > len(order) {
			kk = len(order)
		}
		hit := 0
		for _, i := range order[:kk] {
			if labels[i].IsDatapoint {
				hit++
			}
		}
		r.PrecisionAtK[fmt.Sprint(k)] = float64(hit) / float64(kk)
	}
	return r
}

// Run classifies every label as a stand-alone segment and computes the report.
// In no-model mode only precision@K (a features-only baseline) is meaningful.
func Run(ctx context.Context, cl *pipeline.Classifier, labels []Label, threshold float64, ks []int) (Report, error) {
	segs := make([]segment.Segment, len(labels))
	total := 0
	for i, l := range labels {
		h := segment.HashText(l.Text)
		segs[i] = segment.Segment{ID: fmt.Sprintf("label-%04d", i+1), Start: 0, End: len(l.Text), Text: l.Text, Hash: h}
		total += len(l.Text)
	}
	feats := features.ComputeAll(segs, total)
	recs, err := cl.BuildRecords(ctx, segs, feats)
	if err != nil {
		return Report{}, err
	}
	pipeline.Finalize(recs, rank.SelectOptions{})
	preds := make([]Prediction, len(recs))
	for i, rec := range recs {
		if rec.Rank != nil {
			preds[i].Rank = *rec.Rank
		}
		if rec.Signals != nil {
			preds[i].OK, preds[i].PDatapoint, preds[i].Kind = true, rec.Signals.PDatapoint, rec.Signals.Kind
		}
	}
	rep := Compute(labels, preds, threshold, ks)
	rep.Mode = "model"
	if cl.NoModel {
		rep.Mode, rep.Errors = "no-model", 0
	} else {
		rep.Model = cl.Cfg.Model
	}
	return rep, nil
}

// Format renders a human-readable report.
func (r Report) Format() string {
	var b strings.Builder
	pct := func(p *float64) string {
		if p == nil {
			return "   -  "
		}
		return fmt.Sprintf("%5.1f%%", *p*100)
	}
	fmt.Fprintf(&b, "mode: %s", r.Mode)
	if r.Model != "" {
		fmt.Fprintf(&b, "   model: %s", r.Model)
	}
	fmt.Fprintf(&b, "\nlabels: %d   scored: %d   errors: %d   positives: %d (%.0f%% base rate)\n\n", r.Labels, r.Scored, r.Errors, r.Positives, 100*float64(r.Positives)/float64(max(r.Labels, 1)))
	if r.Mode == "model" {
		fmt.Fprintf(&b, "is_datapoint accuracy (P(yes) >= %.2f): %s", r.Threshold, pct(r.DatapointAccuracy))
		if r.Confusion != nil {
			fmt.Fprintf(&b, "   [TP %d  FP %d  TN %d  FN %d]", r.Confusion.TP, r.Confusion.FP, r.Confusion.TN, r.Confusion.FN)
		}
		fmt.Fprintf(&b, "\nkind accuracy: %s\n\n", pct(r.KindAccuracy))
		fmt.Fprintf(&b, "%-16s %7s %9s %10s %8s\n", "kind", "support", "predicted", "precision", "recall")
		for _, k := range r.Kinds {
			fmt.Fprintf(&b, "%-16s %7d %9d %10s %8s\n", k.Kind, k.Support, k.Predicted, pct(k.Precision), pct(k.Recall))
		}
		b.WriteByte('\n')
	}
	keys := make([]string, 0, len(r.PrecisionAtK))
	for k := range r.PrecisionAtK {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return len(keys[i]) < len(keys[j]) || (len(keys[i]) == len(keys[j]) && keys[i] < keys[j])
	})
	for _, k := range keys {
		v := r.PrecisionAtK[k]
		fmt.Fprintf(&b, "precision@%-3s %5.1f%%\n", k, v*100)
	}
	return b.String()
}
