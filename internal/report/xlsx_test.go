package report

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"
)

// newTestBuilder creates an in-memory workbook with the named sheets so
// per-sheet writers can be exercised without a database.
func newTestBuilder(t *testing.T, sheets ...string) (*excelize.File, *xlsxBuilder) {
	t.Helper()
	f := excelize.NewFile()
	t.Cleanup(func() { f.Close() })
	b, err := newBuilder(f)
	require.NoError(t, err, "newBuilder")
	for _, s := range sheets {
		_, err := f.NewSheet(s)
		require.NoError(t, err, "creating sheet %s", s)
	}
	return f, b
}

// xlsx string cells are never evaluated as formulas by excelize, so values
// must be written verbatim: no formula execution AND no literal apostrophe
// corrupting messages that start with - or +.
func TestXLSXCellsStoredVerbatimWithoutApostrophe(t *testing.T) {
	f, b := newTestBuilder(t, SheetWaiversLog)

	details := []DetailRow{{
		Org: "o", Repo: "r", SHA: "abc12345",
		IsEmptyCommit: true, // emitted unconditionally in the Waivers Log
		AuthorLogin:   "-Wall fix",
		Message:       "=cmd|' /C calc'!A0",
	}}
	require.NoError(t, b.writeWaiversLog(details))

	// Message cell (column H) stays a string cell — no formula.
	formula, err := f.GetCellFormula(SheetWaiversLog, "H2")
	require.NoError(t, err)
	assert.Empty(t, formula, "message must be stored as a string cell, not a formula")
	msg, err := f.GetCellValue(SheetWaiversLog, "H2")
	require.NoError(t, err)
	assert.Equal(t, "=cmd|' /C calc'!A0", msg, "message must not carry a literal apostrophe prefix")

	// Author cell (column E) displays without a leading apostrophe.
	author, err := f.GetCellValue(SheetWaiversLog, "E2")
	require.NoError(t, err)
	assert.Equal(t, "-Wall fix", author)
}

// The Action Queue tie-break must group on the same Org+"/"+Repo key it
// compares, otherwise two orgs sharing a repo name interleave by date and the
// comparator loses strict weak ordering.
func TestActionQueueSortGroupsByOrgRepo(t *testing.T) {
	f, b := newTestBuilder(t, SheetActionQueue)
	now := time.Now()

	// All non-compliant with no PR → identical severity (High, R3).
	details := []DetailRow{
		{Org: "aorg", Repo: "shared", SHA: "a1", CommittedAt: now.Add(-2 * time.Hour)},
		{Org: "borg", Repo: "shared", SHA: "b1", CommittedAt: now.Add(-1 * time.Hour)},
		{Org: "aorg", Repo: "shared", SHA: "a2", CommittedAt: now.Add(-3 * time.Hour)},
	}
	require.NoError(t, b.writeActionQueue(details))

	var repos []string
	for row := 2; row <= 4; row++ {
		v, err := f.GetCellValue(SheetActionQueue, fmt.Sprintf("C%d", row))
		require.NoError(t, err)
		repos = append(repos, v)
	}
	assert.Equal(t, []string{"aorg/shared", "aorg/shared", "borg/shared"}, repos,
		"rows must group by org/repo, not interleave by date")
}

// Clean-revert / clean-merge / bot rows belong in the Waivers Log only when
// the stored verdict is compliant — the log is evidence of what the tool did
// NOT flag, so non-compliant commits must never appear there.
func TestWaiversLogOnlyListsCompliantWaiverRows(t *testing.T) {
	f, b := newTestBuilder(t, SheetWaiversLog)

	details := []DetailRow{
		{Org: "o", Repo: "r", SHA: "cm-bad", IsCleanMerge: true, IsCompliant: false},
		{Org: "o", Repo: "r", SHA: "cm-ok", IsCleanMerge: true, IsCompliant: true},
		{Org: "o", Repo: "r", SHA: "bot-bad", IsBot: true, IsCompliant: false},
		{Org: "o", Repo: "r", SHA: "bot-ok", IsBot: true, IsCompliant: true},
		{Org: "o", Repo: "r", SHA: "rv-bad", IsCleanRevert: true, IsCompliant: false},
		{Org: "o", Repo: "r", SHA: "rv-ok", IsCleanRevert: true, IsCompliant: true},
	}
	require.NoError(t, b.writeWaiversLog(details))

	rows, err := f.GetRows(SheetWaiversLog)
	require.NoError(t, err)
	require.Len(t, rows, 4, "header + 3 compliant waiver rows")

	var kinds []string
	for _, r := range rows[1:] {
		kinds = append(kinds, r[2]) // Waiver Type column
	}
	assert.Equal(t, []string{"clean-merge", "bot", "clean-revert"}, kinds)
}

// The green Summary style means "nothing failed here" — it must be driven by
// NonCompliantCount == 0, never by a compliance percentage that rounded up.
func TestSummaryGreenStyleOnlyWhenZeroNonCompliant(t *testing.T) {
	f, b := newTestBuilder(t, SheetSummary)

	rows := []SummaryRow{
		{Org: "o", Repo: "clean", TotalCommits: 10, CompliantCount: 10, NonCompliantCount: 0, CompliancePct: 100},
		// Simulates a rounded-up 100.0 with one real failure.
		{Org: "o", Repo: "dirty", TotalCommits: 10000, CompliantCount: 9999, NonCompliantCount: 1, CompliancePct: 100},
		{Org: "o", Repo: "yellow", TotalCommits: 100, CompliantCount: 95, NonCompliantCount: 5, CompliancePct: 95},
	}
	require.NoError(t, b.writeSummary(rows, ReportOpts{}))

	// Data starts at row 3; Compliance % is column F.
	cleanStyle, err := f.GetCellStyle(SheetSummary, "F3")
	require.NoError(t, err)
	dirtyStyle, err := f.GetCellStyle(SheetSummary, "F4")
	require.NoError(t, err)
	yellowStyle, err := f.GetCellStyle(SheetSummary, "F5")
	require.NoError(t, err)

	assert.NotEqual(t, cleanStyle, dirtyStyle, "a repo with failures must not get the green style")
	assert.Equal(t, yellowStyle, dirtyStyle, "rounded-up 100%% with failures renders like any >=90%% repo")
}

// Zero CommittedAt (SQL NULL) renders as blank cells, not 0001-01-01 or an
// absurd "days since commit".
func TestZeroCommittedAtRendersBlankCells(t *testing.T) {
	f, b := newTestBuilder(t, SheetActionQueue, SheetDecisionMatrix, SheetWaiversLog)

	nonCompliant := DetailRow{Org: "o", Repo: "r", SHA: "def45678"} // no PR → action queue
	waived := DetailRow{Org: "o", Repo: "r", SHA: "abc12345", IsEmptyCommit: true}

	require.NoError(t, b.writeActionQueue([]DetailRow{nonCompliant}))
	require.NoError(t, b.writeDecisionMatrix([]DetailRow{nonCompliant}))
	require.NoError(t, b.writeWaiversLog([]DetailRow{waived}))

	committed, _ := f.GetCellValue(SheetActionQueue, "K2")
	assert.Empty(t, committed, "Action Queue Committed")
	days, _ := f.GetCellValue(SheetActionQueue, "L2")
	assert.Empty(t, days, "Action Queue Days Since Commit")

	dmDate, _ := f.GetCellValue(SheetDecisionMatrix, "G2")
	assert.Empty(t, dmDate, "Decision Matrix Date")

	wlDate, _ := f.GetCellValue(SheetWaiversLog, "F2")
	assert.Empty(t, wlDate, "Waivers Log Date")
}

func TestEscapeFormulaURL(t *testing.T) {
	assert.Equal(t, "https://github.com/org/repo", escapeFormulaURL("https://github.com/org/repo"))
	assert.Equal(t, `https://example.com/""inject`, escapeFormulaURL(`https://example.com/"inject`))
}

func TestFormatInt(t *testing.T) {
	cases := map[int]string{
		0:       "0",
		7:       "7",
		999:     "999",
		1000:    "1,000",
		12345:   "12,345",
		880987:  "880,987",
		1000000: "1,000,000",
		-12345:  "-12,345",
	}
	for in, want := range cases {
		assert.Equal(t, want, formatInt(in), "formatInt(%d)", in)
	}
}

func TestPluralS(t *testing.T) {
	assert.Equal(t, "", pluralS(1))
	assert.Equal(t, "s", pluralS(0))
	assert.Equal(t, "s", pluralS(2))
	assert.Equal(t, "s", pluralS(42))
}

func TestSummarizeReport(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		s := summarizeReport(nil, nil)
		assert.Equal(t, 0, s.Repos)
		assert.Equal(t, 0, s.Commits)
		assert.Equal(t, 0.0, s.CompliancePct)
	})

	t.Run("aggregates summary totals across repos", func(t *testing.T) {
		summary := []SummaryRow{
			{Org: "o", Repo: "a", TotalCommits: 100, CompliantCount: 90, NonCompliantCount: 10, WaivedCount: 5, ActionQueueCount: 3},
			{Org: "o", Repo: "b", TotalCommits: 200, CompliantCount: 200, NonCompliantCount: 0, WaivedCount: 50, ActionQueueCount: 0},
		}
		s := summarizeReport(summary, nil)
		assert.Equal(t, 2, s.Repos)
		assert.Equal(t, 300, s.Commits)
		assert.Equal(t, 290, s.Compliant)
		assert.Equal(t, 10, s.NonCompliant)
		assert.Equal(t, 55, s.Waived)
		assert.Equal(t, 3, s.ActionQueue)
		assert.InDelta(t, 96.6667, s.CompliancePct, 0.001)
	})

	t.Run("distinct PRs and authors counted from details, not summary", func(t *testing.T) {
		details := []DetailRow{
			{Org: "o", Repo: "a", PRNumber: 1, AuthorLogin: "alice"},
			{Org: "o", Repo: "a", PRNumber: 1, AuthorLogin: "alice"}, // duplicate PR + author
			{Org: "o", Repo: "a", PRNumber: 2, AuthorLogin: "bob"},
			{Org: "o", Repo: "b", PRNumber: 1, AuthorLogin: "alice"}, // PR#1 in different repo distinct from o/a#1
			{Org: "o", Repo: "a", PRNumber: 0, AuthorLogin: "carol"}, // no PR — excluded from PRs but author counted
			{Org: "o", Repo: "a", PRNumber: 3, AuthorLogin: ""},      // empty author — excluded
		}
		s := summarizeReport(nil, details)
		assert.Equal(t, 4, s.PRs, "o/a#1, o/a#2, o/a#3, o/b#1 — distinct on (org, repo, number)")
		assert.Equal(t, 3, s.Authors, "alice, bob, carol")
	})

	t.Run("zero commits → 0%% compliance, no division by zero", func(t *testing.T) {
		s := summarizeReport([]SummaryRow{{TotalCommits: 0}}, nil)
		assert.Equal(t, 0.0, s.CompliancePct)
	})
}
