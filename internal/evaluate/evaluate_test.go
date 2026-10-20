package evaluate

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/s0cks/tev1-rank/internal/config"
	"github.com/s0cks/tev1-rank/internal/pipeline"
)

func approx(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func TestReadLabels(t *testing.T) {
	good := "{\"text\":\"a 1\",\"is_datapoint\":true,\"kind\":\"stats\"}\n\n{\"text\":\"b\",\"is_datapoint\":false}\n{\"text\":\"c\",\"is_datapoint\":false,\"kind\":\"other\"}\n"
	ls, err := ReadLabels(strings.NewReader(good))
	if err != nil || len(ls) != 3 || ls[1].Kind != "" || !ls[0].IsDatapoint {
		t.Fatalf("%+v %v", ls, err)
	}

	for name, bad := range map[string]string{
		"not json":            "nope\n",
		"empty text":          "{\"text\":\"  \",\"is_datapoint\":true}\n",
		"unknown field":       "{\"text\":\"x\",\"is_datapoint\":true,\"isdatapoint\":1}\n",
		"wrong type":          "{\"text\":\"x\",\"is_datapoint\":\"yes\"}\n",
		"other but datapoint": "{\"text\":\"x\",\"is_datapoint\":true,\"kind\":\"other\"}\n",
		"kind but not":        "{\"text\":\"x\",\"is_datapoint\":false,\"kind\":\"stats\"}\n",
	} {
		if _, err := ReadLabels(strings.NewReader(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	if _, err := ReadLabels(strings.NewReader("{\"text\":\"x\",\"is_datapoint\":true}\nbad\n")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error should name the line: %v", err)
	}
}

// The shipped example labels must load (and so be self-consistent).
func TestExampleLabelsLoad(t *testing.T) {
	ls, err := LoadLabels("../../testdata/labels.example.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	kinds := map[string]int{}
	for _, l := range ls {
		kinds[l.Kind]++
	}

	for _, k := range []string{"stats", "chart", "timeline", "quote", "findings", "threads", "callout", "profile", "sources", "other"} {
		if kinds[k] == 0 {
			t.Errorf("no example label of kind %q", k)
		}
	}
}

func TestCompute(t *testing.T) {
	labels := []Label{
		{"a", true, "stats"},    // predicted stats    → TP, kind ok
		{"b", true, "timeline"}, // predicted stats    → TP, kind wrong
		{"c", false, "other"},   // predicted other    → TN, kind ok
		{"d", false, ""},        // predicted findings → FP, no kind label
		{"e", true, "stats"},    // predicted other    → FN, kind wrong
		{"f", true, "stats"},    // model failed
	}
	preds := []Prediction{
		{true, 0.9, "stats", 1}, {true, 0.8, "stats", 2}, {true, 0.1, "other", 5},
		{true, 0.7, "findings", 3}, {true, 0.2, "other", 4}, {OK: false, Rank: 6},
	}
	r := Compute(labels, preds, []int{2, 3, 5, 50})
	if r.Labels != 6 || r.Scored != 5 || r.Errors != 1 || r.Positives != 4 {
		t.Errorf("counts: %+v", r)
	}

	approx(t, "accuracy", r.DatapointAccuracy, 3.0/5)
	if *r.Confusion != (Confusion{TP: 2, FP: 1, TN: 1, FN: 1}) {
		t.Errorf("confusion = %+v", r.Confusion)
	}

	approx(t, "kind accuracy", r.KindAccuracy, 2.0/4) // scored+kind-labeled: a ok, b wrong, c ok, e wrong
	byKind := map[string]KindMetric{}
	for _, k := range r.Kinds {
		byKind[k.Kind] = k
	}

	// stats: support 2 (a, e), predicted 2 (a, b), tp 1
	approx(t, "stats precision", byKind["stats"].Precision, 0.5)
	approx(t, "stats recall", byKind["stats"].Recall, 0.5)
	// other: support 1 (c), predicted 2 (c, e), tp 1
	approx(t, "other precision", byKind["other"].Precision, 0.5)
	approx(t, "other recall", byKind["other"].Recall, 1)
	// timeline: support 1, predicted 0 → precision undefined (nil), recall 0
	if byKind["timeline"].Precision != nil {
		t.Error("precision with no predictions must be null")
	}

	approx(t, "timeline recall", byKind["timeline"].Recall, 0)
	// findings was predicted for an item without a kind label → excluded entirely
	if _, ok := byKind["findings"]; ok {
		t.Error("items without a kind label must not affect kind metrics")
	}

	// confusion matrix: rows labeled, columns predicted
	wantConf := map[string]map[string]int{"stats": {"stats": 1, "other": 1}, "timeline": {"stats": 1}, "other": {"other": 1}}
	if !reflect.DeepEqual(r.KindConfusion, wantConf) {
		t.Errorf("kind confusion = %v, want %v", r.KindConfusion, wantConf)
	}

	// ranking by Rank: a(1,T) b(2,T) d(3,F) e(4,T) c(5,F) f(6,T)
	for k, want := range map[string]float64{"2": 1, "3": 2.0 / 3, "5": 3.0 / 5, "50": 4.0 / 6} {
		if math.Abs(r.PrecisionAtK[k]-want) > 1e-9 {
			t.Errorf("P@%s = %v, want %v", k, r.PrecisionAtK[k], want)
		}
	}
}

func TestComputeEdgeCases(t *testing.T) {
	r := Compute(nil, nil, []int{5})
	if r.DatapointAccuracy != nil || r.KindAccuracy != nil || len(r.PrecisionAtK) != 0 {
		t.Errorf("empty: %+v", r)
	}

	// Duplicates (Rank 0) are excluded from P@K; failures are excluded from accuracy.
	labels := []Label{{Text: "x", IsDatapoint: true}, {Text: "x", IsDatapoint: true}}
	r = Compute(labels, []Prediction{{OK: true, PDatapoint: 1, Kind: "stats", Rank: 1}, {OK: true, PDatapoint: 1, Kind: "stats"}}, []int{5})
	if r.PrecisionAtK["5"] != 1 || r.Scored != 2 {
		t.Errorf("%+v", r)
	}

	// The datapoint decision is the kind, not the soft probability.
	r = Compute(labels[:1], []Prediction{{OK: true, PDatapoint: 0.9, Kind: "other", Rank: 1}}, nil)
	approx(t, "other with high p is still a miss", r.DatapointAccuracy, 0)
	// No-model style: nothing OK → no accuracy but P@K still works.
	r = Compute(labels[:1], []Prediction{{Rank: 1}}, []int{1})
	if r.DatapointAccuracy != nil || r.PrecisionAtK["1"] != 1 {
		t.Errorf("%+v", r)
	}
}

func TestFormatSmoke(t *testing.T) {
	r := Compute([]Label{{"a", true, "stats"}, {"b", false, "other"}}, []Prediction{{true, 1, "stats", 1}, {true, 0.1, "stats", 2}}, []int{5, 10})
	r.Mode, r.Model = "model", "tev1:0.8b"
	out := r.Format()
	for _, want := range []string{"tev1:0.8b", "is_datapoint accuracy", "stats", "kind confusion", "precision@5", "precision@10"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

// Labels are bare text with no upstream annotations, so there is no features-only
// baseline to evaluate: --no-model must be refused, not silently report noise.
func TestRunRefusesNoModel(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cl := &pipeline.Classifier{Cfg: cfg, NoModel: true}
	_, err = Run(context.Background(), cl, []Label{{Text: "Revenue grew 14%.", IsDatapoint: true}}, []int{5})
	if err == nil || !strings.Contains(err.Error(), "needs the model") {
		t.Errorf("err = %v", err)
	}
}
