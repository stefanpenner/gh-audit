package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// classifyHeadMove is the irreducible core of history-rewrite (force-push)
// detection (tla/History.tla). It maps the retained prior head + the
// current head + GitHub's compare status onto a verdict. The soundness
// lesson the spec encodes: a rewrite is only detectable by comparing the
// current head against a RETAINED prior head — a single snapshot cannot
// see it. GitHub's compare (prior...current) gives the reachability:
//
//	identical / ahead → prior is reachable from current → fast-forward (safe)
//	behind / diverged → prior is NOT reachable → history was rewritten
func TestClassifyHeadMove(t *testing.T) {
	cases := []struct {
		name          string
		prior, cur    string
		compareStatus string
		want          HeadMove
	}{
		{"no prior head recorded", "", "b", "", HeadUnknown},
		{"unchanged", "a", "a", "identical", HeadUnchanged},
		{"fast-forward (ahead)", "a", "b", "ahead", HeadFastForward},
		{"identical status but different sha is fast-forward-safe", "a", "b", "identical", HeadFastForward},
		{"rewrite — branch reset to older (behind)", "a", "b", "behind", HeadRewritten},
		{"rewrite — diverged", "a", "b", "diverged", HeadRewritten},
		{"unknown compare status fails safe-to-unknown", "a", "b", "", HeadUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyHeadMove(tc.prior, tc.cur, tc.compareStatus))
		})
	}
}
