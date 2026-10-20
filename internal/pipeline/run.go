package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/s0cks/tev1-rank/internal/cache"
	"github.com/s0cks/tev1-rank/internal/config"
	"github.com/s0cks/tev1-rank/internal/features"
	"github.com/s0cks/tev1-rank/internal/rank"
	"github.com/s0cks/tev1-rank/internal/tev1"
)

// Schema identifiers written into the outputs.
const (
	TopSchemaVersion = "tev1-rank/top/v2"
	RunSchemaVersion = "tev1-rank/run/v1"
)

// Options configure Run.
type Options struct {
	Config        config.Config
	InputPath     string
	OutDir        string
	NoModel       bool
	NoCache       bool
	CacheDir      string // default: <user cache dir>/tev1-rank
	SkipPreflight bool
	Select        rank.SelectOptions
	Logger        *slog.Logger
	Client        *tev1.Client // optional injected client (tests)
	Now           func() time.Time
}

// Result is returned by Run.
type Result struct {
	Manifest Manifest
	Records  []Record
	Selected []int
	// ModelErrors is the number of segments that fell back to features.
	ModelErrors int
}

// Manifest is run.json.
type Manifest struct {
	SchemaVersion string        `json:"schema_version"`
	Tool          ToolInfo      `json:"tool"`
	StartedAt     time.Time     `json:"started_at"`
	FinishedAt    time.Time     `json:"finished_at"`
	Mode          string        `json:"mode"` // "model" or "no-model"
	Input         InputInfo     `json:"input"`
	Model         *ModelInfo    `json:"model"` // null in no-model mode
	Config        config.Config `json:"config"`
	Selection     SelectionInfo `json:"selection"`
	Counts        Counts        `json:"counts"`
	TimingsMS     Timings       `json:"timings_ms"`
	Outputs       Outputs       `json:"outputs"`
}

type ToolInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
type InputInfo struct {
	Path  string `json:"path"`
	Hash  string `json:"hash"`
	Bytes int    `json:"bytes"`
}
type ModelInfo struct {
	Name            string `json:"name"`
	OllamaVersion   string `json:"ollama_version,omitempty"`
	QuestionSetHash string `json:"question_set_hash"`
	StateFormat     string `json:"state_format"`
}
type SelectionInfo struct {
	Top        int     `json:"top"`
	MinScore   float64 `json:"min_score"`
	MaxPerKind int     `json:"max_per_kind"`
}
type Counts struct {
	Segments          int `json:"segments"`
	Duplicates        int `json:"duplicates"`
	Ranked            int `json:"ranked"`
	Selected          int `json:"selected"`
	NeedsReview       int `json:"needs_review"`
	ModelCalls        int `json:"model_calls"`
	ImportanceSkipped int `json:"importance_skipped"` // segments classified "other"; they get no importance request
	CacheHits         int `json:"cache_hits"`
	CacheMisses       int `json:"cache_misses"`
	ModelErrors       int `json:"model_errors"`
	ContextExceeded   int `json:"context_exceeded"` // segments whose reported input_tokens exceeded state.context_tokens
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}
type Timings struct {
	Segment   int64 `json:"segment"`
	Preflight int64 `json:"preflight"`
	Classify  int64 `json:"classify"`
	Total     int64 `json:"total"`
	ModelTime int64 `json:"model_calls_sum"`
}
type Outputs struct {
	Scored string `json:"scored"`
	Top    string `json:"top"`
}

// NewClient builds the Ollama client from the config.
func NewClient(cfg config.Config, log *slog.Logger) *tev1.Client {
	return &tev1.Client{
		BaseURL: cfg.OllamaURL,
		HTTP:    &http.Client{},
		Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
		Retries: cfg.Retries,
		Backoff: time.Duration(cfg.BackoffMillis) * time.Millisecond,
		Logger:  log,
	}
}

// NewCache returns the on-disk cache, or nil when caching is disabled.
func NewCache(noCache bool, dir string) (*cache.Cache, error) {
	if noCache {
		return nil, nil
	}
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("locating cache dir (pass --cache-dir or --no-cache): %w", err)
		}
		dir = filepath.Join(base, "tev1-rank")
	}
	return &cache.Cache{Dir: dir}, nil
}

// Run executes the full stage: segment → classify → rank → write outputs.
func Run(ctx context.Context, opt Options) (*Result, error) {
	log := opt.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	cfg := opt.Config
	started := now()

	data, err := os.ReadFile(opt.InputPath)
	if err != nil {
		return nil, fmt.Errorf("reading input: %w", err)
	}
	text := string(data)

	t0 := time.Now()
	segs, feats := Analyze(text, cfg.Segmenter)
	tSegment := time.Since(t0)
	log.Info("segmented", "input", opt.InputPath, "segments", len(segs))
	if len(segs) == 0 {
		log.Warn("input produced no segments")
	}

	client := opt.Client
	if client == nil {
		client = NewClient(cfg, log)
	}
	var ollamaVersion string
	var tPreflight time.Duration
	if !opt.NoModel && !opt.SkipPreflight {
		t1 := time.Now()
		ollamaVersion, err = client.Preflight(ctx, cfg.Model)
		tPreflight = time.Since(t1)
		if err != nil {
			return nil, fmt.Errorf("preflight: %w", err)
		}
		log.Info("preflight ok", "ollama", ollamaVersion, "model", cfg.Model)
	}

	cc, err := NewCache(opt.NoCache || opt.NoModel, opt.CacheDir)
	if err != nil {
		return nil, err
	}
	cl := &Classifier{Cfg: cfg, Client: client, Cache: cc, Log: log, NoModel: opt.NoModel}
	t2 := time.Now()
	recs, err := cl.BuildRecords(ctx, segs, feats)
	tClassify := time.Since(t2)
	if err != nil {
		return nil, err
	}
	chosen := Finalize(recs, opt.Select)
	stats := cl.Stats()

	m := Manifest{
		SchemaVersion: RunSchemaVersion,
		Tool:          ToolInfo{Name: "tev1-rank", Version: Version},
		StartedAt:     started.UTC(),
		Mode:          "model",
		Input:         InputInfo{Path: opt.InputPath, Hash: hashBytes(data), Bytes: len(data)},
		Config:        cfg,
		Selection:     SelectionInfo{Top: opt.Select.Top, MinScore: opt.Select.MinScore, MaxPerKind: opt.Select.MaxPerKind},
		Outputs:       Outputs{Scored: "scored.jsonl", Top: "top.json"},
	}
	if opt.NoModel {
		m.Mode = "no-model"
	} else {
		m.Model = &ModelInfo{Name: cfg.Model, OllamaVersion: ollamaVersion, QuestionSetHash: cfg.QuestionSetHash(), StateFormat: cfg.StateFormat()}
	}
	m.Counts = Counts{
		Segments: len(recs), Selected: len(chosen), ModelCalls: stats.ModelCalls, ImportanceSkipped: stats.ImportanceSkipped,
		CacheHits: stats.CacheHits, CacheMisses: stats.CacheMisses, ModelErrors: stats.Errors,
		ContextExceeded: stats.ContextExceeded, InputTokens: stats.InputTokens, OutputTokens: stats.OutputTokens,
	}
	for _, r := range recs {
		if r.Rank == nil {
			m.Counts.Duplicates++
		} else {
			m.Counts.Ranked++
		}
		if r.NeedsReview {
			m.Counts.NeedsReview++
		}
	}
	m.FinishedAt = now().UTC()
	m.TimingsMS = Timings{
		Segment: tSegment.Milliseconds(), Preflight: tPreflight.Milliseconds(), Classify: tClassify.Milliseconds(),
		Total: time.Since(t0).Milliseconds(), ModelTime: stats.ModelTime.Milliseconds(),
	}

	if err := writeOutputs(opt.OutDir, m, recs, chosen); err != nil {
		return nil, err
	}
	log.Info("wrote outputs", "dir", opt.OutDir, "selected", len(chosen), "needs_review", m.Counts.NeedsReview,
		"model_calls", stats.ModelCalls, "cache_hits", stats.CacheHits, "model_errors", stats.Errors)
	return &Result{Manifest: m, Records: recs, Selected: chosen, ModelErrors: stats.Errors}, nil
}

// --- outputs ---------------------------------------------------------------

// TopFile is top.json: the shape stage 2 consumes. See docs/output-format.md
// and schema/top.schema.json.
type TopFile struct {
	SchemaVersion   string        `json:"schema_version"`
	Mode            string        `json:"mode"`
	Source          TopSource     `json:"source"`
	Model           *string       `json:"model"`
	QuestionSetHash *string       `json:"question_set_hash"`
	Selection       SelectionInfo `json:"selection"`
	Count           int           `json:"count"`
	Items           []TopItem     `json:"items"`
}

type TopSource struct {
	File string `json:"file"`
	Hash string `json:"hash"`
}

type TopItem struct {
	Rank          int               `json:"rank"`
	ID            string            `json:"id"`
	Hash          string            `json:"hash"`
	Start         int               `json:"start"`
	End           int               `json:"end"`
	Text          string            `json:"text"`
	Kind          *string           `json:"kind"`
	FinalScore    float64           `json:"final_score"`
	NeedsReview   bool              `json:"needs_review"`
	ReviewReasons []string          `json:"review_reasons"`
	Features      features.Features `json:"features"`
	Model         *TopModel         `json:"model"`
}

// TopModel is a compact view of the model's answers.
type TopModel struct {
	PDatapoint float64            `json:"p_datapoint"`
	Importance float64            `json:"importance"`
	KindProbs  map[string]float64 `json:"kind_probs"`
	Confidence *float64           `json:"confidence"`
}

// BuildTop assembles top.json from finalized records.
func BuildTop(m Manifest, recs []Record, chosen []int) TopFile {
	t := TopFile{
		SchemaVersion: TopSchemaVersion, Mode: m.Mode,
		Source:    TopSource{File: filepath.Base(m.Input.Path), Hash: m.Input.Hash},
		Selection: m.Selection, Count: len(chosen), Items: []TopItem{},
	}
	if m.Model != nil {
		name, qs := m.Model.Name, m.Model.QuestionSetHash
		t.Model, t.QuestionSetHash = &name, &qs
	}
	for _, i := range chosen {
		r := recs[i]
		item := TopItem{
			Rank: *r.Rank, ID: r.ID, Hash: r.Hash, Start: r.Start, End: r.End, Text: r.Text,
			FinalScore: r.FinalScore, NeedsReview: r.NeedsReview, Features: r.Features,
			ReviewReasons: append([]string{}, r.ReviewReasons...),
		}
		if r.Signals != nil {
			k := r.Signals.Kind
			item.Kind = &k
			item.Model = &TopModel{
				PDatapoint: round6(r.Signals.PDatapoint),
				Importance: round6(r.Signals.Importance), KindProbs: r.Signals.KindProbs, Confidence: r.Signals.Confidence,
			}
		}
		t.Items = append(t.Items, item)
	}
	return t
}

func writeOutputs(dir string, m Manifest, recs []Record, chosen []int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating out dir: %w", err)
	}
	if err := atomicWrite(filepath.Join(dir, "scored.jsonl"), func(w *bufio.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		for _, r := range recs {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "top.json"), BuildTop(m, recs, chosen)); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "run.json"), m)
}

func writeJSON(path string, v any) error {
	return atomicWrite(path, func(w *bufio.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	})
}

func atomicWrite(path string, fill func(*bufio.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriter(tmp)
	if err := fill(w); err != nil {
		tmp.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
