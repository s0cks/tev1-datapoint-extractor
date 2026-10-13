// Package segment implements the deterministic (model-free) segmenter.
//
// A document is split into paragraphs on blank lines. Paragraphs longer than
// Options.MaxParagraphBytes are further split into sentences, and any piece
// still longer than Options.HardMaxBytes is split at whitespace. Every
// segment carries byte offsets into the original text, and the invariant
//
//	text[seg.Start:seg.End] == seg.Text
//
// always holds. Offsets are byte offsets (not runes), so they are safe to use
// with Go string slicing and with most other languages' byte-indexed APIs.
package segment

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Segment is one unit of text to be classified.
type Segment struct {
	// ID is stable for a given (file, segmenter options): "seg-<ordinal>-<hash8>".
	ID string `json:"id"`
	// Start and End are byte offsets into the original input; End is exclusive.
	Start int `json:"start"`
	End   int `json:"end"`
	// Text is exactly text[Start:End].
	Text string `json:"text"`
	// Hash is "sha256:<hex>" of Text.
	Hash string `json:"hash"`
}

// Options controls splitting.
type Options struct {
	// MaxParagraphBytes: paragraphs longer than this are split into sentences.
	// Zero disables sentence splitting.
	MaxParagraphBytes int `json:"max_paragraph_bytes"`
	// HardMaxBytes: no segment will be longer than this (split at whitespace).
	// Zero disables the hard cap.
	HardMaxBytes int `json:"hard_max_bytes"`
}

// DefaultOptions returns the defaults used when no config overrides them.
func DefaultOptions() Options {
	return Options{MaxParagraphBytes: 600, HardMaxBytes: 1400}
}

// HashText returns "sha256:<hex>" for s.
func HashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Split segments text. It never panics; empty or whitespace-only input
// yields no segments.
func Split(text string, opt Options) []Segment {
	var ranges [][2]int
	for _, p := range paragraphs(text) {
		var parts [][2]int
		if opt.MaxParagraphBytes > 0 && p[1]-p[0] > opt.MaxParagraphBytes {
			parts = sentenceRanges(text, p[0], p[1])
		} else {
			parts = [][2]int{p}
		}
		for _, r := range parts {
			if opt.HardMaxBytes > 0 && r[1]-r[0] > opt.HardMaxBytes {
				ranges = append(ranges, hardSplit(text, r[0], r[1], opt.HardMaxBytes)...)
			} else {
				ranges = append(ranges, r)
			}
		}
	}
	segs := make([]Segment, 0, len(ranges))
	for i, r := range ranges {
		t := text[r[0]:r[1]]
		h := HashText(t)
		segs = append(segs, Segment{
			ID:    fmt.Sprintf("seg-%04d-%s", i+1, h[len("sha256:"):len("sha256:")+8]),
			Start: r[0], End: r[1], Text: t, Hash: h,
		})
	}
	return segs
}

// trim narrows [s,e) so it neither starts nor ends with whitespace.
func trim(text string, s, e int) (int, int) {
	for s < e {
		r, size := utf8.DecodeRuneInString(text[s:e])
		if !unicode.IsSpace(r) {
			break
		}
		s += size
	}
	for e > s {
		r, size := utf8.DecodeLastRuneInString(text[s:e])
		if !unicode.IsSpace(r) {
			break
		}
		e -= size
	}
	return s, e
}

// paragraphs returns trimmed byte ranges of blank-line separated paragraphs.
func paragraphs(text string) [][2]int {
	var out [][2]int
	start, end := -1, -1
	pos := 0
	for pos <= len(text) {
		nl := strings.IndexByte(text[pos:], '\n')
		var lineEnd, next int
		if nl < 0 {
			lineEnd, next = len(text), len(text)+1
		} else {
			lineEnd, next = pos+nl, pos+nl+1
		}
		ls, le := trim(text, pos, lineEnd)
		if ls == le { // blank line
			if start >= 0 {
				out = append(out, [2]int{start, end})
				start = -1
			}
		} else {
			if start < 0 {
				start = ls
			}
			end = le
		}
		pos = next
	}
	if start >= 0 {
		out = append(out, [2]int{start, end})
	}
	return out
}

var abbrevs = map[string]bool{
	"mr.": true, "mrs.": true, "ms.": true, "dr.": true, "prof.": true, "sr.": true,
	"jr.": true, "st.": true, "vs.": true, "etc.": true, "no.": true, "fig.": true,
	"inc.": true, "ltd.": true, "co.": true, "corp.": true, "approx.": true,
	"dept.": true, "est.": true, "vol.": true, "cf.": true, "al.": true,
}

func isAbbrev(word string) bool {
	w := strings.ToLower(strings.TrimLeft(word, "(\"'“‘["))
	if abbrevs[w] {
		return true
	}
	rs := []rune(w)
	if len(rs) == 2 && unicode.IsLetter(rs[0]) { // single initial: "J."
		return true
	}
	return strings.Count(w, ".") >= 2 && len(w) <= 6 // "u.s.", "e.g.", "i.e."
}

const closers = "\"')]}”’»"

func isTerminator(r rune) bool {
	switch r {
	case '.', '!', '?', '…', '。', '！', '？':
		return true
	}
	return false
}

// sentenceRanges splits [s,e) into trimmed sentence ranges.
func sentenceRanges(text string, s, e int) [][2]int {
	var out [][2]int
	add := func(a, b int) {
		a, b = trim(text, a, b)
		if a < b {
			out = append(out, [2]int{a, b})
		}
	}
	start, i := s, s
	for i < e {
		r, size := utf8.DecodeRuneInString(text[i:e])
		if !isTerminator(r) {
			i += size
			continue
		}
		j := i + size
		for j < e {
			r2, s2 := utf8.DecodeRuneInString(text[j:e])
			if isTerminator(r2) || strings.ContainsRune(closers, r2) {
				j += s2
			} else {
				break
			}
		}
		if sentenceBoundary(text, s, e, i, j, r) {
			add(start, j)
			k := j
			for k < e {
				r3, s3 := utf8.DecodeRuneInString(text[k:e])
				if !unicode.IsSpace(r3) {
					break
				}
				k += s3
			}
			start, i = k, k
			continue
		}
		i = j
	}
	add(start, e)
	return out
}

// sentenceBoundary decides whether the terminator run text[i:j] ends a sentence.
func sentenceBoundary(text string, s, e, i, j int, term rune) bool {
	if j >= e {
		return true
	}
	if term == '。' || term == '！' || term == '？' {
		return true
	}
	next, nsize := utf8.DecodeRuneInString(text[j:e])
	if !unicode.IsSpace(next) {
		return false
	}
	if term != '.' {
		return true
	}
	// Period: check abbreviations and a following lowercase word.
	ws := strings.LastIndexFunc(text[s:i], unicode.IsSpace)
	if ws < 0 {
		ws = s
	} else {
		ws += s + 1
	}
	_, tsize := utf8.DecodeRuneInString(text[i:j])
	if isAbbrev(text[ws : i+tsize]) {
		return false
	}
	k := j + nsize
	for k < e {
		r, size := utf8.DecodeRuneInString(text[k:e])
		if !unicode.IsSpace(r) {
			return !unicode.IsLower(r)
		}
		k += size
	}
	return true
}

// hardSplit breaks [s,e) into pieces of at most max bytes, preferring to cut
// at whitespace and never inside a UTF-8 sequence.
func hardSplit(text string, s, e, max int) [][2]int {
	var out [][2]int
	for e-s > max {
		cut := s + max
		for cut > s && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if idx := strings.LastIndexFunc(text[s:cut], unicode.IsSpace); idx > 0 {
			cut = s + idx
		}
		if cut <= s { // pathological: max smaller than one rune
			_, size := utf8.DecodeRuneInString(text[s:e])
			cut = s + size
		}
		a, b := trim(text, s, cut)
		if a < b {
			out = append(out, [2]int{a, b})
		}
		s = cut
		for s < e {
			r, size := utf8.DecodeRuneInString(text[s:e])
			if !unicode.IsSpace(r) {
				break
			}
			s += size
		}
	}
	if a, b := trim(text, s, e); a < b {
		out = append(out, [2]int{a, b})
	}
	return out
}
