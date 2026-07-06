package db

import (
	"context"
	"testing"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CommitterID is the non-forgeable identity §1 anchors the exempt waiver
// on (paired with IsVerified). If it does not round-trip through the DB,
// an offline re-audit reads CommitterID == 0, the verified-signer path
// silently fails, and a signed exempt commit stops being exempt. This is
// the same class of bug as a dropped author_id (see the ID-only-matching
// invariant). Guard both the write and every read path.
func TestCommitterID_RoundTrips(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	c := model.Commit{
		Org: "org1", Repo: "repo1", SHA: "signed",
		AuthorID: 77, CommitterID: 88, IsVerified: true,
		AuthorLogin: "bot", CommitterLogin: "bot-signer",
		CommittedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Message:     "signed by the bot", ParentCount: 1,
	}
	require.NoError(t, db.UpsertCommits(ctx, []model.Commit{c}))

	bySHA, err := db.GetCommitsBySHA(ctx, "org1", "repo1", []string{"signed"})
	require.NoError(t, err)
	require.Len(t, bySHA, 1)
	assert.Equal(t, int64(88), bySHA[0].CommitterID, "GetCommitsBySHA drops committer_id")

	unaudited, err := db.GetUnauditedCommits(ctx, "org1", "repo1", time.Time{}, time.Time{})
	require.NoError(t, err)
	require.Len(t, unaudited, 1)
	assert.Equal(t, int64(88), unaudited[0].CommitterID, "GetUnauditedCommits drops committer_id")
}
