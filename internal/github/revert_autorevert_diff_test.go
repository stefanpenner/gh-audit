package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stefanpenner/gh-audit/internal/model"
	"github.com/stretchr/testify/assert"
)

// AutoRevert used to flip a commit to a clean-revert waiver on the strength of
// its "Automatic revert of <new>..<old>" message alone — a forgeable hint.
// It must now be diff-verified exactly like a ManualRevert: only an exact
// inverse diff sets IsCleanRevert. An unverified message stays advisory and
// the commit falls through to the normal review rules.
func TestClassifyRevert_AutoRevertRequiresDiffVerification(t *testing.T) {
	const revertSHA = "1111111111111111111111111111111111111111"
	const newSHA = "921197f96e12b6e4f5c82104af0d83b7627ed99d" // the commit being reverted
	const oldSHA = "4eca5c7c3b6d1f9563e877a9484c87be6633b647"
	const autoMsg = "Automatic revert of " + newSHA + ".." + oldSHA + "\n\nAutomated safety revert."

	mkFile := func(name, patch string) map[string]any {
		return map[string]any{
			"filename":  name,
			"status":    "modified",
			"additions": 1,
			"deletions": 1,
			"patch":     patch,
		}
	}

	// Serve commit-files for whichever SHA is asked for. The revert (revertSHA)
	// removes the exact line the reverted commit (newSHA) added — an inverse
	// pair — while a mismatch case lets the caller swap newSHA's patch.
	newPatch := "@@ -1 +1 @@\n-before\n+after"    // newSHA: before -> after
	revertPatch := "@@ -1 +1 @@\n-after\n+before" // revertSHA: after -> before (inverse)

	serve := func(t *testing.T) *CachingEnricher {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var files []map[string]any
			switch {
			case strings.Contains(r.URL.Path, revertSHA):
				files = []map[string]any{mkFile("a.go", revertPatch)}
			case strings.Contains(r.URL.Path, newSHA):
				files = []map[string]any{mkFile("a.go", newPatch)}
			default:
				files = nil
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sha":     "x",
				"commit":  map[string]any{"message": "x"},
				"parents": []map[string]any{{"sha": "p1"}},
				"files":   files,
			})
		}))
		t.Cleanup(srv.Close)
		return NewCachingEnricher(NewClient(mockTokenPool(t, srv.URL), testLogger()), stubEnrichmentCache{})
	}

	t.Run("inverse diff -> diff-verified, IsCleanRevert true", func(t *testing.T) {
		ce := serve(t)
		result := &model.EnrichmentResult{
			Commit: model.Commit{SHA: revertSHA, Message: autoMsg},
		}
		ce.classifyRevertAndMerge(context.Background(), "testorg", "repo", revertSHA, 1, result)

		assert.True(t, result.IsCleanRevert, "exact inverse diff must verify")
		assert.Equal(t, "diff-verified", result.RevertVerification)
		assert.Equal(t, newSHA, result.RevertedSHA)
	})

	t.Run("non-inverse diff -> diff-mismatch, no waiver", func(t *testing.T) {
		ce := serve(t)
		// Make the reverted commit's patch NOT the inverse of the revert's.
		newPatch = "@@ -1 +1 @@\n-unrelated\n+content"
		t.Cleanup(func() { newPatch = "@@ -1 +1 @@\n-before\n+after" })

		result := &model.EnrichmentResult{
			Commit: model.Commit{SHA: revertSHA, Message: autoMsg},
		}
		ce.classifyRevertAndMerge(context.Background(), "testorg", "repo", revertSHA, 1, result)

		assert.False(t, result.IsCleanRevert, "a forgeable AutoRevert message must not waive without a verified diff")
		assert.Equal(t, "diff-mismatch", result.RevertVerification)
	})
}
