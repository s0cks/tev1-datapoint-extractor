package evaluate

import (
	"math"
	"strings"
	"testing"
)

func approx(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func TestReadLabels(t *testing.T) {
	good := "{\"text\":\"a 1\",\"is_datapoint\":true,\"kind\":\"stat\"}\n\n{\"text\":\"b\",\"is_datapoint\":false}\n"
	ls, err := ReadLabels(strings.NewReader(good))
	if err != nil || len(ls) != 2 || ls[1].Kind != "" || !ls[0].IsDatapoint {
		t.Fatalf("%+v %v", ls, err)
	}
	for name, bad := range map[string]string{
		"not json":      "nope\n",
		"empty text":    "{\"text\":\"  \",\"is_datapoint\":true}\n",
		"unknown field": "{\"text\":\"x\",\"is_datapoint\":true,\"isdatapoint\":1}\n",
		"wrong type":    "{\"text\":\"x\",\"is_datapoint\":\"yes\"}\n",
	} {
		if _, err := ReadLabels(strings.NewReader(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ReadLabels(strings.NewReader("{\"text\":\"x\",\"is_datapoint\":true}\nbad\n")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error should name the line: %v", err)
	}
}

func TestCompute(t *testing.T) {
	labels := []Label{
		{"a", true, "stat"},           // predicted yes/stat  → TP, kind ok
		{"b", true, "timeline_event"}, // predicted yes/stat  → TP, kind wrong
		{"c", false, "narrative"},     // predicted no/narrative → TN, kind ok
		{"d", false, ""},              // predicted yes/claim → FP, no kind label
		{"e", true, "stat"},           // predicted no/narrative → FN, kind wrong
		{"f", true, "stat"},           // model failed
	}
	preds := []Prediction{
		{true, 0.9, "stat", 1}, {true, 0.8, "stat", 2}, {true, 0.1, "narrative", 5},
		{true, 0.7, "claim", 3}, {true, 0.2, "narrative", 4}, {OK: false, Rank: 6},
	}
	r := Compute(labels, preds, 0.5, []int{2, 3, 5, 50})
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
	// stat: support 2 (a, e), predicted 2 (a, b), tp 1
	approx(t, "stat precision", byKind["stat"].Precision, 0.5)
	approx(t, "stat recall", byKind["stat"].Recall, 0.5)
	// narrative: support 1 (c), predicted 2 (c, e), tp 1
	approx(t, "narrative precision", byKind["narrative"].Precision, 0.5)
	approx(t, "narrative recall", byKind["narrative"].Recall, 1)
	// timeline_event: support 1, predicted 0 → precision undefined (nil), recall 0
	if byKind["timeline_event"].Precision != nil {
		t.Error("precision with no predictions must be null")
	}
	approx(t, "timeline recall", byKind["timeline_event"].Recall, 0)
	// claim was predicted for an item without a kind label → excluded entirely
	if _, ok := byKind["claim"]; ok {
		t.Error("items without a kind label must not affect kind metrics")
	}
	// ranking by Rank: a(1,T) b(2,T) d(3,F) e(4,T) c(5,F) f(6,T)
	for k, want := range map[string]float64{"2": 1, "3": 2.0 / 3, "5": 3.0 / 5, "50": 4.0 / 6} {
		if math.Abs(r.PrecisionAtK[k]-want) > 1e-9 {
			t.Errorf("P@%s = %v, want %v", k, r.PrecisionAtK[k], want)
		}
	}
}

func TestComputeEdgeCases(t *testing.T) {
	r := Compute(nil, nil, 0.5, []int{5})
	if r.DatapointAccuracy != nil || r.KindAccuracy != nil || len(r.PrecisionAtK) != 0 {
		t.Errorf("empty: %+v", r)
	}
	// Duplicates (Rank 0) are excluded from P@K; failures are excluded from accuracy.
	labels := []Label{{Text: "x", IsDatapoint: true}, {Text: "x", IsDatapoint: true}}
	r = Compute(labels, []Prediction{{OK: true, PDatapoint: 1, Kind: "stat", Rank: 1}, {OK: true, PDatapoint: 1, Kind: "stat"}}, 0.5, []int{5})
	if r.PrecisionAtK["5"] != 1 || r.Scored != 2 {
		t.Errorf("%+v", r)
	}
	// Threshold boundary: P == threshold counts as yes.
	r = Compute(labels[:1], []Prediction{{OK: true, PDatapoint: 0.5, Rank: 1}}, 0.5, nil)
	approx(t, "boundary accuracy", r.DatapointAccuracy, 1)
	// No-model style: nothing OK → no accuracy but P@K still works.
	r = Compute(labels[:1], []Prediction{{Rank: 1}}, 0.5, []int{1})
	if r.DatapointAccuracy != nil || r.PrecisionAtK["1"] != 1 {
		t.Errorf("%+v", r)
	}
}

func TestFormatSmoke(t *testing.T) {
	r := Compute([]Label{{"a", true, "stat"}}, []Prediction{{true, 1, "stat", 1}}, 0.5, []int{5, 10})
	r.Mode, r.Model = "model", "tev1:0.8b"
	out := r.Format()
	for _, want := range []string{"tev1:0.8b", "is_datapoint accuracy", "stat", "precision@5", "precision@10"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}
