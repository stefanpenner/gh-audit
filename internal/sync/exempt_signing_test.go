package sync

import (
	"testing"

	"github.com/stefanpenner/gh-audit/internal/model"
	"github.com/stretchr/testify/assert"
)

// TestExemptStatus_SigningPolicy is the irreducible core of the
// "author is only a hint" hardening (Architecture.md §1 trust model).
//
// A commit's author id is set from the git-author email — pushed bytes
// the committer fully controls. Only a VERIFIED signature binds an
// identity (the committer) to an account. So:
//
//   - strong path: IsVerified && committer id ∈ exempt  → exempt, sound
//   - forgeable path: author id ∈ exempt                → exempt only
//     when signing is OPTIONAL, and the verdict is flagged forgeable
//   - signing REQUIRED: the forgeable path is closed entirely
//
// bot = the exempt account (id 500). atk = an attacker account (id 999).
func TestExemptStatus_SigningPolicy(t *testing.T) {
	const bot, atk = int64(500), int64(999)
	exempt := []model.ExemptAuthor{{Login: "bot", ID: bot}}

	cases := []struct {
		name          string
		authorID      int64
		committerID   int64
		verified      bool
		requireSign   bool
		wantExempt    bool
		wantForgeable bool
	}{
		// Strong path — verified signer is the bot. Sound in both modes.
		{"verified bot signer, optional", atk, bot, true, false, true, false},
		{"verified bot signer, required", atk, bot, true, true, true, false},

		// Forgeable path — unsigned commit merely CLAIMS bot authorship.
		{"unsigned forged author, optional", bot, atk, false, false, true, true},
		{"unsigned forged author, required", bot, atk, false, true, false, false},

		// Verified, but the signature covers a NON-exempt committer; the
		// author field claiming the bot is still just a hint.
		{"verified non-bot signer + forged author, optional", bot, atk, true, false, true, true},
		{"verified non-bot signer + forged author, required", bot, atk, true, true, false, false},

		// Committer email says bot but the commit is UNSIGNED — committer
		// email is as forgeable as author email, so no strong path.
		{"unsigned committer=bot, optional", bot, bot, false, false, true, true},
		{"unsigned committer=bot, required", bot, bot, false, true, false, false},

		// Nobody exempt.
		{"unrelated verified commit", atk, atk, true, false, false, false},

		// Untrusted ids never match, even verified.
		{"zero ids", 0, 0, true, false, false, false},
		{"ghost committer", atk, model.GhostUserID, true, false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := model.Commit{
				AuthorID:    tc.authorID,
				CommitterID: tc.committerID,
				IsVerified:  tc.verified,
			}
			exempt2, forgeable := exemptStatus(c, exempt, tc.requireSign)
			assert.Equal(t, tc.wantExempt, exempt2, "exempt")
			assert.Equal(t, tc.wantForgeable, forgeable, "forgeable")
		})
	}
}

// A GitHub web-flow commit (verified, committer login "web-flow") had its
// AUTHOR id set by GitHub from an authenticated action — the exempt bot
// committing via the API/web UI. Only GitHub holds the web-flow signing
// key, so an attacker cannot forge (verified web-flow + author=bot). This
// is a SOUND path: exempt even under signing_policy: required, not flagged
// forgeable.
func TestExemptStatus_WebFlowAttestedAuthor(t *testing.T) {
	const bot, atk = int64(500), int64(999)
	exempt := []model.ExemptAuthor{{Login: "bot", ID: bot}}

	webflowBot := model.Commit{
		AuthorID: bot, CommitterID: 0, IsVerified: true, CommitterLogin: "web-flow",
	}
	for _, requireSigning := range []bool{false, true} {
		ex, forgeable := exemptStatus(webflowBot, exempt, requireSigning)
		assert.True(t, ex, "verified web-flow commit authored by the bot is exempt (requireSigning=%v)", requireSigning)
		assert.False(t, forgeable, "web-flow attestation is sound, not forgeable (requireSigning=%v)", requireSigning)
	}

	// Attacker cannot ride the web-flow path: their web-flow author id is
	// their own (GitHub sets it from THEIR session), never the bot's.
	webflowAtk := model.Commit{AuthorID: atk, IsVerified: true, CommitterLogin: "web-flow"}
	ex, _ := exemptStatus(webflowAtk, exempt, true)
	assert.False(t, ex, "attacker's web-flow commit is not exempt")

	// Web-flow claim without a verified signature is NOT sound — the
	// committer login is a forgeable string absent the signature.
	unverifiedWebflow := model.Commit{AuthorID: bot, IsVerified: false, CommitterLogin: "web-flow"}
	exReq, _ := exemptStatus(unverifiedWebflow, exempt, true)
	assert.False(t, exReq, "unverified web-flow is not the sound path under required")
	exOpt, forgeable := exemptStatus(unverifiedWebflow, exempt, false)
	assert.True(t, exOpt && forgeable, "unverified falls to the forgeable author path under optional")
}

// A forgeable-path exemption (unsigned commit claiming the bot) is still
// compliant under the default optional policy, but must carry both the
// ExemptionForgeable flag and a trust:forgeable-exemption annotation so
// the report can show it would not survive signing:required. A verified
// signer exemption carries neither.
func TestExemptAuthor_ForgeableAnnotation(t *testing.T) {
	const bot = int64(500)
	exempt := []model.ExemptAuthor{{Login: "bot", ID: bot}}

	t.Run("unsigned forged author is flagged forgeable", func(t *testing.T) {
		c := model.Commit{Org: "o", Repo: "r", SHA: "x", AuthorID: bot}
		res := EvaluateCommit(c, model.EnrichmentResult{}, exempt, nil, nil)
		assert.True(t, res.IsCompliant, "still compliant under optional policy")
		assert.True(t, res.ExemptionForgeable, "must set ExemptionForgeable")
		assert.Contains(t, res.Annotations, "trust:forgeable-exemption")
	})

	t.Run("verified signer exemption is not flagged", func(t *testing.T) {
		c := model.Commit{Org: "o", Repo: "r", SHA: "x", CommitterID: bot, IsVerified: true}
		res := EvaluateCommit(c, model.EnrichmentResult{}, exempt, nil, nil)
		assert.True(t, res.IsCompliant)
		assert.False(t, res.ExemptionForgeable)
		assert.NotContains(t, res.Annotations, "trust:forgeable-exemption")
	})
}
