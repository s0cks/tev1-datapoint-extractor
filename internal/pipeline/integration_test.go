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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s0cks/tev1-rank/internal/config"
	"github.com/s0cks/tev1-rank/internal/rank"
	"github.com/s0cks/tev1-rank/internal/tev1"
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

func TestLivePreflightAndDecide(t *testing.T) {
	cfg := liveConfig(t)
	c := NewClient(cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ver, err := c.Preflight(ctx, cfg.Model)
	if err != nil {
		t.Skipf("Ollama not ready: %v", err)
	}
	t.Logf("ollama %s, model %s", ver, cfg.Model)

	req := tev1.Request{
		Model:     cfg.Model,
		State:     state{Segment: "Revenue grew 14% to $2.3 billion in 2024.", Features: "numbers=3"},
		Questions: cfg.QuestionSet(),
	}
	resp, err := c.Decide(ctx, req)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	sig, err := rank.ExtractSignals(cfg, resp.Answers)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("signals: %+v", sig)
	if sig.PDatapoint < 0 || sig.PDatapoint > 1 {
		t.Errorf("P(datapoint) out of range: %v", sig.PDatapoint)
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
