package input

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	samplePath = filepath.Join("..", "..", "testdata", "segments.sample.json")
	sourcePath = filepath.Join("..", "..", "testdata", "sample.md")
)

func TestSampleFixture(t *testing.T) {
	doc, data, err := Load(samplePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || doc.Source != "sample.md" || doc.Format != "md" || len(doc.Segments) != 10 || doc.Skipped != 0 {
		t.Fatalf("doc = %q %q segments=%d skipped=%d", doc.Source, doc.Format, len(doc.Segments), doc.Skipped)
	}

	// Segment ranges are byte offsets into the source document.
	md, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, s := range doc.Segments {
		if got := string(md[s.Range.Start:s.Range.End]); got != s.Text {
			t.Errorf("%s: source[%d:%d] = %q, want %q", s.ID, s.Range.Start, s.Range.End, got, s.Text)
		}
		if s.Hash != HashText(s.Plain) {
			t.Errorf("%s: hash is not the hash of plain", s.ID)
		}
		kinds[s.Kind]++
	}
	if kinds["heading"] != 4 || kinds["paragraph"] != 4 || kinds["blockquote"] != 1 || kinds["list_item"] != 1 {
		t.Errorf("kinds = %v", kinds)
	}
	if h := doc.Segments[1]; h.Level != 4 || strings.Join(h.Section, "/") != "1987/Founding" {
		t.Errorf("heading = level %d section %v", h.Level, h.Section)
	}
}

func TestFeatures(t *testing.T) {
	doc, _, err := Load(samplePath)
	if err != nil {
		t.Fatal(err)
	}
	f := doc.Features()
	if len(f) != len(doc.Segments) {
		t.Fatalf("len = %d", len(f))
	}
	// s6: "Revenue grew 14% in 2024 to $2.3 billion, up from $2.02 billion ..." — four CD tokens, no entities.
	if f[6].Numerics != 4 || f[6].Entities != 0 || f[6].Words < 10 {
		t.Errorf("s6 = %+v", f[6])
	}
	// s9 has a named entity (European Union); punctuation is not a word.
	if f[9].Entities != 1 {
		t.Errorf("s9 = %+v", f[9])
	}
	last := doc.Segments[len(doc.Segments)-1]
	for i, x := range f {
		if x.Position < 0 || x.Position >= 1 {
			t.Errorf("s%d position = %v", i, x.Position)
		}
	}
	if want := float64(doc.Segments[6].Range.Start) / float64(last.Range.End); f[6].Position != want {
		t.Errorf("position = %v, want %v", f[6].Position, want)
	}
	if got := f[6].Summary(); !strings.HasPrefix(got, "numerics=4; named_entities=0; words=") || strings.Contains(got, "position") {
		t.Errorf("summary = %q", got)
	}
}

func TestParse(t *testing.T) {
	seg := func(id, plain string) string {
		return `{"type":"segment","id":"` + id + `","kind":"paragraph","range":{"start":0,"end":5},"text":"x","plain":"` + plain + `"}`
	}
	tests := []struct {
		name    string
		in      string
		wantErr string
		wantN   int
		skipped int
	}{
		{"empty list is fine", `{"source":"a.md","segments":[]}`, "", 0, 0},
		{"minimal", `{"segments":[` + seg("a", "hello") + `]}`, "", 1, 0},
		{"unknown fields ignored", `{"extra":1,"segments":[{"type":"segment","id":"a","plain":"hi","future":{"x":1}}]}`, "", 1, 0},
		{"unknown entry type skipped", `{"segments":[{"type":"table","id":"t"},` + seg("a", "hello") + `]}`, "", 1, 1},
		{"blank plain skipped", `{"segments":[` + seg("a", "  ") + `,` + seg("b", "hello") + `]}`, "", 1, 1},
		{"zero-width only is blank", `{"segments":[{"type":"segment","id":"a","plain":"\u200d\u200b"}]}`, "", 0, 1},
		{"not json", `hello`, "invalid JSON", 0, 0},
		{"trailing data", `{"segments":[]} {"segments":[]}`, "unexpected data", 0, 0},
		{"no segments key", `{"source":"a.md"}`, `missing "segments"`, 0, 0},
		{"null segments", `{"segments":null}`, `missing "segments"`, 0, 0},
		{"missing id", `{"segments":[{"type":"segment","plain":"x"}]}`, "missing id", 0, 0},
		{"duplicate id", `{"segments":[` + seg("a", "x") + `,` + seg("a", "y") + `]}`, `duplicate id "a"`, 0, 0},
		{"inverted range", `{"segments":[{"type":"segment","id":"a","range":{"start":9,"end":2},"plain":"x"}]}`, "invalid range", 0, 0},
		{"negative range", `{"segments":[{"type":"segment","id":"a","range":{"start":-1,"end":2},"plain":"x"}]}`, "invalid range", 0, 0},
		{"wrong field type", `{"segments":[{"type":"segment","id":7,"plain":"x"}]}`, "segments[0]", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := Parse([]byte(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(doc.Segments) != tt.wantN || doc.Skipped != tt.skipped {
				t.Errorf("segments=%d skipped=%d, want %d and %d", len(doc.Segments), doc.Skipped, tt.wantN, tt.skipped)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("missing file accepted")
	}
}
