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
	org            TEXT NOT NULL,
	repo           TEXT NOT NULL,
	sha            TEXT NOT NULL,
	author_login   TEXT,
	author_email   TEXT,
	committed_at   TIMESTAMP,
	message        TEXT,
	parent_count   INTEGER,
	additions      INTEGER,
	deletions      INTEGER,
	href           TEXT,
	fetched_at     TIMESTAMP DEFAULT current_timestamp,
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
	has_pr               BOOLEAN,
	pr_number            INTEGER,
	has_final_approval   BOOLEAN,
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
	if err != nil {
		t.Fatalf("opening duckdb: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, stmt := range strings.Split(schemaDDL, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("schema exec: %v\nSQL: %s", err, stmt)
		}
	}

	return db
}

func insertCommit(t *testing.T, db *sql.DB, org, repo, sha, author string, committedAt time.Time, additions, deletions int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO commits (org, repo, sha, author_login, committed_at, message, parent_count, additions, deletions, href)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		org, repo, sha, author, committedAt, "commit "+sha, 1, additions, deletions,
		fmt.Sprintf("https://github.com/%s/%s/commit/%s", org, repo, sha))
	if err != nil {
		t.Fatalf("insert commit: %v", err)
	}
}

func insertAuditResult(t *testing.T, db *sql.DB, org, repo, sha string, isBot, isEmpty, hasPR, hasApproval, isCompliant bool, prNumber int, approvers []string, reasons []string) {
	t.Helper()
	insertAuditResultFull(t, db, org, repo, sha, isBot, isEmpty, hasPR, hasApproval, isCompliant, false, prNumber, approvers, reasons)
}

func insertAuditResultFull(t *testing.T, db *sql.DB, org, repo, sha string, isBot, isEmpty, hasPR, hasApproval, isCompliant, isSelfApproved bool, prNumber int, approvers []string, reasons []string) {
	t.Helper()

	approverExpr := "list_value()"
	if len(approvers) > 0 {
		quoted := make([]string, len(approvers))
		for i, a := range approvers {
			quoted[i] = fmt.Sprintf("'%s'", a)
		}
		approverExpr = fmt.Sprintf("list_value(%s)", strings.Join(quoted, ", "))
	}

	reasonExpr := "list_value()"
	if len(reasons) > 0 {
		quoted := make([]string, len(reasons))
		for i, r := range reasons {
			quoted[i] = fmt.Sprintf("'%s'", r)
		}
		reasonExpr = fmt.Sprintf("list_value(%s)", strings.Join(quoted, ", "))
	}

	q := fmt.Sprintf(`INSERT INTO audit_results (org, repo, sha, is_empty_commit, is_bot, has_pr, pr_number, has_final_approval, approver_logins, owner_approval_check, is_compliant, reasons, commit_href, pr_href, is_self_approved)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, %s, ?, ?, %s, ?, ?, ?)`, approverExpr, reasonExpr)

	_, err := db.Exec(q,
		org, repo, sha, isEmpty, isBot, hasPR, prNumber, hasApproval,
		"success", isCompliant,
		fmt.Sprintf("https://github.com/%s/%s/commit/%s", org, repo, sha),
		fmt.Sprintf("https://github.com/%s/%s/pull/%d", org, repo, prNumber),
		isSelfApproved)
	if err != nil {
		t.Fatalf("insert audit result: %v", err)
	}
}

func insertCommitBranch(t *testing.T, db *sql.DB, org, repo, sha, branch string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO commit_branches (org, repo, sha, branch) VALUES (?, ?, ?, ?)`,
		org, repo, sha, branch)
	if err != nil {
		t.Fatalf("insert commit branch: %v", err)
	}
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
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}

	if len(summary) != 2 {
		t.Fatalf("expected 2 summary rows, got %d", len(summary))
	}

	// repo1: 3 total, 2 compliant, 1 non-compliant, 0 bots, 1 empty
	s := summary[0]
	if s.TotalCommits != 3 || s.CompliantCount != 2 || s.NonCompliantCount != 1 || s.EmptyCount != 1 {
		t.Errorf("repo1 summary: total=%d, compliant=%d, non=%d, empty=%d",
			s.TotalCommits, s.CompliantCount, s.NonCompliantCount, s.EmptyCount)
	}

	// repo2: 1 total, 1 compliant
	s = summary[1]
	if s.TotalCommits != 1 || s.CompliantCount != 1 {
		t.Errorf("repo2 summary: total=%d, compliant=%d", s.TotalCommits, s.CompliantCount)
	}
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
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}

	if len(summary) != 1 {
		t.Fatalf("expected 1 summary row, got %d", len(summary))
	}
	if summary[0].TotalCommits != 1 {
		t.Errorf("expected 1 commit, got %d", summary[0].TotalCommits)
	}
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
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}

	if len(summary) != 1 {
		t.Fatalf("expected 1 summary row, got %d", len(summary))
	}
	if summary[0].Org != "org1" {
		t.Errorf("expected org1, got %s", summary[0].Org)
	}
}

func TestGetDetailsReturnsAllFields(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa", "dev1", now, 10, 5)
	insertAuditResult(t, db, "org1", "repo1", "aaa", false, false, true, true, true, 42, []string{"reviewer1", "reviewer2"}, []string{"compliant"})

	r := New(db)
	details, err := r.GetDetails(context.Background(), ReportOpts{})
	if err != nil {
		t.Fatalf("GetDetails: %v", err)
	}

	if len(details) != 1 {
		t.Fatalf("expected 1 detail row, got %d", len(details))
	}

	d := details[0]
	if d.Org != "org1" || d.Repo != "repo1" || d.SHA != "aaa" {
		t.Errorf("wrong identity: %s/%s/%s", d.Org, d.Repo, d.SHA)
	}
	if d.AuthorLogin != "dev1" {
		t.Errorf("author = %s, want dev1", d.AuthorLogin)
	}
	if d.PRNumber != 42 {
		t.Errorf("pr_number = %d, want 42", d.PRNumber)
	}
	if !d.IsCompliant {
		t.Error("expected compliant")
	}
	if !strings.Contains(d.ApproverLogins, "reviewer1") || !strings.Contains(d.ApproverLogins, "reviewer2") {
		t.Errorf("approvers = %s, expected reviewer1 and reviewer2", d.ApproverLogins)
	}
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
	if err != nil {
		t.Fatalf("GetDetails: %v", err)
	}

	if len(details) != 1 {
		t.Fatalf("expected 1 detail row, got %d", len(details))
	}
	if details[0].SHA != "bbb" {
		t.Errorf("expected SHA bbb, got %s", details[0].SHA)
	}
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
	if err != nil {
		t.Fatalf("FormatTable: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "SUMMARY") {
		t.Error("missing SUMMARY header")
	}
	if !strings.Contains(output, "DETAILS") {
		t.Error("missing DETAILS header")
	}
	if !strings.Contains(output, "80.0%") {
		t.Error("missing compliance percentage")
	}
}

func TestFormatCSV(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	r := New(nil)

	details := []DetailRow{
		{Org: "org1", Repo: "repo1", SHA: "abc123", AuthorLogin: "dev1", CommittedAt: now, IsCompliant: true, Reasons: "compliant"},
	}

	var buf bytes.Buffer
	err := r.FormatCSV(&buf, details)
	if err != nil {
		t.Fatalf("FormatCSV: %v", err)
	}

	reader := csv.NewReader(&buf)
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("parsing CSV: %v", err)
	}
	if len(records) != 2 { // header + 1 row
		t.Fatalf("expected 2 records, got %d", len(records))
	}
	if records[0][0] != "Org" {
		t.Errorf("expected header Org, got %s", records[0][0])
	}
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
	if err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := result["summary"]; !ok {
		t.Error("missing summary key")
	}
	if _, ok := result["details"]; !ok {
		t.Error("missing details key")
	}
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
	if err != nil {
		t.Fatalf("GenerateXLSX: %v", err)
	}

	// Verify file exists
	info, err := os.Stat(tmpFile)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if info.Size() == 0 {
		t.Error("file is empty")
	}
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
	if err != nil {
		t.Fatalf("GenerateXLSX with 1000 rows: %v", err)
	}

	info, err := os.Stat(tmpFile)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if info.Size() == 0 {
		t.Error("file is empty")
	}
}

func TestGenerateXLSXHasFiveSheets(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa111aaa", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "bbb222bbb", "dev2", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "ccc333ccc", "bot1", now, 0, 0)
	insertCommitBranch(t, db, "org1", "repo1", "aaa111aaa", "main")

	insertAuditResultFull(t, db, "org1", "repo1", "aaa111aaa", false, false, true, true, true, true, 1, []string{"dev1"}, []string{"compliant"})
	insertAuditResult(t, db, "org1", "repo1", "bbb222bbb", false, false, false, false, false, 0, nil, []string{"no associated pull request"})
	insertAuditResult(t, db, "org1", "repo1", "ccc333ccc", false, true, false, false, true, 0, nil, []string{"empty commit"})

	r := New(db)

	tmpFile := t.TempDir() + "/test-five-sheets.xlsx"
	err := r.GenerateXLSX(context.Background(), ReportOpts{}, tmpFile)
	if err != nil {
		t.Fatalf("GenerateXLSX: %v", err)
	}

	xf, err := excelize.OpenFile(tmpFile)
	if err != nil {
		t.Fatalf("opening xlsx: %v", err)
	}
	defer xf.Close()

	sheets := xf.GetSheetList()
	expected := []string{"Summary", "All Commits", "Non-Compliant", "Exemptions", "Self-Approved"}
	if len(sheets) != len(expected) {
		t.Fatalf("expected %d sheets, got %d: %v", len(expected), len(sheets), sheets)
	}
	for i, name := range expected {
		if sheets[i] != name {
			t.Errorf("sheet %d: expected %q, got %q", i, name, sheets[i])
		}
	}
}

func TestSelfApprovedSheetContainsOnlySelfApproved(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "selfaaa11", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "normalbbb", "dev2", now, 10, 5)

	// Self-approved commit
	insertAuditResultFull(t, db, "org1", "repo1", "selfaaa11", false, false, true, true, true, true, 1, []string{"dev1"}, []string{"self-approved"})
	// Normal commit
	insertAuditResultFull(t, db, "org1", "repo1", "normalbbb", false, false, true, true, true, false, 2, []string{"reviewer1"}, []string{"compliant"})

	r := New(db)

	tmpFile := t.TempDir() + "/test-self-approved.xlsx"
	err := r.GenerateXLSX(context.Background(), ReportOpts{}, tmpFile)
	if err != nil {
		t.Fatalf("GenerateXLSX: %v", err)
	}

	xf, err := excelize.OpenFile(tmpFile)
	if err != nil {
		t.Fatalf("opening xlsx: %v", err)
	}
	defer xf.Close()

	rows, err := xf.GetRows("Self-Approved")
	if err != nil {
		t.Fatalf("getting Self-Approved rows: %v", err)
	}

	// 1 header + 1 data row
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (header + 1 data), got %d", len(rows))
	}

	// Check that the self-approved commit SHA prefix is present
	found := false
	for _, cell := range rows[1] {
		if cell == "selfaaa1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected self-approved SHA prefix 'selfaaa1' in row, got: %v", rows[1])
	}
}

func TestSummarySelfApprovedCount(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa111aaa", "dev1", now, 10, 5)
	insertCommit(t, db, "org1", "repo1", "bbb222bbb", "dev2", now, 10, 5)

	insertAuditResultFull(t, db, "org1", "repo1", "aaa111aaa", false, false, true, true, true, true, 1, []string{"dev1"}, []string{"compliant"})
	insertAuditResultFull(t, db, "org1", "repo1", "bbb222bbb", false, false, true, true, true, false, 2, []string{"reviewer1"}, []string{"compliant"})

	r := New(db)
	summary, err := r.GetSummary(context.Background(), ReportOpts{})
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}

	if len(summary) != 1 {
		t.Fatalf("expected 1 summary row, got %d", len(summary))
	}
	if summary[0].SelfApprovedCount != 1 {
		t.Errorf("expected SelfApprovedCount=1, got %d", summary[0].SelfApprovedCount)
	}
}

func TestHyperlinksOnNonStreamingSheets(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "abc12345678", "dev1", now, 10, 5)
	insertAuditResult(t, db, "org1", "repo1", "abc12345678", false, false, false, false, false, 0, nil, []string{"no associated pull request"})

	r := New(db)

	tmpFile := t.TempDir() + "/test-hyperlinks.xlsx"
	err := r.GenerateXLSX(context.Background(), ReportOpts{}, tmpFile)
	if err != nil {
		t.Fatalf("GenerateXLSX: %v", err)
	}

	xf, err := excelize.OpenFile(tmpFile)
	if err != nil {
		t.Fatalf("opening xlsx: %v", err)
	}
	defer xf.Close()

	// Non-Compliant sheet should have a hyperlink on SHA cell (C2)
	val, err := xf.GetCellValue("Non-Compliant", "C2")
	if err != nil {
		t.Fatalf("getting cell C2: %v", err)
	}
	if val != "abc12345" {
		t.Errorf("expected SHA display 'abc12345', got %q", val)
	}

	// Check hyperlink exists
	hasLink, target, err := xf.GetCellHyperLink("Non-Compliant", "C2")
	if err != nil {
		t.Fatalf("getting hyperlink: %v", err)
	}
	if !hasLink {
		t.Error("expected hyperlink on SHA cell C2 in Non-Compliant sheet")
	}
	if target != "https://github.com/org1/repo1/commit/abc12345678" {
		t.Errorf("unexpected hyperlink target: %s", target)
	}
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
	if err != nil {
		t.Fatalf("GenerateXLSX: %v", err)
	}

	xf, err := excelize.OpenFile(tmpFile)
	if err != nil {
		t.Fatalf("opening xlsx: %v", err)
	}
	defer xf.Close()

	sheets := xf.GetSheetList()
	found := false
	for _, s := range sheets {
		if s == "Non-Compliant" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Non-Compliant sheet should exist even with zero non-compliant rows")
	}

	// Should have header row only
	rows, err := xf.GetRows("Non-Compliant")
	if err != nil {
		t.Fatalf("getting Non-Compliant rows: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 row (header only) in empty Non-Compliant sheet, got %d", len(rows))
	}
}

func TestDetailRowBranchName(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Truncate(time.Second)

	insertCommit(t, db, "org1", "repo1", "aaa111aaa", "dev1", now, 10, 5)
	insertCommitBranch(t, db, "org1", "repo1", "aaa111aaa", "main")
	insertAuditResult(t, db, "org1", "repo1", "aaa111aaa", false, false, true, true, true, 1, []string{"reviewer1"}, []string{"compliant"})

	r := New(db)
	details, err := r.GetDetails(context.Background(), ReportOpts{})
	if err != nil {
		t.Fatalf("GetDetails: %v", err)
	}

	if len(details) != 1 {
		t.Fatalf("expected 1 detail row, got %d", len(details))
	}
	if details[0].BranchName != "main" {
		t.Errorf("expected BranchName='main', got %q", details[0].BranchName)
	}
}
