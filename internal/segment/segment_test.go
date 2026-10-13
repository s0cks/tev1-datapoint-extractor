package segment

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// checkInvariants asserts the offset round-trip, hash prefix, ordering and
// size-cap invariants for any segmentation.
func checkInvariants(t *testing.T, text string, segs []Segment, opt Options) {
	t.Helper()
	prevEnd := 0
	seen := map[string]bool{}
	for i, s := range segs {
		if s.Start < 0 || s.End > len(text) || s.Start >= s.End {
			t.Fatalf("seg %d: bad range [%d,%d) for len %d", i, s.Start, s.End, len(text))
		}
		if text[s.Start:s.End] != s.Text {
			t.Fatalf("seg %d: text[%d:%d]=%q != Text %q", i, s.Start, s.End, text[s.Start:s.End], s.Text)
		}
		if s.Start < prevEnd {
			t.Fatalf("seg %d overlaps previous", i)
		}
		prevEnd = s.End
		if !strings.HasPrefix(s.Hash, "sha256:") || s.Hash != HashText(s.Text) {
			t.Fatalf("seg %d: bad hash %q", i, s.Hash)
		}
		if seen[s.ID] {
			t.Fatalf("duplicate id %q", s.ID)
		}
		seen[s.ID] = true
		if strings.TrimSpace(s.Text) != s.Text {
			t.Fatalf("seg %d not trimmed: %q", i, s.Text)
		}
		if opt.HardMaxBytes > 0 && len(s.Text) > opt.HardMaxBytes {
			t.Fatalf("seg %d exceeds hard max: %d", i, len(s.Text))
		}
		if utf8.ValidString(text) && !utf8.ValidString(s.Text) {
			t.Fatalf("seg %d split inside a UTF-8 sequence", i)
		}
	}
}

func TestSplit(t *testing.T) {
	small := Options{MaxParagraphBytes: 40, HardMaxBytes: 80}
	long := strings.Repeat("Revenue grew by 14 percent in 2024. ", 40)
	tests := []struct {
		name string
		text string
		opt  Options
		want []string // nil = only check invariants and count
		n    int      // expected count when want == nil; -1 to skip
	}{
		{"empty", "", DefaultOptions(), []string{}, 0},
		{"whitespace only", " \n\t\n \r\n", DefaultOptions(), []string{}, 0},
		{"single no trailing newline", "Hello world.", DefaultOptions(), []string{"Hello world."}, 1},
		{"single trailing newline", "Hello world.\n", DefaultOptions(), []string{"Hello world."}, 1},
		{"two paragraphs", "One.\n\nTwo.\n", DefaultOptions(), []string{"One.", "Two."}, 2},
		{"many blank lines", "A\n\n\n\n  \nB", DefaultOptions(), []string{"A", "B"}, 2},
		{"soft wrapped paragraph keeps newline", "line one\nline two\n\nnext", DefaultOptions(), []string{"line one\nline two", "next"}, 2},
		{"crlf", "A\r\n\r\nB\r\n", DefaultOptions(), []string{"A", "B"}, 2},
		{"leading whitespace", "\n\n   indented text\n", DefaultOptions(), []string{"indented text"}, 1},
		{"unicode", "Ünïcödé — naïve café.\n\n日本語のテキスト。二つ目の文。", Options{MaxParagraphBytes: 20}, []string{"Ünïcödé — naïve café.", "日本語のテキスト。", "二つ目の文。"}, 3},
		{"emoji", "Launch 🚀 hit $5M.\n\nNext 🎉", DefaultOptions(), []string{"Launch 🚀 hit $5M.", "Next 🎉"}, 2},
		{"sentence split", "Alpha is first. Beta is second! Is gamma third? Yes.", Options{MaxParagraphBytes: 10}, []string{"Alpha is first.", "Beta is second!", "Is gamma third?", "Yes."}, 4},
		{"abbreviations not split", "Dr. Smith met Mr. Jones in the U.S. last year. They talked.", Options{MaxParagraphBytes: 10}, []string{"Dr. Smith met Mr. Jones in the U.S. last year.", "They talked."}, 2},
		{"decimals not split", "Growth was 3.5 percent. Next.", Options{MaxParagraphBytes: 10}, []string{"Growth was 3.5 percent.", "Next."}, 2},
		{"lowercase after period not split", "See fig. 3 for details etc. and more. Done.", Options{MaxParagraphBytes: 10}, []string{"See fig. 3 for details etc. and more.", "Done."}, 2},
		{"closing quote kept", `He said "stop." Then left.`, Options{MaxParagraphBytes: 10}, []string{`He said "stop."`, "Then left."}, 2},
		{"short paragraph not sentence split", "A. B. C.", DefaultOptions(), []string{"A. B. C."}, 1},
		{"very long paragraph", long, DefaultOptions(), nil, -1},
		{"very long no punctuation", strings.Repeat("word ", 2000), small, nil, -1},
		{"very long single token", strings.Repeat("x", 5000), small, nil, -1},
		{"very long multibyte no spaces", strings.Repeat("日本語", 1000), small, nil, -1},
		{"invalid utf8", "ok \xff\xfe bad.\n\nnext", DefaultOptions(), nil, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			segs := Split(tt.text, tt.opt)
			checkInvariants(t, tt.text, segs, tt.opt)
			if tt.want != nil {
				var got []string
				for _, s := range segs {
					got = append(got, s.Text)
				}
				if len(got) != len(tt.want) {
					t.Fatalf("got %d segments %q, want %d %q", len(got), got, len(tt.want), tt.want)
				}
				for i := range got {
					if got[i] != tt.want[i] {
						t.Errorf("seg %d = %q, want %q", i, got[i], tt.want[i])
					}
				}
			}
			if tt.n >= 0 && len(segs) != tt.n {
				t.Errorf("count = %d, want %d", len(segs), tt.n)
			}
		})
	}
}

func TestSplitLongParagraphProducesManySegments(t *testing.T) {
	text := strings.Repeat("Revenue grew by 14 percent in 2024. ", 40)
	segs := Split(text, DefaultOptions())
	if len(segs) != 40 {
		t.Fatalf("got %d segments, want 40", len(segs))
	}
}

func TestSplitDeterministicIDs(t *testing.T) {
	text := "First para.\n\nSecond para."
	a, b := Split(text, DefaultOptions()), Split(text, DefaultOptions())
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic output at %d", i)
		}
	}
	if !strings.HasPrefix(a[0].ID, "seg-0001-") || !strings.HasPrefix(a[1].ID, "seg-0002-") {
		t.Fatalf("unexpected ids: %q %q", a[0].ID, a[1].ID)
	}
}

func FuzzSplit(f *testing.F) {
	for _, s := range []string{"", "a", "A. B.\n\nC", "Dr. X said \"hi.\" Ok.", "日本語。テスト", "\xff\n\n\xfe", strings.Repeat("a b ", 500)} {
		f.Add(s, 30, 60)
	}
	f.Fuzz(func(t *testing.T, s string, maxPara, hardMax int) {
		if maxPara < 0 || maxPara > 500 || hardMax < 0 || hardMax > 500 {
			t.Skip()
		}
		opt := Options{MaxParagraphBytes: maxPara, HardMaxBytes: hardMax}
		checkInvariants(t, s, Split(s, opt), opt)
	})
}
