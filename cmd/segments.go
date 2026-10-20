package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/s0cks/tev1-rank/internal/config"
	"github.com/s0cks/tev1-rank/internal/features"
	"github.com/s0cks/tev1-rank/internal/pipeline"
	"github.com/s0cks/tev1-rank/internal/segment"
)

func newSegments() *cobra.Command {
	var configPath string
	var maxPara, hardMax int
	var noFeatures bool
	cmd := &cobra.Command{
		Use:   "segments <input.txt>",
		Short: "Run only the deterministic segmenter and print JSONL (no model, no Ollama)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}

			opt := cfg.Segmenter
			if cmd.Flags().Changed("max-paragraph-bytes") {
				opt.MaxParagraphBytes = maxPara
			}

			if cmd.Flags().Changed("hard-max-bytes") {
				opt.HardMaxBytes = hardMax
			}

			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}

			segs, feats := pipeline.Analyze(string(data), opt)
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetEscapeHTML(false)
			type line struct {
				segment.Segment
				Features *features.Features `json:"features,omitempty"`
			}

			for i, s := range segs {
				l := line{Segment: s}
				if !noFeatures {
					l.Features = &feats[i]
				}

				if err := enc.Encode(l); err != nil {
					return err
				}
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "%d segments\n", len(segs))
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "JSON config file")
	cmd.Flags().IntVar(&maxPara, "max-paragraph-bytes", 0, "split paragraphs longer than this into sentences (0 = never)")
	cmd.Flags().IntVar(&hardMax, "hard-max-bytes", 0, "never emit a segment longer than this (0 = no cap)")
	cmd.Flags().BoolVar(&noFeatures, "no-features", false, "omit the features object")
	return cmd
}
