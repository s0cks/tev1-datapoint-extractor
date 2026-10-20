package cmd

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/s0cks/tev1-rank/internal/input"
)

func newValidate() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <segments.json>",
		Short: "Check a segments file from the text-preparation tool and summarise it (no model, no Ollama)",
		Long: `Parses the file exactly as 'rank' would and prints what it found: the source document,
how many segments of each block kind, how many entries were skipped, and how many segments
have numeric tokens or named entities. Exits non-zero if the file cannot be used.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			doc, _, err := input.Load(args[0])
			if err != nil {
				return err
			}

			kinds := map[string]int{}
			var withNum, withEnt int
			for i, f := range doc.Features() {
				kinds[doc.Segments[i].Kind]++
				if f.Numerics > 0 {
					withNum++
				}
				if f.Entities > 0 {
					withEnt++
				}
			}
			names := make([]string, 0, len(kinds))
			for k := range kinds {
				names = append(names, k)
			}
			sort.Strings(names)

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "source: %s (%s)\nsegments: %d   skipped entries: %d\n", doc.Source, doc.Format, len(doc.Segments), doc.Skipped)
			for _, k := range names {
				fmt.Fprintf(out, "  %-12s %d\n", k, kinds[k])
			}
			fmt.Fprintf(out, "with numeric tokens: %d   with named entities: %d\n", withNum, withEnt)
			return nil
		},
	}
}
