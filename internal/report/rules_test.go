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
