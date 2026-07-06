package report

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// benchDB seeds n commits + audit rows through set-based SQL (range())
// so benchmark setup stays fast even at 50k rows.
func benchDB(b *testing.B, n int) *sql.DB {
	b.Helper()
	db, err := sql.Open("duckdb", "")
	require.NoError(b, err)
	b.Cleanup(func() { db.Close() })
	for _, stmt := range strings.Split(schemaDDL, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		_, err := db.Exec(stmt)
		require.NoError(b, err, "schema: %s", stmt)
	}

	_, err = db.Exec(fmt.Sprintf(`
		INSERT INTO commits (org, repo, sha, author_login, committed_at, message, parent_count, additions, deletions, href)
		SELECT 'o', 'r' || (i %% 20), 'sha' || i, 'dev' || (i %% 50),
		       TIMESTAMP '2026-01-01 00:00:00' + to_minutes(i),
		       'feat: change ' || i || ' (#' || i || ')', 1, 10, 2,
		       'https://github.com/o/r/commit/' || i
		FROM range(%d) t(i)`, n))
	require.NoError(b, err)
	_, err = db.Exec(fmt.Sprintf(`
		INSERT INTO commit_branches (org, repo, sha, branch)
		SELECT 'o', 'r' || (i %% 20), 'sha' || i, 'main' FROM range(%d) t(i)`, n))
	require.NoError(b, err)
	_, err = db.Exec(fmt.Sprintf(`
		INSERT INTO audit_results (org, repo, sha, is_empty_commit, is_bot, is_exempt_author,
		                           has_pr, pr_number, pr_count, has_final_approval, has_stale_approval,
		                           is_self_approved, approver_logins, is_compliant, reasons, commit_href, pr_href)
		SELECT 'o', 'r' || (i %% 20), 'sha' || i, false, false, false,
		       i %% 10 <> 0, i, 1, i %% 4 <> 0, false,
		       i %% 17 = 0, list_value('rev' || (i %% 5)), i %% 4 <> 0,
		       CASE WHEN i %% 4 <> 0 THEN list_value('compliant') ELSE list_value('no approval on final commit (PR #' || i || ')') END,
		       '', ''
		FROM range(%d) t(i)`, n))
	require.NoError(b, err)
	return db
}

func BenchmarkGetSummary_50kRows(b *testing.B) {
	db := benchDB(b, 50000)
	r := New(db)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s, err := r.GetSummary(context.Background(), ReportOpts{})
		if err != nil || len(s) == 0 {
			b.Fatalf("summary: %v (%d rows)", err, len(s))
		}
	}
}

func BenchmarkGetDetails_50kRows(b *testing.B) {
	db := benchDB(b, 50000)
	r := New(db)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d, err := r.GetDetails(context.Background(), ReportOpts{})
		if err != nil || len(d) == 0 {
			b.Fatalf("details: %v (%d rows)", err, len(d))
		}
	}
}

func BenchmarkGenerateXLSX_10kRows(b *testing.B) {
	db := benchDB(b, 10000)
	r := New(db)
	dir := b.TempDir()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := filepath.Join(dir, fmt.Sprintf("bench-%d.xlsx", i))
		if err := r.GenerateXLSX(context.Background(), ReportOpts{}, out, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDeriveRuleOutcomes(b *testing.B) {
	d := DetailRow{
		HasPR: true, PRNumber: 7, HasFinalApproval: false, IsSelfApproved: true,
		HasStaleApproval: true, OwnerApprovalCheck: "failure",
		Reasons: "self-approved (reviewer is code author) (PR #7)",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		o := DeriveRuleOutcomes(d)
		if !o.RequiresAction() {
			b.Fatal("expected action")
		}
	}
}
