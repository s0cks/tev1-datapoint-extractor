package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/s0cks/tev1-rank/internal/pipeline"
	"github.com/s0cks/tev1-rank/internal/rank"
)

func newRank() *cobra.Command {
	var mf modelFlags
	var outDir string
	var top, maxPerKind, maxPara int
	var minScore float64
	var allowErrors bool
	cmd := &cobra.Command{
		Use:   "rank <input.txt>",
		Short: "Segment, classify with tev1, rank; write scored.jsonl, top.json and run.json",
		Long: `Runs the whole of stage 1.

Outputs (in --out-dir):
  scored.jsonl  every segment with offsets, hash, features, raw model answers, score, rank
  top.json      the selected top-N, in the shape stage 2 consumes
  run.json      manifest: input hash, model, Ollama version, question_set_hash, config, counts, timings

With --no-model, ranking uses deterministic features only and the outputs say so.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := mf.load(cmd)
			if err != nil {
				return err
			}

			fl := cmd.Flags()
			sel := rank.SelectOptions{
				Top:          cfg.Selection.Top,
				MinScore:     cfg.Selection.MinScore,
				MaxPerKind:   cfg.Selection.MaxPerKind,
				ExcludeKinds: cfg.Selection.ExcludeKinds,
			}
			if fl.Changed("top") {
				sel.Top = top
			}

			if fl.Changed("min-score") {
				sel.MinScore = minScore
			}

			if fl.Changed("max-per-kind") {
				sel.MaxPerKind = maxPerKind
			}

			if fl.Changed("max-paragraph-bytes") {
				cfg.Segmenter.MaxParagraphBytes = maxPara
			}

			res, err := pipeline.Run(cmd.Context(), pipeline.Options{
				Config: cfg, InputPath: args[0], OutDir: outDir, NoModel: mf.noModel, NoCache: mf.noCache,
				CacheDir: mf.cacheDir, SkipPreflight: mf.skipPreflight, Select: sel,
			})
			if err != nil {
				return err
			}

			c := res.Manifest.Counts
			fmt.Fprintf(cmd.OutOrStdout(), "mode=%s segments=%d duplicates=%d selected=%d needs_review=%d model_calls=%d cache_hits=%d\n→ %s\n",
				res.Manifest.Mode, c.Segments, c.Duplicates, c.Selected, c.NeedsReview, c.ModelCalls, c.CacheHits, filepath.Join(outDir, "top.json"))
			if res.ModelErrors > 0 && !allowErrors {
				return fmt.Errorf("%d segment(s) fell back to feature-only scoring after model errors (outputs were written; see model_error in scored.jsonl; use --allow-errors to accept this)", res.ModelErrors)
			}

			return nil
		},
	}
	mf.add(cmd.Flags())
	cmd.Flags().StringVar(&outDir, "out-dir", "out", "directory for scored.jsonl, top.json, run.json")
	cmd.Flags().IntVar(&top, "top", 0, "select the top N segments (default from config: 20; 0 = all)")
	cmd.Flags().Float64Var(&minScore, "min-score", 0, "drop selected segments scoring below this (0..1)")
	cmd.Flags().IntVar(&maxPerKind, "max-per-kind", 0, "cap how many selected segments share one kind (0 = no cap)")
	cmd.Flags().IntVar(&maxPara, "max-paragraph-bytes", 0, "split paragraphs longer than this into sentences")
	cmd.Flags().BoolVar(&allowErrors, "allow-errors", false, "exit 0 even if some segments fell back to feature-only scoring")
	return cmd
}
