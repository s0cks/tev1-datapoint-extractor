// Package cache is a tiny on-disk cache of model answers.
//
// Key = sha256(model + question_set_hash + segment hash). Because the question
// set hash already covers the model and questions, any change to either (or
// to the segment text) is a miss; nothing is ever invalidated explicitly.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/s0cks/tev1-rank/internal/tev1"
)

const entryVersion = 1

type Cache struct {
	Dir string
}

type entry struct {
	Version int                    `json:"version"`
	Answers map[string]tev1.Answer `json:"answers"`
	Usage   tev1.Usage             `json:"usage,omitempty"` // tokens the original call used (0 if unknown)
}

// Key builds the cache key.
func Key(model, questionSetHash, segmentHash string) string {
	h := sha256.New()
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write([]byte(questionSetHash))
	h.Write([]byte{0})
	h.Write([]byte(segmentHash))
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Cache) path(key string) string {
	return filepath.Join(c.Dir, key[:2], key+".json")
}

// Get returns the cached answers. Unreadable or corrupt entries are misses.
func (c *Cache) Get(key string) (map[string]tev1.Answer, tev1.Usage, bool) {
	b, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil, tev1.Usage{}, false
	}

	var e entry
	if json.Unmarshal(b, &e) != nil || e.Version != entryVersion || len(e.Answers) == 0 {
		return nil, tev1.Usage{}, false
	}

	return e.Answers, e.Usage, true
}

// Put stores answers atomically (write to a temp file, then rename).
func (c *Cache) Put(key string, answers map[string]tev1.Answer, usage tev1.Usage) error {
	p := c.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}

	b, err := json.Marshal(entry{Version: entryVersion, Answers: answers, Usage: usage})
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), p)
}
