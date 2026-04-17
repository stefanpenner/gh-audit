package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stefanpenner/gh-audit/internal/db"
)

func newImportCmd() *cobra.Command {
	var input string

	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import data from parquet files",
		RunE: func(cmd *cobra.Command, args []string) error {
			if input == "" {
				return fmt.Errorf("--input is required")
			}

			dbConn, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer dbConn.Close()

			if err := dbConn.ImportParquet(cmd.Context(), input); err != nil {
				return fmt.Errorf("importing parquet: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Imported tables from %s\n", input)
			return nil
		},
	}

	cmd.Flags().StringVar(&input, "input", "", "input directory containing parquet files (required)")
	_ = cmd.MarkFlagRequired("input")

	return cmd
}
