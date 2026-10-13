// Package features computes deterministic per-segment features.
//
// Count features (Numbers, Percentages, Currency, Years, Dates, Entities) are
// local to a segment's text. Position and TermWeight depend on the document.
// Counts are independent regexes and may overlap (for example "14%" counts as
// both a number and a percentage; "2024" counts as both a number and a year).
package features

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/s0cks/tev1-rank/internal/segment"
)

// Features is the deterministic feature vector of one segment.
type Features struct {
	Numbers     int     `json:"numbers"`
	Percentages int     `json:"percentages"`
	Currency    int     `json:"currency"`
	Years       int     `json:"years"`
	Dates       int     `json:"dates"`
	Entities    int     `json:"entities"`
	Length      int     `json:"length"` // characters (runes)
	Words       int     `json:"words"`
	Position    float64 `json:"position"`    // start offset / document length, in [0,1)
	TermWeight  float64 `json:"term_weight"` // salient-term weight, normalised to [0,1]
}

var (
	reNumber   = regexp.MustCompile(`\b\d[\d,]*(?:\.\d+)?\b`)
	rePercent  = regexp.MustCompile(`(?i)\d[\d,]*(?:\.\d+)?\s?(?:%|percent\b|per cent\b|percentage points?\b)`)
	reCurrency = regexp.MustCompile(`(?i)(?:[$€£¥]\s?\d[\d,]*(?:\.\d+)?(?:\s?(?:thousand|million|billion|trillion|[kmb])\b)?)|(?:\b\d[\d,]*(?:\.\d+)?\s?(?:usd|eur|gbp|dollars|euros|pounds)\b)`)
	reYear     = regexp.MustCompile(`\b(?:1[5-9]\d\d|20\d\d)\b`)
	reDate     = regexp.MustCompile(`(?i)\b(?:jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\.?\s+\d{1,2}(?:st|nd|rd|th)?(?:,?\s*\d{4})?\b|\b\d{4}-\d{2}-\d{2}\b|\b\d{1,2}/\d{1,2}/\d{2,4}\b`)
	reWord     = regexp.MustCompile(`[\p{L}][\p{L}'’-]{2,}`)
)

// Local computes the document-independent features of text.
func Local(text string) Features {
	f := Features{
		Numbers:     len(reNumber.FindAllString(text, -1)),
		Percentages: len(rePercent.FindAllString(text, -1)),
		Currency:    len(reCurrency.FindAllString(text, -1)),
		Years:       len(reYear.FindAllString(text, -1)),
		Dates:       len(reDate.FindAllString(text, -1)),
		Entities:    countEntities(text),
		Length:      utf8.RuneCountInString(text),
		Words:       len(strings.Fields(text)),
	}
	return f
}

// Summary renders the local count features compactly for use in model state.
// It deliberately excludes Position and TermWeight so cached model answers
// stay valid when other parts of the document change.
func (f Features) Summary() string {
	return fmt.Sprintf("numbers=%d; percentages=%d; currency=%d; years=%d; dates=%d; named_entities=%d; words=%d",
		f.Numbers, f.Percentages, f.Currency, f.Years, f.Dates, f.Entities, f.Words)
}

func countEntities(text string) int {
	fields := strings.Fields(text)
	count, inRun := 0, false
	for i, w := range fields {
		core := strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		first, _ := utf8.DecodeRuneInString(core)
		cap := core != "" && unicode.IsUpper(first) && (utf8.RuneCountInString(core) > 1)
		if cap && i > 0 {
			prev := fields[i-1]
			if last, _ := utf8.DecodeLastRuneInString(prev); strings.ContainsRune(".!?:", last) && !inRun {
				cap = false // sentence-initial capital
			}
		} else if cap && i == 0 {
			cap = false
		}
		if cap {
			if !inRun {
				count++
			}
			inRun = true
		} else {
			inRun = false
		}
	}
	return count
}

var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`the and for that with this from are was were has have had but not you all can her his its our their they them been being will would could should about into over than then there these those which what when where who whom while also more most such only other some any each both very just may might must shall does did doing done out off our own same too under until upon your yours how why per via`) {
		m[w] = true
	}
	return m
}()

func terms(text string) []string {
	var out []string
	for _, w := range reWord.FindAllString(strings.ToLower(text), -1) {
		if !stopwords[w] {
			out = append(out, w)
		}
	}
	return out
}

const topTerms = 5

// ComputeAll computes full features for every segment of a document of
// docLen bytes. Term weights are TF-IDF-like: a term's weight is
// tf * ln(1 + N/df), where tf is its count in the whole document, N the number
// of segments and df the number of segments containing it. Frequent terms
// therefore weigh more, and terms concentrated in fewer segments weigh a bit
// more still. A segment's raw score is the
// mean weight of its top terms; scores are then normalised by the document
// maximum so TermWeight is in [0,1].
func ComputeAll(segs []segment.Segment, docLen int) []Features {
	n := len(segs)
	out := make([]Features, n)
	segTerms := make([]map[string]bool, n)
	tf := map[string]int{}
	df := map[string]int{}
	for i, s := range segs {
		out[i] = Local(s.Text)
		set := map[string]bool{}
		for _, t := range terms(s.Text) {
			tf[t]++
			set[t] = true
		}
		for t := range set {
			df[t]++
		}
		segTerms[i] = set
		if docLen > 0 {
			out[i].Position = float64(s.Start) / float64(docLen)
		}
	}
	weight := func(t string) float64 {
		return float64(tf[t]) * math.Log(1+float64(n)/float64(df[t]))
	}
	raw := make([]float64, n)
	maxRaw := 0.0
	for i := range segs {
		ws := make([]float64, 0, len(segTerms[i]))
		for t := range segTerms[i] {
			ws = append(ws, weight(t))
		}
		sort.Sort(sort.Reverse(sort.Float64Slice(ws)))
		if len(ws) > topTerms {
			ws = ws[:topTerms]
		}
		sum := 0.0
		for _, w := range ws {
			sum += w
		}
		if len(ws) > 0 {
			raw[i] = sum / float64(len(ws))
		}
		if raw[i] > maxRaw {
			maxRaw = raw[i]
		}
	}
	if maxRaw > 0 {
		for i := range out {
			out[i].TermWeight = raw[i] / maxRaw
		}
	}
	return out
}
