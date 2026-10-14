// Package pipeline wires the segmenter, tev1 classifier, cache and ranking
// together and writes the stage-1 outputs.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/s0cks/tev1-rank/internal/cache"
	"github.com/s0cks/tev1-rank/internal/config"
	"github.com/s0cks/tev1-rank/internal/features"
	"github.com/s0cks/tev1-rank/internal/rank"
	"github.com/s0cks/tev1-rank/internal/segment"
	"github.com/s0cks/tev1-rank/internal/tev1"
)

// Version is overridden at build time with
// -ldflags "-X github.com/s0cks/tev1-rank/internal/pipeline.Version=<v>".
var Version = "dev"

// Scoring modes recorded per segment.
const (
	ScoredByModel    = "model"
	ScoredByFeatures = "features"          // --no-model
	ScoredByFallback = "features_fallback" // model call failed for this segment
)

// maxConsecutiveFailures aborts a run whose model calls keep failing from the
// start, instead of retrying every remaining segment.
const maxConsecutiveFailures = 5

// Record is one line of scored.jsonl.
type Record struct {
	ID       string            `json:"id"`
	Start    int               `json:"start"`
	End      int               `json:"end"`
	Hash     string            `json:"hash"`
	Text     string            `json:"text"`
	Features features.Features `json:"features"`

	ScoredBy       string                 `json:"scored_by"`
	Answers        map[string]tev1.Answer `json:"answers,omitempty"`
	Signals        *rank.Signals          `json:"signals,omitempty"`
	Cached         bool                   `json:"cached"`
	StateTruncated bool                   `json:"state_truncated,omitempty"`
	// InputTokens/OutputTokens are what the server reported for the call that
	// produced these answers (kept in the cache, so cached hits report them too).
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	// ContextExceeded: InputTokens is above state.context_tokens, so the prompt
	// may have been truncated by the server and the answers are unreliable.
	ContextExceeded bool   `json:"context_exceeded,omitempty"`
	ModelError      string `json:"model_error,omitempty"`

	FeatureBonus  float64  `json:"feature_bonus"`
	FinalScore    float64  `json:"final_score"`
	Rank          *int     `json:"rank"` // null for removed near-duplicates
	DuplicateOf   string   `json:"duplicate_of,omitempty"`
	Selected      bool     `json:"selected"`
	NeedsReview   bool     `json:"needs_review"`
	ReviewReasons []string `json:"review_reasons,omitempty"`

	normHash string
}

// Kind returns the arg-max kind, or "" when the segment was not classified.
func (r Record) Kind() string {
	if r.Signals == nil {
		return ""
	}
	return r.Signals.Kind
}

// Stats are counters collected while classifying.
type Stats struct {
	ModelCalls      int           `json:"model_calls"`
	CacheHits       int           `json:"cache_hits"`
	CacheMisses     int           `json:"cache_misses"`
	Errors          int           `json:"errors"`
	ContextExceeded int           `json:"context_exceeded"`
	InputTokens     int           `json:"input_tokens"` // summed over model calls (as reported by the server)
	OutputTokens    int           `json:"output_tokens"`
	ModelTime       time.Duration `json:"-"`
}

// Classifier classifies segments with tev1, using the cache when enabled.
type Classifier struct {
	Cfg     config.Config
	Client  *tev1.Client
	Cache   *cache.Cache // nil disables caching
	Log     *slog.Logger
	NoModel bool

	qsHash   string
	mu       sync.Mutex
	stats    Stats
	maxIn    int  // largest input_tokens seen in one call
	loggedIn bool // first-call token message emitted
}

func (cl *Classifier) log() *slog.Logger {
	if cl.Log != nil {
		return cl.Log
	}
	return slog.Default()
}

// Stats returns a snapshot of the counters.
func (cl *Classifier) Stats() Stats {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.stats
}

func (cl *Classifier) add(f func(*Stats)) {
	cl.mu.Lock()
	f(&cl.stats)
	cl.mu.Unlock()
}

type state struct {
	Segment  string `json:"segment"`
	Features string `json:"features,omitempty"`
}

// buildState renders the model input. Text beyond MaxTextBytes is cut on a
// rune boundary because tev1 only has a ~2k-token context.
func (cl *Classifier) buildState(text string, f features.Features) (any, bool) {
	truncated := false
	if cl.Cfg.State.NormalizeText {
		text = segment.NormalizeText(text)
	}
	if max := cl.Cfg.State.MaxTextBytes; max > 0 && len(text) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text, truncated = text[:cut], true
	}
	if !cl.Cfg.State.IncludeFeatures {
		return text, truncated
	}
	return state{Segment: text, Features: f.Summary()}, truncated
}

func round6(x float64) float64 { return math.Round(x*1e6) / 1e6 }

// BuildRecords segments are classified (or scored from features only) and
// turned into unranked Records. On context cancellation it returns ctx.Err()
// (everything already classified stays in the cache).
func (cl *Classifier) BuildRecords(ctx context.Context, segs []segment.Segment, feats []features.Features) ([]Record, error) {
	cfg := cl.Cfg
	cl.qsHash = cfg.QuestionSetHash()
	recs := make([]Record, len(segs))
	for i, s := range segs {
		f := feats[i]
		f.Position, f.TermWeight = round6(f.Position), round6(f.TermWeight)
		recs[i] = Record{
			ID: s.ID, Start: s.Start, End: s.End, Hash: s.Hash, Text: s.Text, Features: f,
			ScoredBy: ScoredByFeatures, normHash: rank.NormalizedHash(s.Text),
			FeatureBonus: round6(rank.FeatureBonus(cfg.Ranking, f)),
		}
		recs[i].FinalScore = recs[i].FeatureBonus
	}
	if cl.NoModel {
		return recs, nil
	}
	for _, w := range cfg.Warnings() {
		cl.log().Warn(w)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var consecutive, done int32
	var abort atomic.Value
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < cfg.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					continue
				}
				rec := &recs[i]
				ans, sig, usage, cached, err := cl.classifyOne(ctx, segs[i], feats[i], rec)
				if err != nil {
					if ctx.Err() != nil {
						continue
					}
					cl.add(func(s *Stats) { s.Errors++ })
					cl.log().Error("classification failed; falling back to features", "segment", rec.ID, "err", err)
					rec.ScoredBy, rec.ModelError = ScoredByFallback, err.Error()
					rec.FinalScore = round6(rank.Score(cfg.Ranking, rank.Signals{}, rec.FeatureBonus))
					rec.NeedsReview, rec.ReviewReasons = true, []string{"model_error"}
					if atomic.AddInt32(&consecutive, 1) >= maxConsecutiveFailures && cl.Stats().ModelCalls == int(consecutive) {
						abort.Store(fmt.Errorf("the first %d model calls all failed (last: %w); is %q usable? try `ollama run %s` or --no-model", consecutive, err, cfg.Model, cfg.Model))
						cancel()
					}
					continue
				}
				atomic.StoreInt32(&consecutive, 0)
				rec.Answers, rec.Signals, rec.Cached = ans, &sig, cached
				rec.ScoredBy = ScoredByModel
				rec.FinalScore = round6(rank.Score(cfg.Ranking, sig, rec.FeatureBonus))
				rec.ReviewReasons = rank.Review(cfg.Ranking.Review, sig)
				rec.InputTokens, rec.OutputTokens = usage.InputTokens, usage.OutputTokens
				if ctxTok := cfg.State.ContextTokens; ctxTok > 0 && usage.InputTokens > ctxTok {
					rec.ContextExceeded = true
					rec.ReviewReasons = append(rec.ReviewReasons, "context_window_exceeded")
					cl.add(func(s *Stats) { s.ContextExceeded++ })
				}
				rec.NeedsReview = len(rec.ReviewReasons) > 0
				if n := atomic.AddInt32(&done, 1); n%25 == 0 {
					cl.log().Info("progress", "classified", n, "of", len(segs))
				}
			}
		}()
	}
loop:
	for i := range segs {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break loop
		}
	}
	close(jobs)
	wg.Wait()
	if v := abort.Load(); v != nil {
		return nil, v.(error)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return recs, nil
}

func (cl *Classifier) classifyOne(ctx context.Context, seg segment.Segment, f features.Features, rec *Record) (map[string]tev1.Answer, rank.Signals, tev1.Usage, bool, error) {
	st, truncated := cl.buildState(seg.Text, f)
	rec.StateTruncated = truncated
	key := cache.Key(cl.Cfg.Model, cl.qsHash, seg.Hash)
	if cl.Cache != nil {
		if ans, usage, ok := cl.Cache.Get(key); ok {
			if sig, err := rank.ExtractSignals(cl.Cfg, ans); err == nil {
				cl.add(func(s *Stats) { s.CacheHits++ })
				return ans, sig, usage, true, nil
			}
			// A cached entry that no longer satisfies the config is just a miss.
		}
		cl.add(func(s *Stats) { s.CacheMisses++ })
	}
	start := time.Now()
	resp, err := cl.Client.Decide(ctx, tev1.Request{
		Model: cl.Cfg.Model, State: st, Questions: cl.Cfg.QuestionSet(), KeepAlive: cl.Cfg.KeepAlive,
	})
	cl.add(func(s *Stats) { s.ModelCalls++; s.ModelTime += time.Since(start) })
	if err != nil {
		return nil, rank.Signals{}, tev1.Usage{}, false, err
	}
	cl.noteUsage(resp.Usage)
	sig, err := rank.ExtractSignals(cl.Cfg, resp.Answers)
	if err != nil {
		return nil, rank.Signals{}, tev1.Usage{}, false, fmt.Errorf("%w: %v", tev1.ErrMalformed, err)
	}
	if cl.Cache != nil {
		if err := cl.Cache.Put(key, resp.Answers, resp.Usage); err != nil {
			cl.log().Warn("cache write failed", "err", err)
		}
	}
	return resp.Answers, sig, resp.Usage, false, nil
}

// Finalize ranks the records, removes near-duplicates, and marks the selection.
// It mutates recs in place.
func Finalize(recs []Record, sel rank.SelectOptions) []int {
	items := make([]rank.Item, len(recs))
	for i, r := range recs {
		items[i] = rank.Item{NormHash: r.normHash, Score: r.FinalScore, Kind: r.Kind()}
	}
	ranks, dupOf := rank.Rank(items)
	for i := range recs {
		if ranks[i] > 0 {
			r := ranks[i]
			recs[i].Rank = &r
		}
		if dupOf[i] >= 0 {
			recs[i].DuplicateOf = recs[dupOf[i]].ID
		}
	}
	chosen := rank.Select(items, ranks, sel)
	for _, i := range chosen {
		recs[i].Selected = true
	}
	return chosen
}

// Analyze runs the deterministic half of the pipeline on text.
func Analyze(text string, opt segment.Options) ([]segment.Segment, []features.Features) {
	segs := segment.Split(text, opt)
	return segs, features.ComputeAll(segs, len(text))
}

// IsCanceled reports whether err is a cancellation.
func IsCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func hashBytes(b []byte) string { return segment.HashText(string(b)) }

// noteUsage accumulates token counts and warns when a request approaches the
// context window. tev1 has a small (~2k token) context, and the question set
// itself is a large part of every request.
func (cl *Classifier) noteUsage(u tev1.Usage) {
	cl.mu.Lock()
	cl.stats.InputTokens += u.InputTokens
	cl.stats.OutputTokens += u.OutputTokens
	first := !cl.loggedIn && u.InputTokens > 0
	cl.loggedIn = cl.loggedIn || first
	newMax := u.InputTokens > cl.maxIn
	if newMax {
		cl.maxIn = u.InputTokens
	}
	cl.mu.Unlock()
	if first {
		cl.log().Info("first model call", "input_tokens", u.InputTokens, "context_tokens", cl.Cfg.State.ContextTokens)
	}
	if ctxTok := cl.Cfg.State.ContextTokens; ctxTok > 0 && newMax && float64(u.InputTokens) > 0.9*float64(ctxTok) {
		cl.log().Warn("request is close to the model context window; long segments may be truncated or rejected",
			"input_tokens", u.InputTokens, "context_tokens", ctxTok,
			"hint", "shorten question descriptions or lower segmenter.hard_max_bytes / state.max_text_bytes")
	}
}
