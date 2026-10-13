package cache

import (
	"os"
	"strings"
	"testing"

	"github.com/s0cks/tev1-rank/internal/tev1"
)

func TestKeyChangesWithEachInput(t *testing.T) {
	base := Key("m", "q", "s")
	for name, k := range map[string]string{
		"model": Key("m2", "q", "s"), "questions": Key("m", "q2", "s"), "segment": Key("m", "q", "s2"),
		"boundary ambiguity": Key("mq", "", "s"),
	} {
		if k == base {
			t.Errorf("%s did not change the key", name)
		}
	}
	if Key("m", "q", "s") != base {
		t.Error("key must be deterministic")
	}
}

func TestRoundTripAndCorruption(t *testing.T) {
	c := &Cache{Dir: t.TempDir()}
	key := Key("m", "q", "s")
	if _, ok := c.Get(key); ok {
		t.Fatal("empty cache hit")
	}
	score := 3.2
	in := map[string]tev1.Answer{"importance": {Score: &score, Probabilities: map[string]float64{"3": 0.6}}}
	if err := c.Put(key, in); err != nil {
		t.Fatal(err)
	}
	out, ok := c.Get(key)
	if !ok || out["importance"].Score == nil || *out["importance"].Score != 3.2 || out["importance"].Probabilities["3"] != 0.6 {
		t.Fatalf("round trip failed: %+v ok=%v", out, ok)
	}
	// Corrupt the entry: must be a miss, not an error or a panic.
	if err := os.WriteFile(c.path(key), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get(key); ok {
		t.Error("corrupt entry should miss")
	}
	// No stray temp files after a successful Put.
	if err := c.Put(key, in); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(strings.TrimSuffix(c.path(key), key+".json"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}
