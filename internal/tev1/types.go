// Package tev1 is a small client for Ollama's POST /v1/systemone endpoint
// (TypeSafe "System One" wire format), as served for decision models such as
// tev1. See docs/tev1-api.md for what has and has not been verified.
package tev1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Question types accepted by /v1/systemone.
const (
	TypeChoice = "choice"
	TypeNoul   = "noul" // yes/no style: returns P(true)
	TypeScore  = "score"
)

// MaxRequestBytes is the documented request body limit (64 KiB).
const MaxRequestBytes = 64 * 1024

// Question is the wire form of one question.
type Question struct {
	Type         string          `json:"type"`
	Instructions string          `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

// NamedQuestion is a Question plus its name (the key in the request object).
// This is also the config-file form: questions are an ordered array.
type NamedQuestion struct {
	Name         string          `json:"name"`
	Type         string          `json:"type"`
	Instructions string          `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

// Options returns the option names of a choice question (nil when criteria is
// not an object). Order follows the criteria object as written.
func (q NamedQuestion) Options() []string {
	return objectKeys(q.Criteria)
}

// Levels returns the number of levels of a score question (0 if unknown).
func (q NamedQuestion) Levels() int {
	var arr []json.RawMessage
	if json.Unmarshal(q.Criteria, &arr) == nil {
		return len(arr)
	}
	return 0
}

// QuestionSet is an ordered set of questions. It marshals as a JSON object
// whose key order is the slice order, because the API documents that answers
// come back "in the same order" as the questions.
type QuestionSet []NamedQuestion

func (qs QuestionSet) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, q := range qs {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(q.Name)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(Question{Type: q.Type, Instructions: q.Instructions, Criteria: q.Criteria})
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func (qs *QuestionSet) UnmarshalJSON(data []byte) error {
	var out QuestionSet
	var m map[string]Question
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	for _, name := range objectKeys(data) {
		q := m[name]
		out = append(out, NamedQuestion{Name: name, Type: q.Type, Instructions: q.Instructions, Criteria: q.Criteria})
	}
	*qs = out
	return nil
}

// objectKeys returns the top-level keys of a JSON object in document order.
func objectKeys(raw json.RawMessage) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil
	}
	var keys []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return keys
		}
		k, _ := kt.(string)
		keys = append(keys, k)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return keys
		}
	}
	return keys
}

// Request is the body of POST /v1/systemone.
type Request struct {
	Model     string      `json:"model"`
	State     any         `json:"state"`
	Questions QuestionSet `json:"questions"`
	KeepAlive string      `json:"keep_alive,omitempty"`
}

// Answer is one answer. Only the fields relevant to the question type are set.
type Answer struct {
	Type          string
	Choice        string
	Noul          *float64
	Score         *float64
	Probabilities map[string]float64 // option name (choice) or level index "0".."n" (score)
	Legend        json.RawMessage    // informational; shape varies (object in live responses), kept verbatim
	Confidence    *float64
	name          string // set when the server labels array entries
}

func (a *Answer) UnmarshalJSON(data []byte) error {
	var w struct {
		Type          string          `json:"type"`
		Name          string          `json:"name"`
		ID            string          `json:"id"`
		Choice        *string         `json:"choice"`
		Noul          json.RawMessage `json:"noul"`
		Score         json.RawMessage `json:"score"`
		Probabilities json.RawMessage `json:"probabilities"`
		Legend        json.RawMessage `json:"legend"`
		Confidence    *float64        `json:"confidence"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*a = Answer{Type: w.Type, Confidence: w.Confidence, name: w.Name}
	if len(w.Legend) > 0 && string(w.Legend) != "null" {
		a.Legend = w.Legend
	}
	if a.name == "" {
		a.name = w.ID
	}
	if w.Choice != nil {
		a.Choice = *w.Choice
	}
	var err error
	if a.Noul, err = optFloat(w.Noul, "noul"); err != nil {
		return err
	}
	if a.Score, err = optFloat(w.Score, "score"); err != nil {
		return err
	}
	if len(w.Probabilities) > 0 && string(w.Probabilities) != "null" {
		var m map[string]float64
		if err := json.Unmarshal(w.Probabilities, &m); err == nil {
			a.Probabilities = m
		} else {
			var arr []float64
			if err2 := json.Unmarshal(w.Probabilities, &arr); err2 != nil {
				return fmt.Errorf("probabilities: neither object nor array of numbers")
			}
			a.Probabilities = make(map[string]float64, len(arr))
			for i, p := range arr {
				a.Probabilities[fmt.Sprint(i)] = p
			}
		}
	}
	return nil
}

func optFloat(raw json.RawMessage, field string) (*float64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: expected a number, got %s", field, strings.TrimSpace(string(raw)))
	}
	return &f, nil
}

// MarshalJSON writes the answer back out in a stable form (used by the cache
// and the scored.jsonl output).
func (a Answer) MarshalJSON() ([]byte, error) {
	type out struct {
		Type          string             `json:"type,omitempty"`
		Choice        string             `json:"choice,omitempty"`
		Noul          *float64           `json:"noul,omitempty"`
		Score         *float64           `json:"score,omitempty"`
		Probabilities map[string]float64 `json:"probabilities,omitempty"`
		Legend        json.RawMessage    `json:"legend,omitempty"`
		Confidence    *float64           `json:"confidence,omitempty"`
	}
	return json.Marshal(out{a.Type, a.Choice, a.Noul, a.Score, a.Probabilities, a.Legend, a.Confidence})
}

// Usage is the token accounting the server reports ("usage" in the response).
//
// PeakInputTokens is never sent by the server: when a segment needs several
// requests, the pipeline sums the tokens and records here the largest single
// request, which is what has to fit in the context window.
type Usage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	PeakInputTokens int `json:"peak_input_tokens,omitempty"`
}

// Response is a decoded and validated /v1/systemone response.
type Response struct {
	Model   string
	Answers map[string]Answer
	Usage   Usage
}

// ErrMalformed marks a response that parsed as HTTP 200 but does not match the
// request (missing answers, out-of-range probabilities, unknown options...).
// Malformed responses are never retried.
var ErrMalformed = errors.New("tev1: malformed response")

// APIError is a non-2xx response from Ollama.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	b := strings.TrimSpace(e.Body)
	if len(b) > 300 {
		b = b[:300] + "…"
	}
	return fmt.Sprintf("ollama returned HTTP %d: %s", e.Status, b)
}

// ParseResponse decodes body and validates it against req.
//
// The documented shape is {"answers": {<name>: {...}}}. As a hedge against
// the unverified wire format it also accepts "answers" as an array: entries
// that carry a "name"/"id" are matched by it, otherwise by position in
// request order.
func ParseResponse(body []byte, req Request) (*Response, error) {
	var top struct {
		Model   string          `json:"model"`
		Answers json.RawMessage `json:"answers"`
		Usage   Usage           `json:"usage"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("%w: invalid JSON: %v", ErrMalformed, err)
	}
	trimmed := bytes.TrimSpace(top.Answers)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, fmt.Errorf("%w: no \"answers\" field", ErrMalformed)
	}
	resp := &Response{Model: top.Model, Answers: map[string]Answer{}, Usage: top.Usage}
	switch trimmed[0] {
	case '{':
		if err := json.Unmarshal(trimmed, &resp.Answers); err != nil {
			return nil, fmt.Errorf("%w: answers: %v", ErrMalformed, err)
		}
	case '[':
		var arr []Answer
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, fmt.Errorf("%w: answers: %v", ErrMalformed, err)
		}
		if len(arr) != len(req.Questions) {
			return nil, fmt.Errorf("%w: got %d answers for %d questions", ErrMalformed, len(arr), len(req.Questions))
		}
		for i, a := range arr {
			name := a.name
			if name == "" {
				name = req.Questions[i].Name
			}
			resp.Answers[name] = a
		}
	default:
		return nil, fmt.Errorf("%w: \"answers\" is neither object nor array", ErrMalformed)
	}
	if err := validate(resp, req); err != nil {
		return nil, err
	}
	return resp, nil
}

func validate(resp *Response, req Request) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrMalformed}, args...)...)
	}
	for _, q := range req.Questions {
		a, ok := resp.Answers[q.Name]
		if !ok {
			return bad("no answer for question %q", q.Name)
		}
		for opt, p := range a.Probabilities {
			if p < -1e-6 || p > 1+1e-6 {
				return bad("question %q: probability %q=%v out of range", q.Name, opt, p)
			}
		}
		switch q.Type {
		case TypeChoice:
			if a.Choice == "" {
				return bad("question %q: missing choice", q.Name)
			}
			if opts := q.Options(); len(opts) > 0 && !contains(opts, a.Choice) {
				return bad("question %q: choice %q is not a declared option", q.Name, a.Choice)
			}
		case TypeNoul:
			if a.Noul == nil {
				return bad("question %q: missing noul", q.Name)
			}
			if *a.Noul < -1e-6 || *a.Noul > 1+1e-6 {
				return bad("question %q: noul %v out of range", q.Name, *a.Noul)
			}
		case TypeScore:
			if a.Score == nil {
				return bad("question %q: missing score", q.Name)
			}
			if n := q.Levels(); n > 0 && (*a.Score < -1e-6 || *a.Score > float64(n-1)+1e-6) {
				return bad("question %q: score %v outside 0..%d", q.Name, *a.Score, n-1)
			}
		default:
			return bad("question %q has unsupported type %q", q.Name, q.Type)
		}
	}
	return nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
