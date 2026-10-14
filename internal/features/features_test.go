package features

import (
	"math"
	"testing"

	"github.com/s0cks/tev1-rank/internal/segment"
)

func TestLocal(t *testing.T) {
	tests := []struct {
		name string
		text string
		want Features // only count fields are compared
	}{
		{"empty", "", Features{}},
		{"plain prose", "The committee met and talked for a while.", Features{Words: 8}},
		{"percent", "Sales rose 14% and costs fell 3.5 percent.", Features{Numbers: 2, Percentages: 2, Words: 8}},
		{"currency symbol", "The deal was worth $2.3 billion, up from €500,000.", Features{Numbers: 2, Currency: 2, Words: 9}},
		{"currency words", "It cost 40 dollars and 12 euros.", Features{Numbers: 2, Currency: 2, Words: 7}},
		{"year", "In 1999 and again in 2024 it happened.", Features{Numbers: 2, Years: 2, Words: 8}},
		{"not a year", "There were 9999 items and 1400 more.", Features{Numbers: 2, Years: 0, Words: 7}},
		{"dates", "On March 3, 2024 and 2023-11-05 and 4/7/2022.", Features{Numbers: 8, Years: 3, Dates: 3, Entities: 1, Words: 8}},
		{"entities merge runs", "Yesterday Tim Cook met Satya Nadella in Redmond.", Features{Entities: 3, Words: 8}},
		{"sentence-initial capital ignored", "Revenue grew. Profit did not.", Features{Words: 5}},
		{"unicode", "Zürich-based Nestlé grew 5%", Features{Numbers: 1, Percentages: 1, Entities: 1, Words: 4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Local(tt.text)
			got.Length, tt.want.Length = 0, 0 // checked separately below
			if got != tt.want {
				t.Errorf("Local(%q)\n got  %+v\n want %+v", tt.text, got, tt.want)
			}
		})
	}
}

func TestLocalLengthCountsRunes(t *testing.T) {
	if got := Local("Zürich 日本").Length; got != 9 {
		t.Errorf("Length = %d, want 9 runes", got)
	}
}

func TestSummaryExcludesDocumentDependentFields(t *testing.T) {
	a := Features{Numbers: 1, Position: 0.1, TermWeight: 0.9}
	b := Features{Numbers: 1, Position: 0.8, TermWeight: 0.1}
	if a.Summary() != b.Summary() {
		t.Fatal("Summary must not depend on Position/TermWeight")
	}
}

func TestComputeAll(t *testing.T) {
	text := "Quasar telemetry quasar data.\n\nQuasar telemetry again.\n\nLunch was nice today."
	segs := segment.Split(text, segment.DefaultOptions())
	fs := ComputeAll(segs, len(text))
	if len(fs) != 3 {
		t.Fatalf("got %d feature rows", len(fs))
	}
	if fs[0].Position != 0 || !(fs[1].Position > 0 && fs[1].Position < fs[2].Position) {
		t.Errorf("positions not increasing: %v %v %v", fs[0].Position, fs[1].Position, fs[2].Position)
	}
	for i, f := range fs {
		if f.TermWeight < 0 || f.TermWeight > 1 || math.IsNaN(f.TermWeight) {
			t.Errorf("seg %d: TermWeight %v out of [0,1]", i, f.TermWeight)
		}
	}
	// Segments sharing a frequent term should outrank the one-off segment.
	if !(fs[0].TermWeight > fs[2].TermWeight) {
		t.Errorf("expected repeated-term segment (%v) > one-off segment (%v)", fs[0].TermWeight, fs[2].TermWeight)
	}
	if len(ComputeAll(nil, 0)) != 0 {
		t.Error("empty doc should yield no features")
	}
}

func TestTermWeightShortFragmentDoesNotMaxOut(t *testing.T) {
	text := "Lumumba\n\nLumumba was the elected prime minister and Lumumba was assassinated.\n\nGottlieb prepared the toxin and Gottlieb travelled to Leopoldville."
	segs := segment.Split(text, segment.DefaultOptions())
	fs := ComputeAll(segs, len(text))
	if fs[0].TermWeight >= 1 || fs[0].TermWeight >= fs[1].TermWeight {
		t.Errorf("heading term weight %v should be below the full sentence's %v", fs[0].TermWeight, fs[1].TermWeight)
	}
}
