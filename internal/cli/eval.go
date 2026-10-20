package cli

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/s0cks/tev1-rank/internal/evaluate"
	"github.com/s0cks/tev1-rank/internal/pipeline"
)

func newEval() *cobra.Command {
	var mf modelFlags
	var ks []int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "eval <labels.jsonl>",
		Short: "Score the classifier against a hand-labeled set",
		Long: `Each line of the labels file is {"text": "...", "is_datapoint": true, "kind": "stats"}
("kind" is optional; "other" means not a datapoint and must go with is_datapoint=false).
Every label is classified as a stand-alone segment. Prints is_datapoint accuracy
(predicted kind != other), per-kind precision/recall, a kind confusion matrix, and
precision@K of the ranking.
Run it once per model (--model tev1:0.8b, --model tev1:4b) to compare them.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := mf.load(cmd)
			if err != nil {
				return err
			}
			labels, err := evaluate.LoadLabels(args[0])
			if err != nil {
				return fmt.Errorf("reading labels: %w", err)
			}
			if len(labels) == 0 {
				return fmt.Errorf("%s contains no labels", args[0])
			}
			if !cmd.Flags().Changed("k") {
				ks = cfg.Eval.K
			}
			log := slog.Default()
			client := pipeline.NewClient(cfg, log)
			if !mf.noModel && !mf.skipPreflight {
				if _, err := client.Preflight(cmd.Context(), cfg.Model); err != nil {
					return fmt.Errorf("preflight: %w", err)
				}
			}
			cc, err := pipeline.NewCache(mf.noCache || mf.noModel, mf.cacheDir)
			if err != nil {
				return err
			}
			cl := &pipeline.Classifier{Cfg: cfg, Client: client, Cache: cc, Log: log, NoModel: mf.noModel}
			rep, err := evaluate.Run(cmd.Context(), cl, labels, ks)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), rep.Format())
			return err
		},
	}
	mf.add(cmd.Flags())
	cmd.Flags().IntSliceVar(&ks, "k", nil, "values of K for precision@K (default from config: 5,10)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	return cmd
}
