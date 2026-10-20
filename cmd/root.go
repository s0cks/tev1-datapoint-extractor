// Package cli defines the cobra command tree.
package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/s0cks/tev1-rank/internal/config"
	"github.com/s0cks/tev1-rank/internal/pipeline"
)

// NewRoot builds the root command.
func NewRoot() *cobra.Command {
	var verbose bool
	root := &cobra.Command{
		Use:           "tev1-rank",
		Short:         "Stage 1 of the infographic pipeline: segment text, classify with tev1, rank",
		Version:       pipeline.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "debug logging")
	root.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		level := slog.LevelInfo
		if verbose {
			level = slog.LevelDebug
		}
		slog.SetDefault(slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level})))
	}
	root.AddCommand(newRank(), newSegments(), newEval(), newConfigCmd())
	return root
}

// modelFlags are shared by commands that may talk to Ollama.
type modelFlags struct {
	configPath    string
	model         string
	ollamaURL     string
	concurrency   int
	timeoutSec    int
	retries       int
	noCache       bool
	cacheDir      string
	noModel       bool
	skipPreflight bool
}

func (m *modelFlags) add(fs *pflag.FlagSet) {
	fs.StringVar(&m.configPath, "config", "", "JSON config file overriding the embedded defaults (see `tev1-rank config`)")
	fs.StringVar(&m.model, "model", "", "Ollama model tag (default from config: tev1:0.8b)")
	fs.StringVar(&m.ollamaURL, "ollama-url", "", "Ollama base URL (default: $OLLAMA_HOST or http://localhost:11434)")
	fs.IntVar(&m.concurrency, "concurrency", 0, "parallel requests (default 1; tev1 loads one model, keep this low on 16 GB)")
	fs.IntVar(&m.timeoutSec, "timeout", 0, "per-request timeout in seconds")
	fs.IntVar(&m.retries, "retries", 0, "retries per request after the first attempt")
	fs.BoolVar(&m.noCache, "no-cache", false, "bypass the on-disk answer cache")
	fs.StringVar(&m.cacheDir, "cache-dir", "", "cache directory (default: <user cache dir>/tev1-rank)")
	fs.BoolVar(&m.noModel, "no-model", false, "rank from deterministic features only; Ollama is not contacted")
	fs.BoolVar(&m.skipPreflight, "skip-preflight", false, "skip the Ollama version/model check")
}

// load returns the effective config: embedded defaults < config file < flags.
func (m *modelFlags) load(cmd *cobra.Command) (config.Config, error) {
	cfg, err := config.Load(m.configPath)
	if err != nil {
		return cfg, err
	}

	fl := cmd.Flags()
	if fl.Changed("model") {
		cfg.Model = m.model
	}

	switch {
	case fl.Changed("ollama-url"):
		cfg.OllamaURL = m.ollamaURL

	case os.Getenv("OLLAMA_HOST") != "" && m.configPath == "":
		cfg.OllamaURL = normalizeHost(os.Getenv("OLLAMA_HOST"))
	}

	if fl.Changed("concurrency") {
		cfg.Concurrency = m.concurrency
	}

	if fl.Changed("timeout") {
		cfg.TimeoutSeconds = m.timeoutSec
	}

	if fl.Changed("retries") {
		cfg.Retries = m.retries
	}

	return cfg, cfg.Validate()
}

// normalizeHost turns OLLAMA_HOST values like "0.0.0.0:11434" or "host" into URLs.
func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	if !strings.Contains(h, "://") {
		h = "http://" + h
	}
	// A bind-all address is not a useful destination.
	h = strings.Replace(h, "://0.0.0.0", "://127.0.0.1", 1)
	if i := strings.LastIndex(h, ":"); i < len("http://") || strings.Contains(h[i:], "/") {
		h = strings.TrimRight(h, "/") + ":11434"
	}
	return h
}

func newConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Print the embedded default configuration (a starting point for --config)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), strings.TrimRight(string(config.DefaultJSON()), "\n"))
			return err
		},
	}
}
