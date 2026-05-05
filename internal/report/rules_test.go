package report

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDeriveRuleOutcomes(t *testing.T) {
	cases := []struct {
		name string
		d    DetailRow
		want RuleOutcomes
	}{
		{
			name: "compliant happy path",
			d: DetailRow{
				HasPR: true, HasFinalApproval: true, IsCompliant: true,
				OwnerApprovalCheck: "success",
			},
			want: RuleOutcomes{
				R1Exempt: OutcomeSkip, R2Empty: OutcomeSkip,
				R3HasPR: OutcomePass, R4FinalApproval: OutcomePass,
				R4bStale: OutcomePass, R4cPostMergeConcern: OutcomePass,
				R5SelfApproval: OutcomePass, R6OwnerCheck: OutcomePass,
				R7Verdict: OutcomePass, R8RevertWaiver: OutcomeNA,
			},
		},
		{
			name: "no PR → R3 fail, R4/R5 n/a",
			d: DetailRow{
				HasPR: false, IsCompliant: false,
			},
			want: RuleOutcomes{
				R1Exempt: OutcomeSkip, R2Empty: OutcomeSkip,
				R3HasPR: OutcomeFail, R4FinalApproval: OutcomeNA,
				R4bStale: OutcomeNA, R4cPostMergeConcern: OutcomeNA,
				R5SelfApproval: OutcomeNA, R6OwnerCheck: OutcomeNA,
				R7Verdict: OutcomeFail, R8RevertWaiver: OutcomeNA,
			},
		},
		{
			name: "empty commit → R2 waived, R3 n/a",
			d: DetailRow{
				IsEmptyCommit: true, HasPR: false, IsCompliant: true,
			},
			want: RuleOutcomes{
				R1Exempt: OutcomeSkip, R2Empty: OutcomeWaived,
				R3HasPR: OutcomeNA, R4FinalApproval: OutcomeNA,
				R4bStale: OutcomeNA, R4cPostMergeConcern: OutcomeNA,
				R5SelfApproval: OutcomeNA, R6OwnerCheck: OutcomeNA,
				R7Verdict: OutcomePass, R8RevertWaiver: OutcomeNA,
			},
		},
		{
			name: "self-approved → R5 fail",
			d: DetailRow{
				HasPR: true, HasFinalApproval: true, IsSelfApproved: true,
				IsCompliant: false, OwnerApprovalCheck: "success",
			},
			want: RuleOutcomes{
				R1Exempt: OutcomeSkip, R2Empty: OutcomeSkip,
				R3HasPR: OutcomePass, R4FinalApproval: OutcomePass,
				R4bStale: OutcomePass, R4cPostMergeConcern: OutcomePass,
				R5SelfApproval: OutcomeFail, R6OwnerCheck: OutcomePass,
				R7Verdict: OutcomeFail, R8RevertWaiver: OutcomeNA,
			},
		},
		{
			name: "stale approval → R4 fail, R4b fail",
			d: DetailRow{
				HasPR: true, HasStaleApproval: true, IsCompliant: false,
			},
			want: RuleOutcomes{
				R1Exempt: OutcomeSkip, R2Empty: OutcomeSkip,
				R3HasPR: OutcomePass, R4FinalApproval: OutcomeFail,
				R4bStale: OutcomeFail, R4cPostMergeConcern: OutcomePass,
				R5SelfApproval: OutcomeNA, R6OwnerCheck: OutcomeNA,
				R7Verdict: OutcomeFail, R8RevertWaiver: OutcomeNA,
			},
		},
		{
			name: "owner check missing",
			d: DetailRow{
				HasPR: true, HasFinalApproval: true, IsCompliant: false,
				OwnerApprovalCheck: "missing",
			},
			want: RuleOutcomes{
				R1Exempt: OutcomeSkip, R2Empty: OutcomeSkip,
				R3HasPR: OutcomePass, R4FinalApproval: OutcomePass,
				R4bStale: OutcomePass, R4cPostMergeConcern: OutcomePass,
				R5SelfApproval: OutcomePass, R6OwnerCheck: OutcomeMissing,
				R7Verdict: OutcomeFail, R8RevertWaiver: OutcomeNA,
			},
		},
		{
			name: "clean revert waiver flips verdict",
			d: DetailRow{
				HasPR: false, IsCleanRevert: true, IsCompliant: true,
			},
			want: RuleOutcomes{
				R1Exempt: OutcomeSkip, R2Empty: OutcomeSkip,
				R3HasPR: OutcomeFail, R4FinalApproval: OutcomeNA,
				R4bStale: OutcomeNA, R4cPostMergeConcern: OutcomeNA,
				R5SelfApproval: OutcomeNA, R6OwnerCheck: OutcomeNA,
				R7Verdict: OutcomePass, R8RevertWaiver: OutcomeWaived,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveRuleOutcomes(tc.d)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRequiresAction(t *testing.T) {
	cases := []struct {
		name string
		d    DetailRow
		want bool
	}{
		{"compliant", DetailRow{HasPR: true, HasFinalApproval: true, IsCompliant: true}, false},
		{"exempt waives non-compliant", DetailRow{IsExemptAuthor: true, IsCompliant: true}, false},
		{"empty waives non-compliant", DetailRow{IsEmptyCommit: true, IsCompliant: true}, false},
		{"clean revert waives non-compliant", DetailRow{IsCleanRevert: true, IsCompliant: true}, false},
		{"no-PR fail triggers action", DetailRow{HasPR: false, IsCompliant: false}, true},
		{"self-approved triggers action", DetailRow{HasPR: true, HasFinalApproval: true, IsSelfApproved: true, IsCompliant: false}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := DeriveRuleOutcomes(tc.d)
			assert.Equal(t, tc.want, o.RequiresAction())
		})
	}
}

func TestSynthesizeActionPriority(t *testing.T) {
	// When multiple rules fire, higher-priority rule wins.
	cases := []struct {
		name     string
		d        DetailRow
		wantSev  Severity
		wantRule string
	}{
		{
			"no PR beats missing check",
			DetailRow{HasPR: false, OwnerApprovalCheck: "missing", IsCompliant: false},
			SeverityHigh, "R3 HasPR",
		},
		{
			"self-approved beats stale",
			DetailRow{HasPR: true, HasFinalApproval: true, IsSelfApproved: true, HasStaleApproval: true, IsCompliant: false, OwnerApprovalCheck: "success"},
			SeverityHigh, "R5 SelfApproval",
		},
		{
			"owner check fail beats stale",
			DetailRow{HasPR: true, HasFinalApproval: true, HasStaleApproval: true, OwnerApprovalCheck: "failure", IsCompliant: false},
			SeverityHigh, "R6 OwnerCheck",
		},
		{
			"stale beats plain no-final-approval",
			DetailRow{HasPR: true, HasFinalApproval: false, HasStaleApproval: true, IsCompliant: false, OwnerApprovalCheck: "success"},
			SeverityMedium, "R4b Stale",
		},
		{
			"compliant returns empty severity",
			DetailRow{HasPR: true, HasFinalApproval: true, IsCompliant: true, OwnerApprovalCheck: "success"},
			SeverityNone, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := DeriveRuleOutcomes(tc.d)
			sev, rule, action := SynthesizeAction(tc.d, o)
			assert.Equal(t, tc.wantSev, sev)
			assert.Equal(t, tc.wantRule, rule)
			if tc.wantRule != "" {
				assert.NotEmpty(t, action, "expected a prescribed action string")
			}
		})
	}
}

func TestSynthesizeContext(t *testing.T) {
	cases := []struct {
		name string
		d    DetailRow
		want string
	}{
		{
			name: "no notable signals → empty",
			d:    DetailRow{},
			want: "",
		},
		{
			name: "self-merged with squash strategy",
			d: DetailRow{
				AuthorLogin:   "alice",
				MergedByLogin: "alice",
				MergeStrategy: "squash",
			},
			want: "self-merged · squash",
		},
		{
			name: "different author and merger — no self-merged signal",
			d: DetailRow{
				AuthorLogin:   "alice",
				MergedByLogin: "bob",
				MergeStrategy: "squash",
			},
			want: "squash",
		},
		{
			name: "merge_strategy 'unknown' is suppressed",
			d: DetailRow{
				MergeStrategy: "unknown",
			},
			want: "",
		},
		{
			name: "revert classifier ran but didn't grant waiver — surface verification + truncated target",
			d: DetailRow{
				IsCleanRevert:      false,
				RevertVerification: "diff-mismatch",
				RevertedSHA:        "8423dc092a39ebd38e6021e24055dc5fa5e8437a",
			},
			want: "revert: diff-mismatch (target 8423dc09)",
		},
		{
			name: "clean revert (waiver granted) — no revert signal in context (R8 already handles it)",
			d: DetailRow{
				IsCleanRevert:      true,
				RevertVerification: "diff-verified",
				RevertedSHA:        "abc123",
			},
			want: "",
		},
		{
			name: "revert_verification 'none' is suppressed",
			d: DetailRow{
				RevertVerification: "none",
			},
			want: "",
		},
		{
			name: "stale + post-merge concern + multiple PRs + bot, all signals",
			d: DetailRow{
				HasStaleApproval:    true,
				HasPostMergeConcern: true,
				PRCount:             3,
				IsBot:               true,
			},
			want: "stale · post-merge concern · 3 PRs · bot",
		},
		{
			name: "PRCount==1 doesn't emit multi-PR signal",
			d: DetailRow{
				PRCount: 1,
				IsBot:   true,
			},
			want: "bot",
		},
		{
			name: "ordering: self-merged → strategy → revert → stale → post-merge → multi-pr → bot",
			d: DetailRow{
				AuthorLogin:         "alice",
				MergedByLogin:       "alice",
				MergeStrategy:       "merge",
				RevertVerification:  "message-only",
				RevertedSHA:         "deadbeef00000000",
				HasStaleApproval:    true,
				HasPostMergeConcern: true,
				PRCount:             2,
				IsBot:               true,
			},
			want: "self-merged · merge · revert: message-only (target deadbeef) · stale · post-merge concern · 2 PRs · bot",
		},
		{
			name: "revert without target SHA — emits verification only, no parens",
			d: DetailRow{
				RevertVerification: "diff-mismatch",
			},
			want: "revert: diff-mismatch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, SynthesizeContext(tc.d))
		})
	}
}

func TestTruncSHA8(t *testing.T) {
	assert.Equal(t, "abcdef12", truncSHA8("abcdef1234567890"))
	assert.Equal(t, "abc", truncSHA8("abc"))
	assert.Equal(t, "", truncSHA8(""))
	assert.Equal(t, "abcdef12", truncSHA8("abcdef12"))
}
