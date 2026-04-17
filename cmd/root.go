package cmd

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"
	"github.com/stefanpenner/gh-audit/internal/config"
)

var (
	cfgFile string
	dbPath  string
	verbose bool
)

// NewRootCmd creates the root cobra command.
func NewRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "gh-audit",
		Short: "Enterprise-scale GitHub commit audit tool",
		Long:  "gh-audit audits commits across GitHub organisations for compliance with PR, review, and check requirements.",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			level := slog.LevelInfo
			if verbose {
				level = slog.LevelDebug
			}
			handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
			slog.SetDefault(slog.New(handler))
		},
		SilenceUsage: true,
	}

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", config.DefaultConfigPath(), "config file path")
	rootCmd.PersistentFlags().StringVar(&dbPath, "db", config.DefaultDBPath(), "database file path")
	rootCmd.PersistentFlags().BoolVar(&verbose, "verbose", false, "enable verbose logging")

	rootCmd.AddCommand(newSyncCmd())
	rootCmd.AddCommand(newReportCmd())
	rootCmd.AddCommand(newConfigCmd())
	rootCmd.AddCommand(newExportCmd())
	rootCmd.AddCommand(newImportCmd())

	return rootCmd
}
