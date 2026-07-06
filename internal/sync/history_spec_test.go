package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Bridge test for tla/History.tla — the JVM-free half of the spec↔code
// link for force-push detection. TLC proves the DESIGN (a rewrite is
// sound-detectable only by checking a retained prior head against the
// current head's reachability); this proves the IMPLEMENTATION
// (classifyHeadMove) reports a rewrite exactly when the prior head is not
// reachable from the current head.
//
// The spec abstracts a head to the SET of commits reachable from it, and a
// move is a rewrite iff the prior set ⊄ the current set. GitHub's compare
// status is the observable of that reachability:
//
//	prior ⊆ current  ⇔  status ∈ {identical, ahead}  (fast-forward)
//	prior ⊄ current  ⇔  status ∈ {behind, diverged}  (rewrite)
func TestHistorySpecBridge(t *testing.T) {
	// The spec's move outcomes, paired with the GitHub compare statuses
	// that realize each. `reachable` is the spec's "prior ⊆ current".
	statuses := []struct {
		status    string
		reachable bool // spec: is the prior head still reachable (⊆)?
	}{
		{"identical", true},
		{"ahead", true},
		{"behind", false},
		{"diverged", false},
	}

	t.Run("recorded rule conforms: rewrite iff prior unreachable (green)", func(t *testing.T) {
		for _, s := range statuses {
			got := classifyHeadMove("prior", "current", s.status)
			// Spec TrulySafe ⇔ prior reachable ⇔ NOT flagged as rewrite.
			isRewrite := got == HeadRewritten
			assert.Equal(t, !s.reachable, isRewrite,
				"status %q: reachable=%v should map to rewrite=%v", s.status, s.reachable, !s.reachable)
		}
	})

	t.Run("sound: never call a non-fast-forward move safe", func(t *testing.T) {
		for _, s := range statuses {
			got := classifyHeadMove("prior", "current", s.status)
			if got == HeadFastForward || got == HeadUnchanged {
				require.True(t, s.reachable,
					"status %q classified safe but prior is not reachable — a rewrite slipped through", s.status)
			}
		}
	})

	t.Run("mutation: the snapshot-blind rule misses the rewrite (red)", func(t *testing.T) {
		// tla/History_red.cfg: a history-blind auditor inspects only the
		// current head and never compares against the prior — it can never
		// report a rewrite. Model that rule and confirm it fails to flag a
		// genuine divergence, which classifyHeadMove must catch.
		snapshotBlind := func(_, _, _ string) HeadMove { return HeadUnchanged }
		require.Equal(t, HeadRewritten, classifyHeadMove("prior", "current", "diverged"),
			"the real rule flags the diverge")
		require.NotEqual(t, HeadRewritten, snapshotBlind("prior", "current", "diverged"),
			"harness gone vacuous: the snapshot-blind rule must miss it")
	})
}
