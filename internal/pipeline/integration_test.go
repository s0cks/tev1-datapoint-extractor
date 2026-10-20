//go:build integration

package pipeline

// Live tests against a real Ollama. Run with:
//
//	go test -tags integration ./internal/pipeline/ -run Live -v
//
// Environment: OLLAMA_URL (default http://localhost:11434), TEV1_MODEL (default tev1:0.8b).
// These tests are the only place the real /v1/systemone wire format is
// exercised; if they fail on parsing, capture fixtures with
// scripts/capture-fixtures.sh and adjust internal/tev1/types.go.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s0cks/tev1-rank/internal/config"
	"github.com/s0cks/tev1-rank/internal/features"
	"github.com/s0cks/tev1-rank/internal/rank"
	"github.com/s0cks/tev1-rank/internal/segment"
)

func liveConfig(t *testing.T) config.Config {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if v := os.Getenv("OLLAMA_URL"); v != "" {
		cfg.OllamaURL = v
	}
	if v := os.Getenv("TEV1_MODEL"); v != "" {
		cfg.Model = v
	}
	cfg.TimeoutSeconds = 120
	return cfg
}

func TestLivePreflightAndClassify(t *testing.T) {
	cfg := liveConfig(t)
	c := NewClient(cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ver, err := c.Preflight(ctx, cfg.Model)
	if err != nil {
		t.Skipf("Ollama not ready: %v", err)
	}
	t.Logf("ollama %s, model %s", ver, cfg.Model)

	// One datapoint (kind + importance requests) and one piece of prose (kind only).
	texts := []string{
		"Revenue grew 14% to $2.3 billion in 2024.",
		"The harbor smells of salt and diesel at dawn.",
	}
	segs := make([]segment.Segment, len(texts))
	for i, tx := range texts {
		segs[i] = segment.Segment{ID: fmt.Sprintf("live-%d", i), Text: tx, Hash: segment.HashText(tx), End: len(tx)}
	}
	cl := &Classifier{Cfg: cfg, Client: c}
	recs, err := cl.BuildRecords(ctx, segs, features.ComputeAll(segs, len(texts[0])+len(texts[1])))
	if err != nil {
		t.Fatalf("BuildRecords: %v", err)
	}
	for _, r := range recs {
		t.Logf("%q: kind=%s p_datapoint=%.2f importance=%.1f tokens=%d (peak %d) answers=%d",
			r.Text, r.Kind(), r.Signals.PDatapoint, r.Signals.Importance, r.InputTokens, r.PeakInputTokens, len(r.Answers))
		if r.Signals.PDatapoint < 0 || r.Signals.PDatapoint > 1 {
			t.Errorf("P(datapoint) out of range: %v", r.Signals.PDatapoint)
		}
	}
	st := cl.Stats()
	t.Logf("requests = %d, importance skipped = %d", st.ModelCalls, st.ImportanceSkipped)
	if st.ModelCalls != 2*len(texts)-st.ImportanceSkipped {
		t.Errorf("requests = %d, want %d", st.ModelCalls, 2*len(texts)-st.ImportanceSkipped)
	}
}

func TestLiveRunSample(t *testing.T) {
	cfg := liveConfig(t)
	c := NewClient(cfg, nil)
	if _, err := c.Preflight(context.Background(), cfg.Model); err != nil {
		t.Skipf("Ollama not ready: %v", err)
	}
	dir := t.TempDir()
	res, err := Run(context.Background(), Options{
		Config: cfg, InputPath: filepath.Join("..", "..", "testdata", "sample.txt"),
		OutDir: filepath.Join(dir, "out"), CacheDir: filepath.Join(dir, "cache"),
		Select: rank.SelectOptions{Top: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ModelErrors != 0 || res.Manifest.Counts.Selected != 5 {
		t.Errorf("counts: %+v", res.Manifest.Counts)
	}
	for _, i := range res.Selected {
		r := res.Records[i]
		t.Logf("#%d %.3f %-14s %s", *r.Rank, r.FinalScore, r.Kind(), r.Text[:min(70, len(r.Text))])
	}
}
