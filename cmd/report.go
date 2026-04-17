package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanpenner/gh-audit/internal/db"
	"github.com/stefanpenner/gh-audit/internal/report"
)

func newReportCmd() *cobra.Command {
	var (
		org          string
		repo         string
		since        string
		until        string
		format       string
		onlyFailures bool
		output       string
	)

	cmd := &cobra.Command{
		Use:   "report",
		Short: "Generate audit reports",
		RunE: func(cmd *cobra.Command, args []string) error {
			dbConn, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer dbConn.Close()

			// Support org/repo format for --repo flag.
			if repo != "" && strings.Contains(repo, "/") {
				parts := strings.SplitN(repo, "/", 2)
				if org == "" {
					org = parts[0]
				}
				repo = parts[1]
			}

			opts := report.ReportOpts{
				Org:          org,
				Repo:         repo,
				OnlyFailures: onlyFailures,
			}

			if since != "" {
				if t, err := time.Parse(time.RFC3339, since); err == nil {
					opts.Since = t
				} else if t, err := time.Parse("2006-01-02", since); err == nil {
					opts.Since = t
				} else {
					return fmt.Errorf("invalid --since format: %s (use ISO 8601)", since)
				}
			}
			if until != "" {
				if t, err := time.Parse(time.RFC3339, until); err == nil {
					opts.Until = t
				} else if t, err := time.Parse("2006-01-02", until); err == nil {
					opts.Until = t
				} else {
					return fmt.Errorf("invalid --until format: %s (use ISO 8601)", until)
				}
			}

			r := report.New(dbConn.DB)

			switch format {
			case "xlsx":
				if output == "" {
					return fmt.Errorf("--output is required for xlsx format")
				}
				return r.GenerateXLSX(cmd.Context(), opts, output)

			case "table", "":
				summary, err := r.GetSummary(cmd.Context(), opts)
				if err != nil {
					return err
				}
				details, err := r.GetDetails(cmd.Context(), opts)
				if err != nil {
					return err
				}

				w := os.Stdout
				if output != "" {
					f, err := os.Create(output)
					if err != nil {
						return err
					}
					defer f.Close()
					w = f
				}
				return r.FormatTable(w, summary, details)

			case "csv":
				details, err := r.GetDetails(cmd.Context(), opts)
				if err != nil {
					return err
				}
				w := os.Stdout
				if output != "" {
					f, err := os.Create(output)
					if err != nil {
						return err
					}
					defer f.Close()
					w = f
				}
				return r.FormatCSV(w, details)

			case "json":
				summary, err := r.GetSummary(cmd.Context(), opts)
				if err != nil {
					return err
				}
				details, err := r.GetDetails(cmd.Context(), opts)
				if err != nil {
					return err
				}
				w := os.Stdout
				if output != "" {
					f, err := os.Create(output)
					if err != nil {
						return err
					}
					defer f.Close()
					w = f
				}
				return r.FormatJSON(w, summary, details)

			default:
				return fmt.Errorf("unsupported format: %s (use table, csv, json, or xlsx)", format)
			}
		},
	}

	cmd.Flags().StringVar(&org, "org", "", "filter by org")
	cmd.Flags().StringVar(&repo, "repo", "", "filter by repo")
	cmd.Flags().StringVar(&since, "since", "", "filter since date (ISO 8601)")
	cmd.Flags().StringVar(&until, "until", "", "filter until date (ISO 8601)")
	cmd.Flags().StringVar(&format, "format", "table", "output format (table|csv|json|xlsx)")
	cmd.Flags().BoolVar(&onlyFailures, "only-failures", false, "show only non-compliant commits")
	cmd.Flags().StringVar(&output, "output", "", "output file path (required for xlsx)")

	return cmd
}
