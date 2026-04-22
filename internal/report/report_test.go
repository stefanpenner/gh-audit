package report

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "github.com/marcboeker/go-duckdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"
)

const schemaDDL = `
CREATE TABLE IF NOT EXISTS sync_cursors (
	org        TEXT NOT NULL,
	repo       TEXT NOT NULL,
	last_date  TIMESTAMP,
	updated_at TIMESTAMP,
	PRIMARY KEY (org, repo)
);

CREATE TABLE IF NOT EXISTS commits (
	org              TEXT NOT NULL,
	repo             TEXT NOT NULL,
	sha              TEXT NOT NULL,
	author_login     TEXT,
	author_email     TEXT,
	committer_login  TEXT,
	committed_at     TIMESTAMP,
	message          TEXT,
	parent_count     INTEGER,
	additions        INTEGER,
	deletions        INTEGER,
	href             TEXT,
	fetched_at       TIMESTAMP DEFAULT current_timestamp,
	PRIMARY KEY (org, repo, sha)
);

CREATE TABLE IF NOT EXISTS commit_prs (
	org       TEXT NOT NULL,
	repo      TEXT NOT NULL,
	sha       TEXT NOT NULL,
	pr_number INTEGER NOT NULL,
	PRIMARY KEY (org, repo, sha, pr_number)
);

CREATE TABLE IF NOT EXISTS commit_branches (
	org    TEXT NOT NULL,
	repo   TEXT NOT NULL,
	sha    TEXT NOT NULL,
	branch TEXT NOT NULL,
	PRIMARY KEY (org, repo, sha, branch)
);

CREATE TABLE IF NOT EXISTS pull_requests (
	org              TEXT NOT NULL,
	repo             TEXT NOT NULL,
	number           INTEGER NOT NULL,
	title            TEXT,
	merged           BOOLEAN,
	head_sha         TEXT,
	head_branch      TEXT,
	merge_commit_sha TEXT,
	author_login     TEXT,
	merged_by_login  TEXT,
	merged_at        TIMESTAMP,
	href             TEXT,
	fetched_at       TIMESTAMP DEFAULT current_timestamp,
	PRIMARY KEY (org, repo, number)
);

CREATE TABLE IF NOT EXISTS reviews (
	org            TEXT NOT NULL,
	repo           TEXT NOT NULL,
	pr_number      INTEGER NOT NULL,
	review_id      BIGINT NOT NULL,
	reviewer_login TEXT,
	state          TEXT,
	commit_id      TEXT,
	submitted_at   TIMESTAMP,
	href           TEXT,
	fetched_at     TIMESTAMP DEFAULT current_timestamp,
	PRIMARY KEY (org, repo, pr_number, review_id)
);

CREATE TABLE IF NOT EXISTS check_runs (
	org           TEXT NOT NULL,
	repo          TEXT NOT NULL,
	commit_sha    TEXT NOT NULL,
	check_run_id  BIGINT NOT NULL,
	check_name    TEXT,
	status        TEXT,
	conclusion    TEXT,
	completed_at  TIMESTAMP,
	fetched_at    TIMESTAMP DEFAULT current_timestamp,
	PRIMARY KEY (org, repo, commit_sha, check_run_id)
);

CREATE TABLE IF NOT EXISTS audit_results (
	org                  TEXT NOT NULL,
	repo                 TEXT NOT NULL,
	sha                  TEXT NOT NULL,
	is_empty_commit      BOOLEAN,
	is_bot               BOOLEAN,
	is_exempt_author     BOOLEAN,
	has_pr               BOOLEAN,
	pr_number            INTEGER,
	pr_count             INTEGER DEFAULT 0,
	has_final_approval   BOOLEAN,
	has_stale_approval   BOOLEAN DEFAULT false,
	has_post_merge_concern BOOLEAN DEFAULT false,
	is_clean_revert      BOOLEAN DEFAULT false,
	revert_verification  TEXT,
	reverted_sha         TEXT,
	is_clean_merge       BOOLEAN DEFAULT false,
	merge_verification   TEXT,
	approver_logins      TEXT[],
	owner_approval_check TEXT,
	is_compliant         BOOLEAN,
	reasons              TEXT[],
	commit_href          TEXT,
	pr_href              TEXT,
	is_self_approved     BOOLEAN,
	merge_strategy            TEXT,
	pr_commit_author_logins   TEXT[],
	annotations               TEXT[],
	audited_at                TIMESTAMP DEFAULT current_timestamp,
	PRIMARY KEY (org, repo, sha)
);
`

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err, "opening duckdb")
	t.Cleanup(func() { db.Close() })

	for _, stmt := range strings.Split(schemaDDL, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		_, err := db.Exec(stmt)
		require.NoError(t, err, "schema exec: SQL: %s", stmt)
	}

	return db
}

func insertCommit(t *testing.T, db *sql.DB, org, repo, sha, author string, committedAt time.Time, additions, deletions int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO commits (org, repo, sha, author_login, committed_at, message, parent_count, additions, deletions, href)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		org, repo, sha, author, committedAt, "commit "+sha, 1, additions, deletions,
		fmt.Sprintf("https://github.com/%s/%s/commit/%s", org, repo, sha))
	require.NoError(t, err, "insert commit")
}

type auditResultOpts struct {
	isBot, isExempt, isEmpty, hasPR, hasApproval, isCompliant, isSelfApproved, hasStaleApproval bool
	prNumber, prCount                                                                           int
	approvers                                                                                   []string
	reasons                                                                                     []string
}

func insertAuditResult(t *testing.T, db *sql.DB, org, repo, sha string, isBot, isEmpty, hasPR, hasApproval, isCompliant bool, prNumber int, approvers []string, reasons []string) {
	t.Helper()
	insertAuditResultFull(t, db, org, repo, sha, auditResultOpts{
		isBot: isBot, isExempt: isBot, isEmpty: isEmpty,
		hasPR: hasPR, hasApproval: hasApproval, isCompliant: isCompliant,
		prNumber: prNumber, approvers: approvers, reasons: reasons,
	})
}

func insertAuditResultFull(t *testing.T, db *sql.DB, org, repo, sha string, opts auditResultOpts) {
	t.Helper()

	approverExpr := "list_value()"
	if len(opts.approvers) > 0 {
		quoted := make([]string, len(opts.approvers))
		for i, a := range opts.approvers {
			quoted[i] = fmt.Sprintf("'%s'", a)
		}
		approverExpr = fmt.Sprintf("list_value(%s)", strings.Join(quoted, ", "))
	}

	reasonExpr := "list_value()"
	if len(opts.reasons) > 0 {
		quoted := make([]string, len(opts.reasons))
		for i, r := range opts.reasons {
			quoted[i] = fmt.Sprintf("'%s'", r)
		}
		reasonExpr = fmt.Sprintf("list_value(%s)", strings.Join(quoted, ", "))
	}

	q := fmt.Sprintf(`INSERT INTO audit_results (org, repo, sha, is_empty_commit, is_bot, is_exempt_author, has_pr, pr_number, pr_count, has_final_approval, has_stale_approval, approver_logins, owner_approval_check, is_compliant, reasons, commit_href, pr_href, is_self_approved)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, %s, ?, ?, %s, ?, ?, ?)`, approverExpr, reasonExpr)

	_, err := db.Exec(q,
		org, repo, sha, opts.isEmpty, opts.isBot, opts.isExempt, opts.hasPR, opts.prNumber, opts.prCount, opts.hasApproval, opts.hasStaleApproval,
		"success", opts.isCompliant,
		fmt.Sprintf("https://github.com/%s/%s/commit/%s", org, repo, sha),
		fmt.Sprintf("https://github.com/%s/%s/pull/%d", org, repo, opts.prNumber),
		opts.isSelfApproved)
	require.NoError(t, err, "insert audit result")

	// Default-branch membership: the report layer filters audit_results to
	// commits on 'master' or 'main' (see defaultBranchExists). Tests that
	// expect an audit row to be visible need a corresponding commit_branches
	// entry. Insert one by default; tests that want to simulate a
	// PR-branch-only commit can insert an explicit non-default branch before
	// or after (multi-branch rows coexist).
	_, err = db.Exec(`INSERT OR IGNORE INTO commit_branches (org, repo, sha, branch) VALUES (?, ?, ?, 'master')`,
		org, repo, sha)
	require.NoError(t, err, "insert commit branch (master)")
}

func insertCommitBranch(t *testing.T, db *sql.DB, org, repo, sha, branch string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO commit_branches (org, repo, sha, branch) VALUES (?, ?, ?, ?)`,
		org, repo, sha, branch)
	require.NoError(t, err, "insert commit branch")
}

func TestGetSummaryCorrectCounts(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "bbb", "dev2", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "ccc", "bot1", now, 0, 0)
	insertCommit(t, db, "org1", "repo2", "ddd", "dev1", now, 10, 5)

	insertAuditResult(t, db, "org1", "repo1", "aaa", false, false, true, true, true, 1, []string{"reviewer1"}, []string{"compliant"})
	insertAuditResult(t, db, "org1", "repo1", "bbb", false, false, false, false, false, 0, nil, []string{"no associated pull request"})
	insertAuditResult(t, db, "org1", "repo1", "ccc", false, true, false, false, true, 0, nil, []string{"empty commit"})
	insertAuditResult(t, db, "org1", "repo2", "ddd", false, false, true, true, true, 2, []string{"reviewer1"}, []string{"compliant"})

	r := New(db)
	summary, err := r.GetSummary(context.Background(), ReportOpts{})
	require.NoError(t, err, "GetSummary")
	require.Len(t, summary, 2)

	// repo1: 3 total, 2 compliant, 1 non-compliant, 0 bots, 1 empty
	s := summary[0]
	assert.Equal(t, 3, s.TotalCommits, "repo1 total")
	assert.Equal(t, 2, s.CompliantCount, "repo1 compliant")
	assert.Equal(t, 1, s.NonCompliantCount, "repo1 non-compliant")
	assert.Equal(t, 1, s.EmptyCount, "repo1 empty")

	// repo2: 1 total, 1 compliant
	s = summary[1]
	assert.Equal(t, 1, s.TotalCommits, "repo2 total")
	assert.Equal(t, 1, s.CompliantCount, "repo2 compliant")
}

func TestGetSummaryPartitionInvariant(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	// Mix of overlapping categories:
	// exempt bot (compliant + bot + exempt), empty (compliant + empty),
	// non-exempt bot (non-compliant + bot), self-approved (non-compliant + self-approved),
	// normal compliant
	insertCommit(t, db, "org1", "repo1", "aaa", "dependabot[bot]", now, 5, 2)
	insertCommit(t, db, "org1", "repo1", "bbb", "dev1", now, 0, 0)
	insertCommit(t, db, "org1", "repo1", "ccc", "ci-bot[bot]", now, 3, 1)
	insertCommit(t, db, "org1", "repo1", "ddd", "dev2", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "eee", "dev3", now, 7, 3)

	insertAuditResultFull(t, db, "org1", "repo1", "aaa", auditResultOpts{isBot: true, isExempt: true, isCompliant: true, reasons: []string{"exempt: configured author"}})
	insertAuditResultFull(t, db, "org1", "repo1", "bbb", auditResultOpts{isEmpty: true, isCompliant: true, reasons: []string{"empty commit"}})
	insertAuditResultFull(t, db, "org1", "repo1", "ccc", auditResultOpts{isBot: true, reasons: []string{"no associated pull request"}})
	insertAuditResultFull(t, db, "org1", "repo1", "ddd", auditResultOpts{hasPR: true, hasApproval: true, isSelfApproved: true, prNumber: 1, approvers: []string{"dev2"}, reasons: []string{"self-approved"}})
	insertAuditResultFull(t, db, "org1", "repo1", "eee", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: true, prNumber: 2, approvers: []string{"reviewer1"}, reasons: []string{"compliant"}})

	r := New(db)
	summary, err := r.GetSummary(context.Background(), ReportOpts{})
	require.NoError(t, err, "GetSummary")
	require.Len(t, summary, 1)
	s := summary[0]

	// Primary partition: Compliant + Non-Compliant must equal Total
	assert.Equal(t, s.TotalCommits, s.CompliantCount+s.NonCompliantCount, "partition broken")

	assert.Equal(t, 5, s.TotalCommits, "total")
	assert.Equal(t, 3, s.CompliantCount, "compliant (exempt bot + empty + normal)")
	assert.Equal(t, 2, s.NonCompliantCount, "non-compliant (non-exempt bot + self-approved)")

	// Annotation counts overlap with primary partition
	assert.Equal(t, 2, s.BotCount, "bots (one compliant, one not)")
	assert.Equal(t, 1, s.ExemptCount, "exempt")
	assert.Equal(t, 1, s.EmptyCount, "empty")
	assert.Equal(t, 1, s.SelfApprovedCount, "self_approved")
}

func TestGetSummaryRespectsSinceUntil(t *testing.T) {
	db := setupTestDB(t)

	old := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	insertCommit(t, db, "org1", "repo1", "old1", "dev1", old, 10, 5)
	insertCommit(t, db, "org1", "repo1", "new1", "dev1", recent, 10, 5)

	insertAuditResult(t, db, "org1", "repo1", "old1", false, false, true, true, true, 1, nil, []string{"compliant"})
	insertAuditResult(t, db, "org1", "repo1", "new1", false, false, true, true, true, 2, nil, []string{"compliant"})

	r := New(db)
	summary, err := r.GetSummary(context.Background(), ReportOpts{
		Since: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err, "GetSummary")
	require.Len(t, summary, 1)
	assert.Equal(t, 1, summary[0].TotalCommits)
}

func TestGetSummaryRespectsOrgRepoFilters(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa", "dev1", now, 10, 5)
	insertCommit(t, db, "org2", "repo2", "bbb", "dev1", now, 10, 5)

	insertAuditResult(t, db, "org1", "repo1", "aaa", false, false, true, true, true, 1, nil, []string{"compliant"})
	insertAuditResult(t, db, "org2", "repo2", "bbb", false, false, true, true, true, 2, nil, []string{"compliant"})

	r := New(db)
	summary, err := r.GetSummary(context.Background(), ReportOpts{Org: "org1"})
	require.NoError(t, err, "GetSummary")
	require.Len(t, summary, 1)
	assert.Equal(t, "org1", summary[0].Org)
}

func TestGetSummaryMultiRepoFilter(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "nodejs", "node", "aaa", "dev1", now, 10, 5)
	insertCommit(t, db, "rails", "rails", "bbb", "dev1", now, 10, 5)
	insertCommit(t, db, "other", "stuff", "ccc", "dev1", now, 10, 5)

	insertAuditResult(t, db, "nodejs", "node", "aaa", false, false, true, true, true, 1, nil, []string{"compliant"})
	insertAuditResult(t, db, "rails", "rails", "bbb", false, false, true, true, true, 2, nil, []string{"compliant"})
	insertAuditResult(t, db, "other", "stuff", "ccc", false, false, false, false, false, 0, nil, []string{"no associated pull request"})

	r := New(db)

	// Filter to just nodejs/node and rails/rails
	summary, err := r.GetSummary(context.Background(), ReportOpts{
		Repos: []RepoFilter{
			{Org: "nodejs", Repo: "node"},
			{Org: "rails", Repo: "rails"},
		},
	})
	require.NoError(t, err, "GetSummary")
	require.Len(t, summary, 2)

	orgs := map[string]bool{}
	for _, s := range summary {
		orgs[s.Org+"/"+s.Repo] = true
	}
	assert.True(t, orgs["nodejs/node"], "expected nodejs/node in results")
	assert.True(t, orgs["rails/rails"], "expected rails/rails in results")

	// Verify details also filtered
	details, err := r.GetDetails(context.Background(), ReportOpts{
		Repos: []RepoFilter{
			{Org: "nodejs", Repo: "node"},
		},
	})
	require.NoError(t, err, "GetDetails")
	require.Len(t, details, 1)
	assert.Equal(t, "nodejs", details[0].Org)
	assert.Equal(t, "node", details[0].Repo)
}

func TestGetDetailsReturnsAllFields(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa", "dev1", now, 10, 5)
	insertAuditResult(t, db, "org1", "repo1", "aaa", false, false, true, true, true, 42, []string{"reviewer1", "reviewer2"}, []string{"compliant"})

	r := New(db)
	details, err := r.GetDetails(context.Background(), ReportOpts{})
	require.NoError(t, err, "GetDetails")
	require.Len(t, details, 1)

	d := details[0]
	assert.Equal(t, "org1", d.Org)
	assert.Equal(t, "repo1", d.Repo)
	assert.Equal(t, "aaa", d.SHA)
	assert.Equal(t, "dev1", d.AuthorLogin)
	assert.Equal(t, 42, d.PRNumber)
	assert.True(t, d.IsCompliant)
	assert.Contains(t, d.ApproverLogins, "reviewer1")
	assert.Contains(t, d.ApproverLogins, "reviewer2")
}

func TestGetDetailsOnlyFailuresFilter(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "bbb", "dev2", now, 10, 5)

	insertAuditResult(t, db, "org1", "repo1", "aaa", false, false, true, true, true, 1, nil, []string{"compliant"})
	insertAuditResult(t, db, "org1", "repo1", "bbb", false, false, false, false, false, 0, nil, []string{"no associated pull request"})

	r := New(db)
	details, err := r.GetDetails(context.Background(), ReportOpts{OnlyFailures: true})
	require.NoError(t, err, "GetDetails")
	require.Len(t, details, 1)
	assert.Equal(t, "bbb", details[0].SHA)
}

func TestFormatTable(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	r := New(nil) // no DB needed for formatting

	summary := []SummaryRow{
		{Org: "org1", Repo: "repo1", TotalCommits: 10, CompliantCount: 8, NonCompliantCount: 2, CompliancePct: 80.0},
	}
	details := []DetailRow{
		{Org: "org1", Repo: "repo1", SHA: "abc1234567", AuthorLogin: "dev1", CommittedAt: now, IsCompliant: true, Reasons: "compliant"},
	}

	var buf bytes.Buffer
	err := r.FormatTable(&buf, summary, details)
	require.NoError(t, err, "FormatTable")

	output := buf.String()
	assert.Contains(t, output, "SUMMARY")
	assert.Contains(t, output, "DETAILS")
	assert.Contains(t, output, "80.0%")
}

func TestFormatCSV(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	r := New(nil)

	details := []DetailRow{
		{Org: "org1", Repo: "repo1", SHA: "abc123", AuthorLogin: "dev1", CommittedAt: now, IsCompliant: true, Reasons: "compliant"},
	}

	var buf bytes.Buffer
	err := r.FormatCSV(&buf, details)
	require.NoError(t, err, "FormatCSV")

	reader := csv.NewReader(&buf)
	records, err := reader.ReadAll()
	require.NoError(t, err, "parsing CSV")
	require.Len(t, records, 2) // header + 1 row
	assert.Equal(t, "Org", records[0][0])
}

func TestFormatJSON(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	r := New(nil)

	summary := []SummaryRow{
		{Org: "org1", Repo: "repo1", TotalCommits: 10, CompliantCount: 8},
	}
	details := []DetailRow{
		{Org: "org1", Repo: "repo1", SHA: "abc123", AuthorLogin: "dev1", CommittedAt: now},
	}

	var buf bytes.Buffer
	err := r.FormatJSON(&buf, summary, details)
	require.NoError(t, err, "FormatJSON")

	var result map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result), "invalid JSON")
	assert.Contains(t, result, "summary")
	assert.Contains(t, result, "details")
}

func TestGenerateXLSXCreatesFile(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "bbb", "dev2", now, 10, 5)
	insertAuditResult(t, db, "org1", "repo1", "aaa", false, false, true, true, true, 1, []string{"reviewer1"}, []string{"compliant"})
	insertAuditResult(t, db, "org1", "repo1", "bbb", false, false, false, false, false, 0, nil, []string{"no associated pull request"})

	r := New(db)

	tmpFile := t.TempDir() + "/test-report.xlsx"
	err := r.GenerateXLSX(context.Background(), ReportOpts{}, tmpFile)
	require.NoError(t, err, "GenerateXLSX")

	info, err := os.Stat(tmpFile)
	require.NoError(t, err, "file not created")
	assert.Greater(t, info.Size(), int64(0))
}

func TestGenerateXLSXLargeDataset(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	// Insert 1000 rows
	for i := 0; i < 1000; i++ {
		sha := fmt.Sprintf("sha%06d", i)
		insertCommit(t, db, "org1", "repo1", sha, "dev1", now.Add(time.Duration(-i)*time.Minute), 10, 5)
		isCompliant := i%3 != 0
		reasons := []string{"compliant"}
		if !isCompliant {
			reasons = []string{"no associated pull request"}
		}
		insertAuditResult(t, db, "org1", "repo1", sha, false, false, isCompliant, isCompliant, isCompliant, i+1, nil, reasons)
	}

	r := New(db)

	tmpFile := t.TempDir() + "/test-large-report.xlsx"
	err := r.GenerateXLSX(context.Background(), ReportOpts{}, tmpFile)
	require.NoError(t, err, "GenerateXLSX with 1000 rows")

	info, err := os.Stat(tmpFile)
	require.NoError(t, err, "file not created")
	assert.Greater(t, info.Size(), int64(0))
}

func TestGenerateXLSXHasExpectedSheets(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa111aaa", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "bbb222bbb", "dev2", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "ccc333ccc", "bot1", now, 0, 0)
	insertCommitBranch(t, db, "org1", "repo1", "aaa111aaa", "main")

	insertAuditResultFull(t, db, "org1", "repo1", "aaa111aaa", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: true, isSelfApproved: true, prNumber: 1, approvers: []string{"dev1"}, reasons: []string{"compliant"}})
	insertAuditResult(t, db, "org1", "repo1", "bbb222bbb", false, false, false, false, false, 0, nil, []string{"no associated pull request"})
	insertAuditResult(t, db, "org1", "repo1", "ccc333ccc", false, true, false, false, true, 0, nil, []string{"empty commit"})

	r := New(db)

	tmpFile := t.TempDir() + "/test-five-sheets.xlsx"
	err := r.GenerateXLSX(context.Background(), ReportOpts{}, tmpFile)
	require.NoError(t, err, "GenerateXLSX")

	xf, err := excelize.OpenFile(tmpFile)
	require.NoError(t, err, "opening xlsx")
	defer xf.Close()

	sheets := xf.GetSheetList()
	expected := []string{SheetREADME, SheetActionQueue, SheetSummary, SheetByRule, SheetByAuthor, SheetDecisionMatrix, SheetWaiversLog, SheetMultiplePRs}
	require.Len(t, sheets, len(expected))
	for i, name := range expected {
		assert.Equal(t, name, sheets[i], "sheet %d", i)
	}
}

// Self-approved commits are a compliance failure; they surface as rows in the
// Action Queue with rule R5 SelfApproval.
func TestSelfApprovedAppearsInActionQueue(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "selfaaa11", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "normalbbb", "dev2", now, 10, 5)

	// Self-approved = non-compliant (only approver is the author).
	insertAuditResultFull(t, db, "org1", "repo1", "selfaaa11", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: false, isSelfApproved: true, prNumber: 1, approvers: []string{"dev1"}, reasons: []string{"self-approved"}})
	insertAuditResultFull(t, db, "org1", "repo1", "normalbbb", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: true, prNumber: 2, approvers: []string{"reviewer1"}, reasons: []string{"compliant"}})

	r := New(db)

	tmpFile := t.TempDir() + "/test-self-approved.xlsx"
	err := r.GenerateXLSX(context.Background(), ReportOpts{}, tmpFile)
	require.NoError(t, err, "GenerateXLSX")

	xf, err := excelize.OpenFile(tmpFile)
	require.NoError(t, err, "opening xlsx")
	defer xf.Close()

	rows, err := xf.GetRows(SheetActionQueue)
	require.NoError(t, err, "getting Action Queue rows")
	require.Len(t, rows, 2, "1 header + 1 self-approved row")

	// SHA column is D (4th).
	formula, _ := xf.GetCellFormula(SheetActionQueue, "D2")
	assert.Contains(t, formula, "selfaaa1", "expected self-approved SHA in formula, got: %s", formula)
	// Failing Rule column is H (8th).
	rule, _ := xf.GetCellValue(SheetActionQueue, "H2")
	assert.Contains(t, rule, "R5", "expected R5 SelfApproval rule, got: %s", rule)
}

func TestSummarySelfApprovedCount(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa111aaa", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "bbb222bbb", "dev2", now, 10, 5)

	insertAuditResultFull(t, db, "org1", "repo1", "aaa111aaa", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: true, isSelfApproved: true, prNumber: 1, approvers: []string{"dev1"}, reasons: []string{"compliant"}})
	insertAuditResultFull(t, db, "org1", "repo1", "bbb222bbb", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: true, prNumber: 2, approvers: []string{"reviewer1"}, reasons: []string{"compliant"}})

	r := New(db)
	summary, err := r.GetSummary(context.Background(), ReportOpts{})
	require.NoError(t, err, "GetSummary")
	require.Len(t, summary, 1)
	assert.Equal(t, 1, summary[0].SelfApprovedCount)
}

func TestHyperlinksOnNonStreamingSheets(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "abc12345678", "dev1", now, 10, 5)
	insertAuditResult(t, db, "org1", "repo1", "abc12345678", false, false, false, false, false, 0, nil, []string{"no associated pull request"})

	r := New(db)

	tmpFile := t.TempDir() + "/test-hyperlinks.xlsx"
	err := r.GenerateXLSX(context.Background(), ReportOpts{}, tmpFile)
	require.NoError(t, err, "GenerateXLSX")

	xf, err := excelize.OpenFile(tmpFile)
	require.NoError(t, err, "opening xlsx")
	defer xf.Close()

	// Action Queue SHA column is D (4th); non-compliant no-PR commit should
	// land there with a HYPERLINK formula.
	formula, err := xf.GetCellFormula(SheetActionQueue, "D2")
	require.NoError(t, err, "getting formula D2")
	assert.Contains(t, formula, "abc12345", "expected SHA in HYPERLINK formula")
	assert.Contains(t, formula, "https://github.com/org1/repo1/commit/abc12345678", "expected commit URL in HYPERLINK formula")
}

func TestEmptyActionQueueSheetStillCreated(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	// All commits are compliant
	insertCommit(t, db, "org1", "repo1", "aaa111aaa", "dev1", now, 10, 5)
	insertAuditResult(t, db, "org1", "repo1", "aaa111aaa", false, false, true, true, true, 1, []string{"reviewer1"}, []string{"compliant"})

	r := New(db)

	tmpFile := t.TempDir() + "/test-empty-aq.xlsx"
	err := r.GenerateXLSX(context.Background(), ReportOpts{}, tmpFile)
	require.NoError(t, err, "GenerateXLSX")

	xf, err := excelize.OpenFile(tmpFile)
	require.NoError(t, err, "opening xlsx")
	defer xf.Close()

	sheets := xf.GetSheetList()
	assert.Contains(t, sheets, SheetActionQueue, "Action Queue sheet should exist even when nothing needs action")

	rows, err := xf.GetRows(SheetActionQueue)
	require.NoError(t, err, "getting Action Queue rows")
	assert.Len(t, rows, 1, "expected header only in empty Action Queue sheet")
}

func TestDetailRowMergedByLogin(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa111aaa", "alice", now, 10, 5)
	insertAuditResult(t, db, "org1", "repo1", "aaa111aaa", false, false, true, true, true, 42, []string{"bob"}, []string{"compliant"})

	// Insert PR with merged_by_login
	_, err := db.Exec(`INSERT INTO pull_requests (org, repo, number, title, merged, head_sha, author_login, merged_by_login, merged_at, href)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"org1", "repo1", 42, "fix stuff", true, "aaa111aaa", "alice", "carol", now,
		"https://github.com/org1/repo1/pull/42")
	require.NoError(t, err, "insert PR")

	r := New(db)
	details, err := r.GetDetails(context.Background(), ReportOpts{})
	require.NoError(t, err, "GetDetails")
	require.Len(t, details, 1)
	assert.Equal(t, "carol", details[0].MergedByLogin)
}

func TestDetailRowMergedByLoginEmpty(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	// Commit with no PR (direct push) — merged_by should be empty
	insertCommit(t, db, "org1", "repo1", "bbb222bbb", "dave", now, 5, 2)
	insertAuditResult(t, db, "org1", "repo1", "bbb222bbb", false, false, false, false, false, 0, nil, []string{"no associated pull request"})

	r := New(db)
	details, err := r.GetDetails(context.Background(), ReportOpts{})
	require.NoError(t, err, "GetDetails")
	require.Len(t, details, 1)
	assert.Empty(t, details[0].MergedByLogin)
}

func TestSummaryStaleApprovalCount(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa111aaa", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "bbb222bbb", "dev2", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "ccc333ccc", "dev3", now, 10, 5)

	insertAuditResultFull(t, db, "org1", "repo1", "aaa111aaa", auditResultOpts{hasPR: true, hasApproval: false, hasStaleApproval: true, prNumber: 1, reasons: []string{"approval is stale — not on final commit"}})
	insertAuditResultFull(t, db, "org1", "repo1", "bbb222bbb", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: true, prNumber: 2, approvers: []string{"reviewer1"}, reasons: []string{"compliant"}})
	insertAuditResultFull(t, db, "org1", "repo1", "ccc333ccc", auditResultOpts{hasPR: true, hasApproval: false, hasStaleApproval: true, prNumber: 3, reasons: []string{"approval is stale — not on final commit"}})

	r := New(db)
	summary, err := r.GetSummary(context.Background(), ReportOpts{})
	require.NoError(t, err, "GetSummary")
	require.Len(t, summary, 1)
	assert.Equal(t, 2, summary[0].StaleApprovalCount, "stale approval count")
}

func TestSummaryMultiplePRCount(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa111aaa", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "bbb222bbb", "dev2", now, 10, 5)

	insertAuditResultFull(t, db, "org1", "repo1", "aaa111aaa", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: true, prNumber: 1, prCount: 3, approvers: []string{"reviewer1"}, reasons: []string{"compliant"}})
	insertAuditResultFull(t, db, "org1", "repo1", "bbb222bbb", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: true, prNumber: 2, prCount: 1, approvers: []string{"reviewer1"}, reasons: []string{"compliant"}})

	r := New(db)
	summary, err := r.GetSummary(context.Background(), ReportOpts{})
	require.NoError(t, err, "GetSummary")
	require.Len(t, summary, 1)
	assert.Equal(t, 1, summary[0].MultiplePRCount, "multiple PR count")
}

func TestGetMultiplePRDetails(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "multipr111", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "singlepr22", "dev2", now, 10, 5)

	// Commit with 2 PRs
	insertAuditResultFull(t, db, "org1", "repo1", "multipr111", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: true, prNumber: 10, prCount: 2, approvers: []string{"reviewer1"}, reasons: []string{"compliant"}})
	// Commit with 1 PR
	insertAuditResultFull(t, db, "org1", "repo1", "singlepr22", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: true, prNumber: 20, prCount: 1, approvers: []string{"reviewer1"}, reasons: []string{"compliant"}})

	// Insert commit_prs associations
	_, err := db.Exec(`INSERT INTO commit_prs (org, repo, sha, pr_number) VALUES (?, ?, ?, ?), (?, ?, ?, ?)`,
		"org1", "repo1", "multipr111", 10, "org1", "repo1", "multipr111", 11)
	require.NoError(t, err, "insert commit_prs")

	_, err = db.Exec(`INSERT INTO commit_prs (org, repo, sha, pr_number) VALUES (?, ?, ?, ?)`,
		"org1", "repo1", "singlepr22", 20)
	require.NoError(t, err, "insert commit_prs single")

	// Insert pull_requests
	_, err = db.Exec(`INSERT INTO pull_requests (org, repo, number, title, merged, head_sha, author_login, merged_by_login, merged_at, href)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"org1", "repo1", 10, "PR ten", true, "multipr111", "dev1", "merger1", now, "https://github.com/org1/repo1/pull/10",
		"org1", "repo1", 11, "PR eleven", true, "multipr111", "dev1", "merger2", now, "https://github.com/org1/repo1/pull/11")
	require.NoError(t, err, "insert PRs")

	r := New(db)
	rows, err := r.GetMultiplePRDetails(context.Background(), ReportOpts{})
	require.NoError(t, err, "GetMultiplePRDetails")

	// Should return 2 rows (one per PR for the multi-PR commit), not the single-PR commit
	require.Len(t, rows, 2, "expected 2 rows for multi-PR commit")
	assert.Equal(t, "multipr111", rows[0].SHA)
	assert.Equal(t, 2, rows[0].PRCount)

	// One row should be the audited PR (10), the other not (11)
	auditedCount := 0
	for _, row := range rows {
		if row.IsAuditedPR {
			auditedCount++
			assert.Equal(t, 10, row.PRNumber)
		}
	}
	assert.Equal(t, 1, auditedCount, "exactly one row should be the audited PR")
}

// Stale approvals surface in the Decision Matrix with R4b Stale = fail,
// filterable alongside every other commit in one sheet.
func TestStaleApprovalSurfacesInDecisionMatrix(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "staleaaa11", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "normalbbb2", "dev2", now.Add(-time.Hour), 10, 5)
	insertCommitBranch(t, db, "org1", "repo1", "staleaaa11", "main")

	insertAuditResultFull(t, db, "org1", "repo1", "staleaaa11", auditResultOpts{hasPR: true, hasApproval: false, hasStaleApproval: true, prNumber: 1, approvers: []string{"old-reviewer"}, reasons: []string{"approval is stale"}})
	insertAuditResultFull(t, db, "org1", "repo1", "normalbbb2", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: true, prNumber: 2, approvers: []string{"reviewer1"}, reasons: []string{"compliant"}})

	r := New(db)

	tmpFile := t.TempDir() + "/test-stale.xlsx"
	err := r.GenerateXLSX(context.Background(), ReportOpts{}, tmpFile)
	require.NoError(t, err, "GenerateXLSX")

	xf, err := excelize.OpenFile(tmpFile)
	require.NoError(t, err, "opening xlsx")
	defer xf.Close()

	rows, err := xf.GetRows(SheetDecisionMatrix)
	require.NoError(t, err, "getting Decision Matrix rows")
	require.Len(t, rows, 3, "expected 1 header + 2 commit rows")

	// Rule R4b Stale is column N (14th: 9 identity + 5th rule column).
	// Find the stale row by SHA in column B (2nd).
	staleRow := -1
	for i, r := range rows {
		if i == 0 {
			continue
		}
		if formula, _ := xf.GetCellFormula(SheetDecisionMatrix, fmt.Sprintf("B%d", i+1)); strings.Contains(formula, "staleaaa") {
			staleRow = i + 1
			break
		}
		_ = r
	}
	require.Greater(t, staleRow, 0, "expected to find stale SHA in Decision Matrix")
	cell, _ := xf.GetCellValue(SheetDecisionMatrix, fmt.Sprintf("N%d", staleRow))
	assert.Equal(t, "fail", cell, "R4b Stale should read fail")
}

func TestDetailRowHasStaleApprovalAndPRCount(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa111aaa", "dev1", now, 10, 5)
	insertAuditResultFull(t, db, "org1", "repo1", "aaa111aaa", auditResultOpts{hasPR: true, hasStaleApproval: true, prNumber: 1, prCount: 2, reasons: []string{"approval is stale — not on final commit"}})

	r := New(db)
	details, err := r.GetDetails(context.Background(), ReportOpts{})
	require.NoError(t, err, "GetDetails")
	require.Len(t, details, 1)

	assert.True(t, details[0].HasStaleApproval, "HasStaleApproval")
	assert.Equal(t, 2, details[0].PRCount, "PRCount")
}

func TestDetailRowBranchName(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa111aaa", "dev1", now, 10, 5)
	insertCommitBranch(t, db, "org1", "repo1", "aaa111aaa", "main")
	insertAuditResult(t, db, "org1", "repo1", "aaa111aaa", false, false, true, true, true, 1, []string{"reviewer1"}, []string{"compliant"})

	r := New(db)
	details, err := r.GetDetails(context.Background(), ReportOpts{})
	require.NoError(t, err, "GetDetails")
	require.Len(t, details, 1)
	assert.Equal(t, "main", details[0].BranchName)
}

func TestGlobsToRegex(t *testing.T) {
	cases := []struct {
		name   string
		globs  []string
		want   string
		should []string // branch names that must match
		shouldNot []string // branch names that must not match
	}{
		{
			name:  "default when empty",
			globs: nil,
			want:  "^(master|main)$",
			should:    []string{"master", "main"},
			shouldNot: []string{"develop", "release/1.0"},
		},
		{
			name:  "exact names only",
			globs: []string{"master", "trunk"},
			want:  "^(master|trunk)$",
			should:    []string{"master", "trunk"},
			shouldNot: []string{"main", "mastering"},
		},
		{
			name:      "release/* and HF_BF_* patterns",
			globs:     []string{"master", "main", "release/*", "HF_BF_*"},
			want:      `^(master|main|release/.*|HF_BF_.*)$`,
			should:    []string{"master", "main", "release/2026-q1", "release/", "HF_BF_123", "HF_BF_"},
			shouldNot: []string{"dev", "feature/release/x", "hf_bf_123" /* case-sensitive */},
		},
		{
			name:      "both casings listed explicitly",
			globs:     []string{"HF_BF_*", "hf_bf_*"},
			want:      `^(HF_BF_.*|hf_bf_.*)$`,
			should:    []string{"HF_BF_1", "hf_bf_2"},
			shouldNot: []string{"Hf_Bf_1"},
		},
		{
			name:   "underscores in pattern are literal (not LIKE wildcards)",
			globs:  []string{"HF_BF_*"},
			want:   `^(HF_BF_.*)$`,
			should: []string{"HF_BF_x"},
			// Without proper regex escaping, "HFXBFYZ" would match a LIKE
			// pattern whose `_` is a single-char wildcard. Regex treats `_`
			// literally, so this must not match.
			shouldNot: []string{"HFXBFYZ"},
		},
		{
			name:      "regex metacharacters escaped",
			globs:     []string{"feature.branch", "a+b", "v1.0"},
			want:      `^(feature\.branch|a\+b|v1\.0)$`,
			should:    []string{"feature.branch", "a+b", "v1.0"},
			shouldNot: []string{"featureXbranch", "ab", "v1X0"},
		},
		{
			name:      "? matches single char",
			globs:     []string{"v1.?"},
			want:      `^(v1\..)$`,
			should:    []string{"v1.0", "v1.9"},
			shouldNot: []string{"v1.", "v1.10"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := globsToRegex(tc.globs)
			assert.Equal(t, tc.want, got)

			re, err := regexp.Compile(got)
			require.NoError(t, err, "generated regex must compile")
			for _, s := range tc.should {
				assert.True(t, re.MatchString(s), "expected %q to match %s", s, got)
			}
			for _, s := range tc.shouldNot {
				assert.False(t, re.MatchString(s), "expected %q to NOT match %s", s, got)
			}
		})
	}
}

func TestReporterWithCustomBranches(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	// Insert 3 commits on different branches. Each helper call inserts a
	// default 'master' row in commit_branches; we remove that and set the
	// intended branch explicitly.
	for _, c := range []struct {
		sha, branch string
	}{
		{"mastercommit", "master"},
		{"releasecommit1", "release/2026-q1"},
		{"hotfixcommit1", "HF_BF_urgent"},
		{"featurecommit", "feature/xyz"},
	} {
		insertCommit(t, db, "org", "repo", c.sha, "dev", now, 5, 2)
		insertAuditResult(t, db, "org", "repo", c.sha, false, false, true, true, true, 1, []string{"r1"}, []string{"compliant"})
		_, err := db.Exec(`DELETE FROM commit_branches WHERE sha=?`, c.sha)
		require.NoError(t, err)
		insertCommitBranch(t, db, "org", "repo", c.sha, c.branch)
	}

	// Default reporter keeps only 'master'.
	rDefault := New(db)
	details, err := rDefault.GetDetails(context.Background(), ReportOpts{})
	require.NoError(t, err)
	require.Len(t, details, 1)
	assert.Equal(t, "mastercommit", details[0].SHA)

	// Custom branch list widens the scope.
	rCustom := NewWithBranches(db, []string{"master", "main", "release/*", "HF_BF_*"})
	details, err = rCustom.GetDetails(context.Background(), ReportOpts{})
	require.NoError(t, err)
	shas := make(map[string]bool)
	for _, d := range details {
		shas[d.SHA] = true
	}
	assert.True(t, shas["mastercommit"], "master commit visible")
	assert.True(t, shas["releasecommit1"], "release/* commit visible")
	assert.True(t, shas["hotfixcommit1"], "HF_BF_* commit visible")
	assert.False(t, shas["featurecommit"], "feature/xyz commit still excluded")
}

// Regression guard: re-audit can populate audit_results rows for PR-branch
// commits that never landed on the default branch. The report layer must
// filter those out so All Commits / Non-Compliant / Summary aren't polluted
// with false-positive "no associated PR" rows on ephemeral feature commits.
func TestGetDetailsFiltersPRBranchOnlyCommits(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	// Real default-branch commit: inserted by helper with branch='master'.
	insertCommit(t, db, "org1", "repo1", "mainsha01", "dev1", now, 10, 5)
	insertAuditResult(t, db, "org1", "repo1", "mainsha01", false, false, true, true, true, 1, []string{"r1"}, []string{"compliant"})

	// PR-branch-only commit: overwrite the helper's default 'master' row by
	// swapping to a feature branch. This simulates a re-audit that wrote an
	// audit_results row for a commit that never reached master.
	insertCommit(t, db, "org1", "repo1", "branchsha1", "dev2", now, 3, 2)
	insertAuditResult(t, db, "org1", "repo1", "branchsha1", false, false, false, false, false, 0, nil, []string{"no associated pull request"})
	_, err := db.Exec(`DELETE FROM commit_branches WHERE sha='branchsha1'`)
	require.NoError(t, err)
	insertCommitBranch(t, db, "org1", "repo1", "branchsha1", "feature/xyz")

	r := New(db)

	// GetDetails: only the master-branch commit shows up.
	details, err := r.GetDetails(context.Background(), ReportOpts{})
	require.NoError(t, err)
	require.Len(t, details, 1)
	assert.Equal(t, "mainsha01", details[0].SHA)

	// GetSummary: counts only reflect the master-branch commit.
	summary, err := r.GetSummary(context.Background(), ReportOpts{})
	require.NoError(t, err)
	require.Len(t, summary, 1)
	assert.Equal(t, 1, summary[0].TotalCommits)
	assert.Equal(t, 1, summary[0].CompliantCount)
	assert.Equal(t, 0, summary[0].NonCompliantCount)
}
