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
		repos        []string
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
			cfg, err := loadConfigOrDefault(cfgFile, cmd.Flag("config").Changed)
			if err != nil {
				return err
			}

			dbConn, err := db.Open(resolveDBPath(cfg, cmd.Flag("db").Changed))
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer dbConn.Close()

			// Build repo filters from --repo flags (org/repo format).
			var repoFilters []report.RepoFilter
			for _, r := range repos {
				parts := strings.SplitN(r, "/", 2)
				if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
					return fmt.Errorf("invalid --repo format %q: expected org/repo", r)
				}
				repoFilters = append(repoFilters, report.RepoFilter{Org: parts[0], Repo: parts[1]})
			}

			opts := report.ReportOpts{
				Org:          org,
				Repos:        repoFilters,
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

			r := report.NewWithBranches(dbConn.DB, cfg.AuditRules.AuditBranches)

			// The provenance manifest attributes this report to a build +
			// audit config and carries a tamper-evident results digest.
			manifest, err := r.BuildManifest(cmd.Context(), opts, cfg.AuditFingerprint(), time.Now().UTC())
			if err != nil {
				return err
			}

			switch format {
			case "xlsx":
				if output == "" {
					return fmt.Errorf("--output is required for xlsx format")
				}
				// The workbook's sheets cross-reference each other (README
				// at-a-glance stats vs Summary vs Decision Matrix); filtering
				// details to failures only while summaries stay unfiltered
				// produces a self-contradicting workbook.
				if onlyFailures {
					return fmt.Errorf("--only-failures is not supported with --format xlsx: the workbook's summary sheets would disagree with its filtered detail sheets (use table/csv/json, or filter in Excel)")
				}
				return r.GenerateXLSX(cmd.Context(), opts, output, manifest)

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
				return r.FormatTable(w, summary, details, manifest)

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
				return r.FormatJSON(w, summary, details, manifest)

			default:
				return fmt.Errorf("unsupported format: %s (use table, csv, json, or xlsx)", format)
			}
		},
	}

	cmd.Flags().StringVar(&org, "org", "", "filter by org")
	cmd.Flags().StringSliceVar(&repos, "repo", nil, "filter by repo (org/repo format, repeatable)")
	cmd.Flags().StringVar(&since, "since", "", "filter since date (ISO 8601)")
	cmd.Flags().StringVar(&until, "until", "", "filter until date (ISO 8601)")
	cmd.Flags().StringVar(&format, "format", "table", "output format (table|csv|json|xlsx)")
	cmd.Flags().BoolVar(&onlyFailures, "only-failures", false, "show only non-compliant commits")
	cmd.Flags().StringVar(&output, "output", "", "output file path (required for xlsx)")

	return cmd
}
