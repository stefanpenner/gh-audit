package sync

import (
	"testing"

	"github.com/stefanpenner/gh-audit/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Bridge test for tla/Exempt.tla — the JVM-free half of the spec↔code
// link for §1. TLC proves the DESIGN (verified-signer anchor is sound,
// the author-id path is forgeable); this proves the IMPLEMENTATION
// (exemptStatus) computes the spec's Exempt(c) predicate on every
// observable commit state, for both signing policies.
//
// The spec's per-commit observable state is (authorId, committerId,
// verified). We enumerate all of them against the same exempt-of-one
// world the .cfg checks, and mirror each Variant:
//
//	spec "signer"   ⇔ exemptStatus(requireSigning=true)   ⇔ Exempt_green
//	spec "optional" ⇔ exemptStatus(requireSigning=false)  ⇔ Exempt_amber
//	spec "author"   ⇔ the retired rule (mutation check)   ⇔ Exempt_red
//
// The soundness assertion mirrors Exempt_green: under the required
// policy, every state exemptStatus waives is a verified-signer state —
// the one the attacker provably cannot forge (an unsigned commit can
// never carry committerId == exempt under verification).
func TestExemptSpecBridge(t *testing.T) {
	const exemptID, atkID = int64(500), int64(999)
	exempt := []model.ExemptAuthor{{Login: "bot", ID: exemptID}}

	// The spec's id domain: exempt id, attacker id, unresolved (0).
	ids := []int64{exemptID, atkID, 0}

	type state struct {
		authorID, committerID int64
		verified, webflow     bool
	}
	var states []state
	for _, a := range ids {
		for _, c := range ids {
			for _, v := range []bool{true, false} {
				for _, wf := range []bool{true, false} {
					states = append(states, state{a, c, v, wf})
				}
			}
		}
	}
	require.Len(t, states, 3*3*2*2)

	// The spec predicates (Exempt.tla), transcribed.
	signerExempt := func(s state) bool { return s.verified && s.committerID == exemptID }
	webFlowExempt := func(s state) bool { return s.verified && s.webflow && s.authorID == exemptID }
	soundExempt := func(s state) bool { return signerExempt(s) || webFlowExempt(s) }
	authorExempt := func(s state) bool { return s.authorID == exemptID }

	commitOf := func(s state) model.Commit {
		login := ""
		if s.webflow {
			login = "web-flow"
		}
		return model.Commit{AuthorID: s.authorID, CommitterID: s.committerID, IsVerified: s.verified, CommitterLogin: login}
	}

	t.Run("required policy == spec signer variant (green)", func(t *testing.T) {
		for _, s := range states {
			got, forgeable := exemptStatus(commitOf(s), exempt, true)
			assert.Equal(t, soundExempt(s), got, "exempt on %+v", s)
			assert.False(t, forgeable, "required policy never yields a forgeable waiver: %+v", s)
		}
	})

	t.Run("optional policy == spec optional variant (amber)", func(t *testing.T) {
		for _, s := range states {
			got, forgeable := exemptStatus(commitOf(s), exempt, false)
			want := soundExempt(s) || authorExempt(s)
			assert.Equal(t, want, got, "exempt on %+v", s)
			// Forgeable exactly when waived via the author hint, not a sound signer.
			assert.Equal(t, got && !soundExempt(s), forgeable, "forgeable flag on %+v", s)
		}
	})

	t.Run("required is sound: every waiver is an unforgeable verified signer", func(t *testing.T) {
		for _, s := range states {
			got, _ := exemptStatus(commitOf(s), exempt, true)
			if got {
				assert.True(t, soundExempt(s),
					"required waived a state with no sound signer %+v — attacker-forgeable", s)
			}
		}
	})

	t.Run("mutation: retired author-only rule admits the forgery (red)", func(t *testing.T) {
		// The attack Exempt_red rediscovers: unsigned commit, forged author
		// id = exempt, committer NOT the exempt account. The author-only
		// rule waives it; the shipped required policy must not.
		attack := state{authorID: exemptID, committerID: atkID, verified: false}
		require.True(t, authorExempt(attack), "author-only rule waives the forgery")
		got, _ := exemptStatus(commitOf(attack), exempt, true)
		require.False(t, got, "required policy must reject the forged unsigned author")
		gotOpt, forgeable := exemptStatus(commitOf(attack), exempt, false)
		require.True(t, gotOpt && forgeable, "optional waives it but flags it forgeable")
	})
}
