package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanpenner/gh-audit/internal/db"
	"github.com/stefanpenner/gh-audit/internal/report"
)

// newVerifyCmd independently re-checks a report's tamper-evident digest
// against the database — the verification step an external auditor runs to
// confirm a JSON report's verdicts were not altered after generation.
func newVerifyCmd() *cobra.Command {
	var (
		manifestPath string
		org          string
		repos        []string
		since        string
		until        string
	)

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify a report's tamper-evident results digest against the database",
		Long: "Recomputes the audit results digest from the database and compares it to the\n" +
			"digest claimed in a JSON report's provenance manifest. A match proves the\n" +
			"report's verdicts were not altered after generation; a mismatch means the\n" +
			"report or the database changed. Pass the SAME scope flags (--repo/--since/\n" +
			"--until) that produced the report.\n\n" +
			"Exit code 0 = match, 1 = mismatch (or error).",
		RunE: func(cmd *cobra.Command, args []string) error {
			if manifestPath == "" {
				return fmt.Errorf("--manifest is required (a JSON report produced by `report --format json`)")
			}
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				return fmt.Errorf("reading manifest: %w", err)
			}
			var doc struct {
				Manifest *report.AuditManifest `json:"manifest"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("parsing manifest JSON: %w", err)
			}
			if doc.Manifest == nil || doc.Manifest.ResultsDigest == "" {
				return fmt.Errorf("%s carries no manifest.results_digest (was it produced by `report --format json`?)", manifestPath)
			}

			cfg, err := loadConfigOrDefault(cfgFile, cmd.Flag("config").Changed)
			if err != nil {
				return err
			}
			dbConn, err := db.Open(resolveDBPath(cfg, cmd.Flag("db").Changed))
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer dbConn.Close()

			opts, err := verifyReportOpts(org, repos, since, until)
			if err != nil {
				return err
			}

			r := report.NewWithBranches(dbConn.DB, cfg.AuditRules.AuditBranches)
			ok, actual, err := r.VerifyResultsDigest(cmd.Context(), opts, doc.Manifest.ResultsDigest)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if ok {
				fmt.Fprintf(out, "MATCH — results digest verified\n  sha256:%s\n", actual)
				fmt.Fprintf(out, "  tool: %s  config: %s\n", doc.Manifest.ToolVersion, short12(doc.Manifest.ConfigFingerprint))
				return nil
			}
			fmt.Fprintf(out, "MISMATCH — the report does NOT match the database\n")
			fmt.Fprintf(out, "  claimed:    sha256:%s\n", doc.Manifest.ResultsDigest)
			fmt.Fprintf(out, "  recomputed: sha256:%s\n", actual)
			fmt.Fprintf(out, "The verdicts were altered after the report was generated, the database changed,\n"+
				"or the scope flags differ from the report's. Do not rely on the report.\n")
			return fmt.Errorf("results digest mismatch")
		},
	}

	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to the JSON report to verify (from `report --format json`)")
	cmd.Flags().StringVar(&org, "org", "", "scope: filter by org (must match the report)")
	cmd.Flags().StringSliceVar(&repos, "repo", nil, "scope: filter by repo org/repo (must match the report)")
	cmd.Flags().StringVar(&since, "since", "", "scope: since date (must match the report)")
	cmd.Flags().StringVar(&until, "until", "", "scope: until date (must match the report)")
	return cmd
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// verifyReportOpts mirrors the report command's scope parsing so a verify
// run reconstructs the exact set of commits the report digested.
func verifyReportOpts(org string, repos []string, since, until string) (report.ReportOpts, error) {
	opts := report.ReportOpts{Org: org}
	for _, r := range repos {
		parts := strings.SplitN(r, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return opts, fmt.Errorf("invalid --repo format %q: expected org/repo", r)
		}
		opts.Repos = append(opts.Repos, report.RepoFilter{Org: parts[0], Repo: parts[1]})
	}
	parse := func(flag, v string, dst *time.Time) error {
		if v == "" {
			return nil
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			*dst = t
		} else if t, err := time.Parse("2006-01-02", v); err == nil {
			*dst = t
		} else {
			return fmt.Errorf("invalid --%s format: %s (use ISO 8601)", flag, v)
		}
		return nil
	}
	if err := parse("since", since, &opts.Since); err != nil {
		return opts, err
	}
	if err := parse("until", until, &opts.Until); err != nil {
		return opts, err
	}
	return opts, nil
}
