package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// An AuditManifest is the tamper-evident, attributable provenance record
// embedded in a report so an external auditor can trust it: it names the
// build and audit config that produced the verdicts, the scope covered, a
// content digest that detects tampering of the results, and a consolidated
// disclosure of everything the auditor must NOT take at face value.
//
//	audit_runs (build + config fingerprint) ──┐
//	audit_results (verdicts) ──────────────────→ AuditManifest ──→ Provenance sheet / JSON
//	commits (scope) ───────────────────────────┘                    └──→ integrity digest
type AuditManifest struct {
	// GeneratedAt is when this report was produced.
	GeneratedAt time.Time `json:"generated_at"`
	// ToolVersion / AuditFinishedAt / ConfigFingerprint come from the
	// audit-run stamp — the build and config that actually produced the
	// verdicts. Empty/zero when the DB predates provenance stamping.
	ToolVersion       string    `json:"tool_version"`
	AuditFinishedAt   time.Time `json:"audit_finished_at"`
	ConfigFingerprint string    `json:"config_fingerprint"`
	// ReportConfigFingerprint is the fingerprint of the config the REPORT
	// was run with. ConfigDrift is true when it differs from the audit-time
	// fingerprint — the verdicts predate the current rules and should be
	// re-audited before relying on them.
	ReportConfigFingerprint string `json:"report_config_fingerprint"`
	ConfigDrift             bool   `json:"config_drift"`
	// ResultsDigest is a SHA-256 over the canonical, ordered verdict rows
	// in scope. Recompute it over the same DB to detect tampering.
	ResultsDigest string          `json:"results_digest"`
	Scope         ManifestScope   `json:"scope"`
	Coverage      CoverageCaveats `json:"coverage"`
}

// ManifestScope records what the report covered.
type ManifestScope struct {
	Repos          []string  `json:"repos"`
	CommitCount    int       `json:"commit_count"`
	EarliestCommit time.Time `json:"earliest_commit"`
	LatestCommit   time.Time `json:"latest_commit"`
}

// CoverageCaveats consolidates every signal an auditor must not take at
// face value — the honest disclosure of the audit's residual assumptions.
type CoverageCaveats struct {
	// NonCompliant is the number of in-scope commits that failed the audit.
	NonCompliant int `json:"non_compliant"`
	// HistoryRewrites is force-push / non-fast-forward moves detected on
	// the in-scope repos — previously-audited history was rewritten.
	HistoryRewrites int `json:"history_rewrites"`
	// ForgeableExemptions is §1 waivers that rest on the forgeable
	// author-id hint (signing_policy: optional) — they would fail under
	// signing_policy: required.
	ForgeableExemptions int `json:"forgeable_exemptions"`
	// UnresolvedAuthorIDs is in-scope commits whose author id GitHub could
	// not bind to an account (id 0) — identity-based rules fall through.
	UnresolvedAuthorIDs int `json:"unresolved_author_ids"`
}

// BuildManifest assembles the provenance manifest for the given report
// scope. reportConfigFingerprint is config.AuditFingerprint() for the
// config the report is being run with; generatedAt is passed in for
// deterministic testing.
func (r *Reporter) BuildManifest(ctx context.Context, opts ReportOpts, reportConfigFingerprint string, generatedAt time.Time) (*AuditManifest, error) {
	m := &AuditManifest{
		GeneratedAt:             generatedAt,
		ReportConfigFingerprint: reportConfigFingerprint,
	}

	// 1. Audit-run provenance (build + config that produced the verdicts).
	runRow := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(tool_version, ''), COALESCE(config_fingerprint, ''), finished_at
		FROM audit_runs ORDER BY finished_at DESC LIMIT 1`)
	var af time.Time
	if err := runRow.Scan(&m.ToolVersion, &m.ConfigFingerprint, &af); err == nil {
		m.AuditFinishedAt = af
	}
	m.ConfigDrift = m.ConfigFingerprint != "" && reportConfigFingerprint != "" &&
		m.ConfigFingerprint != reportConfigFingerprint

	// 2. Scope + coverage aggregates, over the same commits the report covers.
	scopeSQL := `
		SELECT COUNT(*),
		       COALESCE(MIN(c.committed_at), TIMESTAMP '1970-01-01'),
		       COALESCE(MAX(c.committed_at), TIMESTAMP '1970-01-01'),
		       COUNT(*) FILTER (WHERE a.is_compliant = false),
		       COUNT(*) FILTER (WHERE a.is_compliant = true
		                          AND COALESCE(list_contains(a.annotations, 'trust:forgeable-exemption'), false)),
		       COUNT(*) FILTER (WHERE COALESCE(c.author_id, 0) = 0)
		FROM audit_results a
		JOIN commits c ON a.org = c.org AND a.repo = c.repo AND a.sha = c.sha
		WHERE 1=1` + defaultBranchExists
	args := []any{r.branchRegex}
	scopeSQL, args = appendRepoFilter(scopeSQL, args, opts)
	scopeSQL, args = appendSinceUntil(scopeSQL, args, opts)
	row := r.db.QueryRowContext(ctx, scopeSQL, args...)
	if err := row.Scan(&m.Scope.CommitCount, &m.Scope.EarliestCommit, &m.Scope.LatestCommit,
		&m.Coverage.NonCompliant, &m.Coverage.ForgeableExemptions, &m.Coverage.UnresolvedAuthorIDs); err != nil {
		return nil, fmt.Errorf("manifest scope query: %w", err)
	}

	repos, err := r.scopedRepos(ctx, opts)
	if err != nil {
		return nil, err
	}
	m.Scope.Repos = repos

	rewrites, err := r.historyRewriteCount(ctx, opts)
	if err != nil {
		return nil, err
	}
	m.Coverage.HistoryRewrites = rewrites

	// 3. Integrity digest over the canonical, ordered verdict rows.
	digest, err := r.resultsDigest(ctx, opts)
	if err != nil {
		return nil, err
	}
	m.ResultsDigest = digest

	return m, nil
}

// writeManifestHeader prints the provenance block that leads a plain-text
// report — the attribution and coverage disclosure an external auditor
// reads first.
func writeManifestHeader(w io.Writer, m *AuditManifest) {
	fmt.Fprintln(w, "=== AUDIT PROVENANCE ===")
	fmt.Fprintf(w, "Tool version:        %s\n", orNA(m.ToolVersion))
	fmt.Fprintf(w, "Config fingerprint:  %s\n", orNA(m.ConfigFingerprint))
	if m.ConfigDrift {
		fmt.Fprintf(w, "  ⚠ CONFIG DRIFT: verdicts were produced under a different config than this report's (%s) — re-audit before relying on them.\n", short(m.ReportConfigFingerprint))
	}
	if !m.AuditFinishedAt.IsZero() {
		fmt.Fprintf(w, "Audit finished:      %s\n", m.AuditFinishedAt.UTC().Format("2006-01-02 15:04 MST"))
	}
	fmt.Fprintf(w, "Report generated:    %s\n", m.GeneratedAt.UTC().Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(w, "Results digest:      sha256:%s\n", m.ResultsDigest)
	fmt.Fprintf(w, "Scope:               %d repo(s), %d commit(s)\n", len(m.Scope.Repos), m.Scope.CommitCount)
	fmt.Fprintln(w, "Coverage caveats (do NOT take at face value):")
	fmt.Fprintf(w, "  non-compliant:         %d\n", m.Coverage.NonCompliant)
	fmt.Fprintf(w, "  history rewrites:      %d\n", m.Coverage.HistoryRewrites)
	fmt.Fprintf(w, "  forgeable exemptions:  %d\n", m.Coverage.ForgeableExemptions)
	fmt.Fprintf(w, "  unresolved author ids: %d\n", m.Coverage.UnresolvedAuthorIDs)
	fmt.Fprintln(w)
}

func orNA(s string) string {
	if s == "" {
		return "(unknown — DB predates provenance stamping)"
	}
	return s
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// VerifyResultsDigest recomputes the results digest over the given scope
// and reports whether it matches the claimed digest — the independent
// tamper check an external auditor runs against a report's manifest.
// Returns (match, recomputed digest, error).
func (r *Reporter) VerifyResultsDigest(ctx context.Context, opts ReportOpts, claimed string) (bool, string, error) {
	actual, err := r.resultsDigest(ctx, opts)
	if err != nil {
		return false, "", err
	}
	return actual == claimed, actual, nil
}

// scopedRepos returns the distinct "org/repo" list in scope, sorted.
func (r *Reporter) scopedRepos(ctx context.Context, opts ReportOpts) ([]string, error) {
	q := `SELECT DISTINCT a.org || '/' || a.repo AS repo
	      FROM audit_results a WHERE 1=1` + defaultBranchExists
	args := []any{r.branchRegex}
	q, args = appendRepoFilter(q, args, opts)
	q += " ORDER BY repo"
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("manifest repos query: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			return nil, err
		}
		out = append(out, repo)
	}
	return out, rows.Err()
}

// historyRewriteCount counts recorded force-pushes for the in-scope repos.
func (r *Reporter) historyRewriteCount(ctx context.Context, opts ReportOpts) (int, error) {
	q := `SELECT COUNT(*) FROM history_rewrites a WHERE 1=1`
	var args []any
	q, args = appendRepoFilter(q, args, opts)
	var n int
	if err := r.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("manifest history-rewrite count: %w", err)
	}
	return n, nil
}

// resultsDigest streams the canonical verdict rows in a fixed order and
// returns their SHA-256. Any change to a verdict — or tampering with the
// audit_results table — changes the digest.
func (r *Reporter) resultsDigest(ctx context.Context, opts ReportOpts) (string, error) {
	q := `
		SELECT a.org || '|' || a.repo || '|' || a.sha || '|' ||
		       a.is_compliant || '|' ||
		       COALESCE(array_to_string(a.reasons, ';'), '') || '|' ||
		       COALESCE(a.is_exempt_author, false) || '|' ||
		       COALESCE(a.is_empty_commit, false) || '|' ||
		       COALESCE(a.is_clean_revert, false) || '|' ||
		       COALESCE(a.is_self_approved, false) || '|' ||
		       COALESCE(a.has_final_approval, false)
		FROM audit_results a WHERE 1=1` + defaultBranchExists
	args := []any{r.branchRegex}
	q, args = appendRepoFilter(q, args, opts)
	q += " ORDER BY a.org, a.repo, a.sha"
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return "", fmt.Errorf("manifest digest query: %w", err)
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		h.Write([]byte(line))
		h.Write([]byte{'\n'})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
