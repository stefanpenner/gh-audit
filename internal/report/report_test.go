package report

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
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
	approver_logins      TEXT[],
	owner_approval_check TEXT,
	is_compliant         BOOLEAN,
	reasons              TEXT[],
	commit_href          TEXT,
	pr_href              TEXT,
	is_self_approved     BOOLEAN,
	audited_at           TIMESTAMP DEFAULT current_timestamp,
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

func TestGenerateXLSXHasSevenSheets(t *testing.T) {
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
	expected := []string{"Summary", "All Commits", "Non-Compliant", "Exemptions", "Self-Approved", "Stale Approvals", "Multiple PRs"}
	require.Len(t, sheets, len(expected))
	for i, name := range expected {
		assert.Equal(t, name, sheets[i], "sheet %d", i)
	}
}

func TestSelfApprovedSheetContainsOnlySelfApproved(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "selfaaa11", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "normalbbb", "dev2", now, 10, 5)

	insertAuditResultFull(t, db, "org1", "repo1", "selfaaa11", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: true, isSelfApproved: true, prNumber: 1, approvers: []string{"dev1"}, reasons: []string{"self-approved"}})
	insertAuditResultFull(t, db, "org1", "repo1", "normalbbb", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: true, prNumber: 2, approvers: []string{"reviewer1"}, reasons: []string{"compliant"}})

	r := New(db)

	tmpFile := t.TempDir() + "/test-self-approved.xlsx"
	err := r.GenerateXLSX(context.Background(), ReportOpts{}, tmpFile)
	require.NoError(t, err, "GenerateXLSX")

	xf, err := excelize.OpenFile(tmpFile)
	require.NoError(t, err, "opening xlsx")
	defer xf.Close()

	rows, err := xf.GetRows("Self-Approved")
	require.NoError(t, err, "getting Self-Approved rows")
	require.Len(t, rows, 2) // 1 header + 1 data row

	found := false
	for _, cell := range rows[1] {
		if cell == "selfaaa1" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected self-approved SHA prefix 'selfaaa1' in row, got: %v", rows[1])
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

	// Non-Compliant sheet should have a hyperlink on SHA cell (C2)
	val, err := xf.GetCellValue("Non-Compliant", "C2")
	require.NoError(t, err, "getting cell C2")
	assert.Equal(t, "abc12345", val)

	// Check hyperlink exists
	hasLink, target, err := xf.GetCellHyperLink("Non-Compliant", "C2")
	require.NoError(t, err, "getting hyperlink")
	assert.True(t, hasLink, "expected hyperlink on SHA cell C2 in Non-Compliant sheet")
	assert.Equal(t, "https://github.com/org1/repo1/commit/abc12345678", target)
}

func TestEmptyNonCompliantSheetStillCreated(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	// All commits are compliant
	insertCommit(t, db, "org1", "repo1", "aaa111aaa", "dev1", now, 10, 5)
	insertAuditResult(t, db, "org1", "repo1", "aaa111aaa", false, false, true, true, true, 1, []string{"reviewer1"}, []string{"compliant"})

	r := New(db)

	tmpFile := t.TempDir() + "/test-empty-nc.xlsx"
	err := r.GenerateXLSX(context.Background(), ReportOpts{}, tmpFile)
	require.NoError(t, err, "GenerateXLSX")

	xf, err := excelize.OpenFile(tmpFile)
	require.NoError(t, err, "opening xlsx")
	defer xf.Close()

	sheets := xf.GetSheetList()
	assert.Contains(t, sheets, "Non-Compliant", "Non-Compliant sheet should exist even with zero non-compliant rows")

	// Should have header row only
	rows, err := xf.GetRows("Non-Compliant")
	require.NoError(t, err, "getting Non-Compliant rows")
	assert.Len(t, rows, 1, "expected header only in empty Non-Compliant sheet")
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

func TestStaleApprovalsSheetContent(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "staleaaa11", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "normalbbb2", "dev2", now, 10, 5)
	insertCommitBranch(t, db, "org1", "repo1", "staleaaa11", "main")

	insertAuditResultFull(t, db, "org1", "repo1", "staleaaa11", auditResultOpts{hasPR: true, hasApproval: false, hasStaleApproval: true, prNumber: 1, approvers: []string{"old-reviewer"}, reasons: []string{"approval is stale — not on final commit"}})
	insertAuditResultFull(t, db, "org1", "repo1", "normalbbb2", auditResultOpts{hasPR: true, hasApproval: true, isCompliant: true, prNumber: 2, approvers: []string{"reviewer1"}, reasons: []string{"compliant"}})

	r := New(db)

	tmpFile := t.TempDir() + "/test-stale.xlsx"
	err := r.GenerateXLSX(context.Background(), ReportOpts{}, tmpFile)
	require.NoError(t, err, "GenerateXLSX")

	xf, err := excelize.OpenFile(tmpFile)
	require.NoError(t, err, "opening xlsx")
	defer xf.Close()

	rows, err := xf.GetRows("Stale Approvals")
	require.NoError(t, err, "getting Stale Approvals rows")
	require.Len(t, rows, 2, "expected 1 header + 1 data row")

	found := false
	for _, cell := range rows[1] {
		if cell == "staleaaa" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected stale SHA prefix in row, got: %v", rows[1])
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
