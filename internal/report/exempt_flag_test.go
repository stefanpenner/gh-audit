package report

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// IsExemptAuthor is a VISIBILITY flag, not a waiver: when an exempt bot's
// squash contains non-exempt human commits, the pipeline sets the flag but
// audits the content normally — the commit can be non-compliant. The
// report layer used to treat the flag as the waiver, dropping those
// commits from the Action Queue and counting them as waived.
func TestExemptFlagWithoutWaiverIsNotSuppressed(t *testing.T) {
	flagOnly := DetailRow{
		IsExemptAuthor: true, IsCompliant: false, HasPR: true,
		IsSelfApproved: true,
		Reasons:        "self-approved (reviewer is code author) (PR #7)",
	}
	granted := DetailRow{
		IsExemptAuthor: true, IsCompliant: true,
		Reasons: "exempt: configured author",
	}

	t.Run("rule outcomes distinguish flag from waiver", func(t *testing.T) {
		assert.NotEqual(t, OutcomeWaived, DeriveRuleOutcomes(flagOnly).R1Exempt,
			"flag without a granted waiver must not read as waived")
		assert.Equal(t, OutcomeWaived, DeriveRuleOutcomes(granted).R1Exempt)
	})

	t.Run("non-compliant flagged commit requires action", func(t *testing.T) {
		assert.True(t, DeriveRuleOutcomes(flagOnly).RequiresAction(),
			"unreviewed human code in a bot squash must reach the Action Queue")
		assert.False(t, DeriveRuleOutcomes(granted).RequiresAction())
	})

	t.Run("flag-only no-PR commit fails R3", func(t *testing.T) {
		noPRFlagOnly := DetailRow{IsExemptAuthor: true, IsCompliant: false, HasPR: false,
			Reasons: "no associated pull request"}
		assert.Equal(t, OutcomeFail, DeriveRuleOutcomes(noPRFlagOnly).R3HasPR)
	})

	t.Run("summary counts key on verdict not flag", func(t *testing.T) {
		db := setupTestDB(t)
		now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		insertCommit(t, db, "o", "r", "granted", "bot", now, 5, 1)
		insertCommit(t, db, "o", "r", "flagonly", "bot", now.Add(time.Hour), 5, 1)
		insertCommitBranch(t, db, "o", "r", "granted", "main")
		insertCommitBranch(t, db, "o", "r", "flagonly", "main")
		insertAuditResultFull(t, db, "o", "r", "granted", auditResultOpts{
			isExempt: true, isCompliant: true,
			reasons: []string{"exempt: configured author"},
		})
		insertAuditResultFull(t, db, "o", "r", "flagonly", auditResultOpts{
			isExempt: true, isCompliant: false, hasPR: true, prNumber: 7, isSelfApproved: true,
			reasons: []string{"self-approved (reviewer is code author) (PR #7)"},
		})

		r := New(db)
		summaries, err := r.GetSummary(context.Background(), ReportOpts{})
		require.NoError(t, err)
		require.Len(t, summaries, 1)
		assert.Equal(t, 1, summaries[0].ActionQueueCount,
			"the non-compliant flag-only commit is an action item")
		assert.Equal(t, 1, summaries[0].WaivedCount,
			"only the granted exemption counts as waived")
	})
}

// A flagged commit that passed NORMAL review (voided carve-out, then a
// real approval) is compliant — but not BY the exemption. It must not
// appear as an exemption waiver anywhere.
func TestExemptFlagWithPRComplianceIsNotAWaiver(t *testing.T) {
	db := setupTestDB(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	insertCommit(t, db, "o", "r", "viapr", "bot", now, 5, 1)
	insertCommitBranch(t, db, "o", "r", "viapr", "main")
	insertAuditResultFull(t, db, "o", "r", "viapr", auditResultOpts{
		isExempt: true, isCompliant: true, hasPR: true, prNumber: 9,
		hasApproval: true, approvers: []string{"rev1"},
		reasons: []string{"compliant"},
	})

	r := New(db)
	summaries, err := r.GetSummary(context.Background(), ReportOpts{})
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Zero(t, summaries[0].WaivedCount,
		"compliant-via-PR with a visibility flag is not an exemption waiver")
	assert.Zero(t, summaries[0].ActionQueueCount)
}
