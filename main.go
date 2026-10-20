// Command tev1-rank is stage 1 of the infographic pipeline: it segments a
// plain-text document, has a tev1 decision model (served by Ollama) classify
// and score each segment, and writes a ranked list for the next stage.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/s0cks/tev1-rank/cmd"
	"github.com/s0cks/tev1-rank/internal/pipeline"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cmd.NewRoot().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if ctx.Err() != nil || (errors.Is(err, context.Canceled) && pipeline.IsCanceled(err)) {
			stop()
			os.Exit(130)
		}

		os.Exit(1)
	}
}
