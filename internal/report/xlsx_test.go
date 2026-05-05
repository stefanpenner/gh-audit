package report

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeCell(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"normal text", "normal text"},
		{"=cmd|'/c calc'", "'=cmd|'/c calc'"},
		{"+cmd", "'+cmd"},
		{"-cmd", "'-cmd"},
		{"@SUM(A1)", "'@SUM(A1)"},
		{"", ""},
		{"123", "123"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, sanitizeCell(tt.input), "sanitizeCell(%q)", tt.input)
	}
}

func TestEscapeFormulaURL(t *testing.T) {
	assert.Equal(t, "https://github.com/org/repo", escapeFormulaURL("https://github.com/org/repo"))
	assert.Equal(t, `https://example.com/""inject`, escapeFormulaURL(`https://example.com/"inject`))
}

func TestFormatInt(t *testing.T) {
	cases := map[int]string{
		0:        "0",
		7:        "7",
		999:      "999",
		1000:     "1,000",
		12345:    "12,345",
		880987:   "880,987",
		1000000:  "1,000,000",
		-12345:   "-12,345",
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
