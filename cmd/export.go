package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stefanpenner/gh-audit/internal/db"
)

func newExportCmd() *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export all tables as parquet files",
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				return fmt.Errorf("--output is required")
			}

			dbConn, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer dbConn.Close()

			if err := dbConn.ExportParquet(cmd.Context(), output); err != nil {
				return fmt.Errorf("exporting parquet: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Exported tables to %s\n", output)
			return nil
		},
	}

	cmd.Flags().StringVar(&output, "output", "", "output directory for parquet files (required)")
	_ = cmd.MarkFlagRequired("output")

	return cmd
}
