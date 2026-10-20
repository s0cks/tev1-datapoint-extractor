// Package input reads the pre-segmented, pre-annotated document produced by
// the upstream text-preparation tool.
//
// tev1-rank does no segmenting or text analysis of its own: the upstream tool
// splits the source document into blocks (headings, paragraphs, block quotes,
// list items), strips markup, and annotates each block with POS-tagged tokens
// and named entities. This package parses that file (see docs/input-format.md),
// validates what the rest of the pipeline relies on, and derives a few cheap
// counts from the annotations for ranking.
package input

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"unicode"
)

// TypeSegment is the value of the "type" field of an entry we understand.
// Entries of any other type are skipped, so the upstream tool can add new
// entry types without breaking this one.
const TypeSegment = "segment"

// Document is the top level of the upstream file.
type Document struct {
	// Source is the name of the document the segments were cut from (e.g. main.md).
	Source string `json:"source"`
	// Format is the source document's format (e.g. "md").
	Format   string    `json:"format"`
	Segments []Segment `json:"-"`
	// Skipped counts entries that were ignored: unknown "type", or no text.
	Skipped int `json:"-"`
}

// Range is a half-open [Start, End) interval. For a segment it is a byte
// range in the SOURCE document, which is not part of the upstream file; for a
// token or entity it is a byte range within the segment's plain text.
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Token is one POS-tagged token.
type Token struct {
	T     string `json:"t"`
	POS   string `json:"pos"`
	Stem  string `json:"stem,omitempty"`
	Range Range  `json:"range"`
}

// Entity is one named-entity span.
type Entity struct {
	Text  string `json:"text"`
	Label string `json:"label"`
	Range Range  `json:"range"`
}

// Segment is one block of the source document.
type Segment struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	// Block is the ordinal of the source block (a block may yield several segments).
	Block int `json:"block"`
	// Kind is the structural kind: heading, paragraph, blockquote, list_item, ...
	// It is not the tev1 datapoint kind.
	Kind    string   `json:"kind"`
	Level   int      `json:"level,omitempty"`
	Section []string `json:"section"`
	// Range locates the segment in the source document (byte offsets).
	Range Range `json:"range"`
	// Text is the segment as it appears in the source (markup included).
	Text string `json:"text"`
	// Plain is Text with markup stripped and whitespace collapsed. It is what
	// the model sees and what the outputs report.
	Plain string `json:"plain"`
	// TTS is a speech-friendly rewrite of Plain. It is carried by the upstream
	// file but not used here.
	TTS      string   `json:"tts"`
	Tokens   []Token  `json:"tokens"`
	Entities []Entity `json:"entities"`
	// TokenCount is the upstream tool's own count. It does not always equal
	// len(Tokens) (the two use different tokenizers), so nothing here relies on it.
	TokenCount int `json:"token_count"`

	// Hash is the sha256 of Plain ("sha256:<hex>"). Filled in by Parse.
	Hash string `json:"-"`
}

// Load reads and parses the file at path.
func Load(path string) (*Document, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	doc, err := Parse(data)
	if err != nil {
		return nil, data, fmt.Errorf("%s: %w", path, err)
	}
	return doc, data, nil
}

// Parse parses the upstream JSON. Unknown fields are ignored (the file belongs
// to another tool), but what this tool depends on is checked: every segment
// needs a unique id and a sane range. Segments with no text are skipped.
func Parse(data []byte) (*Document, error) {
	var raw struct {
		Source   string             `json:"source"`
		Format   string             `json:"format"`
		Segments *[]json.RawMessage `json:"segments"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("invalid JSON: unexpected data after the top-level object")
	}
	if raw.Segments == nil {
		return nil, fmt.Errorf(`missing "segments" array (is this the output of the text-preparation tool?)`)
	}

	doc := &Document{Source: raw.Source, Format: raw.Format, Segments: make([]Segment, 0, len(*raw.Segments))}
	ids := map[string]int{}
	for i, msg := range *raw.Segments {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msg, &head); err != nil {
			return nil, fmt.Errorf("segments[%d]: %w", i, err)
		}
		if head.Type != TypeSegment {
			doc.Skipped++
			continue
		}
		var s Segment
		if err := json.Unmarshal(msg, &s); err != nil {
			return nil, fmt.Errorf("segments[%d]: %w", i, err)
		}
		if s.ID == "" {
			return nil, fmt.Errorf("segments[%d]: missing id", i)
		}
		if prev, dup := ids[s.ID]; dup {
			return nil, fmt.Errorf("segments[%d]: duplicate id %q (first seen at segments[%d])", i, s.ID, prev)
		}
		ids[s.ID] = i
		if s.Range.Start < 0 || s.Range.End < s.Range.Start {
			return nil, fmt.Errorf("segment %q: invalid range [%d, %d)", s.ID, s.Range.Start, s.Range.End)
		}
		if isBlank(s.Plain) {
			doc.Skipped++
			continue
		}
		s.Hash = HashText(s.Plain)
		doc.Segments = append(doc.Segments, s)
	}
	return doc, nil
}

func isBlank(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) && r != '\u200b' && r != '\u200c' && r != '\u200d' && r != '\u2060' && r != '\ufeff' {
			return false
		}
	}
	return true
}

// HashText returns "sha256:<hex>" for s.
func HashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Features are cheap counts read straight off the upstream annotations; no
// text analysis happens here.
type Features struct {
	// Numerics is the number of tokens the upstream tagger marked CD (cardinal number).
	Numerics int `json:"numerics"`
	// Entities is the number of named-entity spans the upstream tool found.
	Entities int `json:"entities"`
	// Words is the number of tokens containing a letter or digit (punctuation excluded).
	Words int `json:"words"`
	// Position is the segment's start in the source document as a fraction of
	// the document's extent, in [0,1).
	Position float64 `json:"position"`
}

// Features returns the features of the i-th segment. docEnd is the end of the
// document's extent (see Document.Features).
func (s Segment) Features(docEnd int) Features {
	f := Features{Entities: len(s.Entities)}
	for _, t := range s.Tokens {
		if t.POS == "CD" {
			f.Numerics++
		}
		for _, r := range t.T {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				f.Words++
				break
			}
		}
	}
	if docEnd > 0 {
		f.Position = float64(s.Range.Start) / float64(docEnd)
	}
	return f
}

// Features returns the features of every segment, in order.
func (d *Document) Features() []Features {
	end := 0
	for _, s := range d.Segments {
		if s.Range.End > end {
			end = s.Range.End
		}
	}
	out := make([]Features, len(d.Segments))
	for i, s := range d.Segments {
		out[i] = s.Features(end)
	}
	return out
}

// Summary renders the count features compactly for use in model state.
// Position is deliberately excluded: it depends on the rest of the document.
func (f Features) Summary() string {
	return fmt.Sprintf("numerics=%d; named_entities=%d; words=%d", f.Numerics, f.Entities, f.Words)
}
