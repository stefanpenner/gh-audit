package sync

import (
	"testing"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Bridge test for tla/Checks.tla — the JVM-free half of the spec↔code
// link. TLC proves the *design* (latest-run-wins) sound; this test
// proves the *implementation* (evaluateRequiredChecks) conforms to the
// spec's Compliant definition over the same small world the green
// config checks: every run sequence up to MaxRuns=3, completedAt in
// 0..MaxTime=2, conclusion in {success, failure, queued}.
//
// Two assertions, mirroring the green/red config pair:
//   - shipped rule: conforms to spec Compliant on every state, so
//     Sound (Compliant => TrulySafe) holds — the green config in Go.
//   - mutation check: the retired any-run rule MUST violate soundness
//     on at least one enumerated state — the red config in Go. If this
//     stops failing-the-naive-rule, the harness has gone vacuous.

// specWorldMaxRuns / specWorldMaxTime mirror tla/Checks_green.cfg.
// Keep in sync with the cfg CONSTANTS.
const (
	specWorldMaxRuns = 3
	specWorldMaxTime = 2
)

// specRun is one observable check run in the spec's world:
// [t |-> 0..MaxTime, concl |-> {"success","failure","none"}].
// The spec's "none" (queued/in_progress) is Go's empty Conclusion.
type specRun struct {
	t     int
	concl string
}

var specConcls = []string{"success", "failure", ""}

// enumSpecStates enumerates every run sequence of length 0..MaxRuns —
// the full observable state space of Checks.tla with merged = TRUE
// (merged is decided upstream of evaluateRequiredChecks, so the bridge
// covers the CheckPasses half of Compliant).
func enumSpecStates() [][]specRun {
	states := [][]specRun{{}}
	frontier := [][]specRun{{}}
	for len(frontier) > 0 && len(frontier[0]) < specWorldMaxRuns {
		var next [][]specRun
		for _, s := range frontier {
			for t := 0; t <= specWorldMaxTime; t++ {
				for _, c := range specConcls {
					child := append(append([]specRun{}, s...), specRun{t, c})
					next = append(next, child)
				}
			}
		}
		states = append(states, next...)
		frontier = next
	}
	return states
}

// specTrulySafe is the spec's TrulySafe / LatestConcl oracle: the run
// with max t wins, ties broken by later index (higher run id).
func specTrulySafe(runs []specRun) bool {
	latest := -1
	for i, r := range runs {
		if latest == -1 || r.t >= runs[latest].t {
			latest = i
		}
	}
	return latest >= 0 && runs[latest].concl == "success"
}

// toCheckRuns maps a spec state onto the model: index order is
// insertion order, so CheckRunID grows with index — the same tie-break
// the spec uses (j <= i).
func toCheckRuns(runs []specRun) []model.CheckRun {
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]model.CheckRun, len(runs))
	for i, r := range runs {
		out[i] = model.CheckRun{
			CommitSHA:   "head",
			CheckRunID:  int64(i + 1),
			CheckName:   "ci",
			Conclusion:  r.concl,
			CompletedAt: epoch.Add(time.Duration(r.t) * time.Second),
		}
	}
	return out
}

func TestChecksSpecBridge(t *testing.T) {
	states := enumSpecStates()
	require.Len(t, states, 1+9+81+729, "state space must match the spec bounds")

	required := []RequiredCheck{{Name: "ci", Conclusion: "success"}}

	t.Run("shipped rule conforms to spec Compliant (green)", func(t *testing.T) {
		for _, s := range states {
			goPass := evaluateRequiredChecks(toCheckRuns(s), "head", required) == "success"
			assert.Equal(t, specTrulySafe(s), goPass,
				"evaluateRequiredChecks diverges from Checks.tla on %+v", s)
		}
	})

	t.Run("any-run rule violates soundness (red / mutation check)", func(t *testing.T) {
		anyRunPassed := func(runs []specRun) bool {
			for _, r := range runs {
				if r.concl == "success" {
					return true
				}
			}
			return false
		}
		violations := 0
		for _, s := range states {
			if anyRunPassed(s) && !specTrulySafe(s) {
				violations++
			}
		}
		require.NotZero(t, violations,
			"harness gone vacuous: the retired any-run rule must be caught")
	})
}
