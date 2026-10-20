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

	"github.com/s0cks/tev1-rank/internal/config"
	"github.com/s0cks/tev1-rank/internal/input"
	"github.com/s0cks/tev1-rank/internal/pipeline"
	"github.com/s0cks/tev1-rank/internal/rank"
)

// Label is one line of the labels file:
//
//	{"text": "...", "is_datapoint": true, "kind": "stats"}
//
// "kind" is optional; unlabeled kinds are left out of the kind metrics. When
// given it must agree with is_datapoint: "other" means not a datapoint, every
// other kind means datapoint.
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

		if l.Kind != "" && l.IsDatapoint == (l.Kind == config.KindOther) {
			return nil, fmt.Errorf("line %d: kind %q contradicts is_datapoint=%t (kind %q means not a datapoint)", n, l.Kind, l.IsDatapoint, config.KindOther)
		}

		out = append(out, l)
	}

	return out, sc.Err()
}

// Prediction is what the classifier produced for one label.
type Prediction struct {
	OK         bool    // false when the model call failed or the run was feature-only
	PDatapoint float64 // 1 - P(kind = other); informational, the decision is Kind != "other"
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
	Model             string       `json:"model,omitempty"`
	Mode              string       `json:"mode"`
	Labels            int          `json:"labels"`
	Scored            int          `json:"scored"` // labels with a usable model prediction
	Errors            int          `json:"errors"`
	DatapointAccuracy *float64     `json:"datapoint_accuracy"`
	Confusion         *Confusion   `json:"datapoint_confusion"`
	KindAccuracy      *float64     `json:"kind_accuracy"`
	Kinds             []KindMetric `json:"kinds"`
	// KindConfusion[labeled][predicted] counts kind-labeled items.
	KindConfusion map[string]map[string]int `json:"kind_confusion"`
	PrecisionAtK  map[string]float64        `json:"precision_at_k"` // key: "5", "10", ...
	Positives     int                       `json:"positives"`      // labeled is_datapoint=true (the P@K baseline rate is Positives/Labels)
}

func ratio(num, den int) *float64 {
	if den == 0 {
		return nil
	}

	v := float64(num) / float64(den)
	return &v
}

// Compute is the pure metrics function (no I/O, no model).
func Compute(labels []Label, preds []Prediction, ks []int) Report {
	r := Report{
		Labels:        len(labels),
		PrecisionAtK:  map[string]float64{},
		Kinds:         []KindMetric{},
		KindConfusion: map[string]map[string]int{},
	}

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
		yes := p.Kind != config.KindOther // the pipeline's own gate for the importance request
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
			if r.KindConfusion[l.Kind] == nil {
				r.KindConfusion[l.Kind] = map[string]int{}
			}

			r.KindConfusion[l.Kind][p.Kind]++
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
		r.Kinds = append(r.Kinds, KindMetric{
			Kind:      k,
			Support:   n.support,
			Predicted: n.predicted,
			TP:        n.tp,
			Precision: ratio(n.tp, n.predicted),
			Recall:    ratio(n.tp, n.support),
		})
	}

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

		kk := min(k, len(order))
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

// Run classifies every label as a stand-alone paragraph and computes the report.
//
// Labels are bare text with no upstream annotations, so they carry no
// features, and a features-only baseline would rank everything equally;
// Run therefore needs the model.
func Run(ctx context.Context, cl *pipeline.Classifier, labels []Label, ks []int) (Report, error) {
	if cl.NoModel {
		return Report{}, fmt.Errorf("eval needs the model: labels are bare text without upstream annotations, so there is nothing to rank without it")
	}
	segs := make([]input.Segment, len(labels))
	for i, l := range labels {
		segs[i] = input.Segment{
			ID: fmt.Sprintf("label-%04d", i+1), Kind: "paragraph", Section: []string{},
			Range: input.Range{End: len(l.Text)}, Text: l.Text, Plain: l.Text, Hash: input.HashText(l.Text),
		}
	}

	feats := make([]input.Features, len(segs))
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

	rep := Compute(labels, preds, ks)
	rep.Mode, rep.Model = "model", cl.Cfg.Model

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
		fmt.Fprintf(&b, "is_datapoint accuracy (kind != other): %s", pct(r.DatapointAccuracy))
		if r.Confusion != nil {
			fmt.Fprintf(&b, "   [TP %d  FP %d  TN %d  FN %d]", r.Confusion.TP, r.Confusion.FP, r.Confusion.TN, r.Confusion.FN)
		}

		fmt.Fprintf(&b, "\nkind accuracy: %s\n\n", pct(r.KindAccuracy))
		fmt.Fprintf(&b, "%-16s %7s %9s %10s %8s\n", "kind", "support", "predicted", "precision", "recall")
		for _, k := range r.Kinds {
			fmt.Fprintf(&b, "%-16s %7d %9d %10s %8s\n", k.Kind, k.Support, k.Predicted, pct(k.Precision), pct(k.Recall))
		}

		b.WriteByte('\n')
		b.WriteString(r.formatConfusion())
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

// formatConfusion renders KindConfusion as a matrix (rows = labeled kind,
// columns = predicted kind), which shows which criteria descriptions to fix.
func (r Report) formatConfusion() string {
	if len(r.KindConfusion) == 0 {
		return ""
	}

	set := map[string]bool{}
	for l, row := range r.KindConfusion {
		set[l] = true
		for p := range row {
			set[p] = true
		}
	}

	kinds := make([]string, 0, len(set))
	for k := range set {
		kinds = append(kinds, k)
	}

	sort.Strings(kinds)
	var b strings.Builder
	b.WriteString("kind confusion (rows: labeled, columns: predicted)\n")
	fmt.Fprintf(&b, "%-10s", "")
	for _, k := range kinds {
		fmt.Fprintf(&b, " %8.8s", k)
	}

	b.WriteByte('\n')
	for _, l := range kinds {
		if r.KindConfusion[l] == nil {
			continue
		}

		fmt.Fprintf(&b, "%-10.10s", l)
		for _, p := range kinds {
			if n := r.KindConfusion[l][p]; n > 0 {
				fmt.Fprintf(&b, " %8d", n)
			} else {
				fmt.Fprintf(&b, " %8s", ".")
			}
		}

		b.WriteByte('\n')
	}

	b.WriteByte('\n')
	return b.String()
}
