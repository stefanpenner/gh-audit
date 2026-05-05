package report

import (
	"fmt"
	"strings"
)

// A RuleOutcome is a single rule's verdict for one commit in the Decision
// Matrix. Values are human-readable strings so they render directly in the
// XLSX and CSV without additional formatting.
//
//	DetailRow ──→ DeriveRuleOutcomes ──→ RuleOutcomes ──→ SynthesizeAction
type RuleOutcome string

const (
	OutcomePass    RuleOutcome = "pass"
	OutcomeFail    RuleOutcome = "fail"
	OutcomeSkip    RuleOutcome = "skip"
	OutcomeNA      RuleOutcome = "n/a"
	OutcomeMissing RuleOutcome = "missing"
	OutcomeWaived  RuleOutcome = "waived"
)

// A Severity ranks an Action Queue row so auditors can sort by urgency.
type Severity string

const (
	SeverityHigh   Severity = "High"
	SeverityMedium Severity = "Medium"
	SeverityLow    Severity = "Low"
	SeverityNone   Severity = ""
)

// severityRank orders severities for sorting; higher rank = more urgent.
func severityRank(s Severity) int {
	switch s {
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	}
	return 0
}

// RuleOutcomes is the per-rule evaluation of a single commit, mirroring the
// decision tree in internal/sync/audit.go (R1..R8). Cells are derived from
// DetailRow flags already populated by the audit pipeline; no new SQL runs.
type RuleOutcomes struct {
	R1Exempt            RuleOutcome // exempt author waiver
	R2Empty             RuleOutcome // empty-commit waiver
	R3HasPR             RuleOutcome // associated PR exists
	R4FinalApproval     RuleOutcome // non-self approval on final commit
	R4bStale            RuleOutcome // stale approval (pre-force-push)
	R4cPostMergeConcern RuleOutcome // CHANGES_REQUESTED / DISMISSED after merge
	R5SelfApproval      RuleOutcome // only approver is a code contributor
	R6OwnerCheck        RuleOutcome // configured required status check
	R7Verdict           RuleOutcome // overall compliance verdict (pre-R8)
	R8RevertWaiver      RuleOutcome // clean-revert waiver flipped a failure
}

// DeriveRuleOutcomes maps a DetailRow's stored booleans to per-rule PASS /
// FAIL / SKIP / N/A / MISSING / WAIVED cells. Ordering matches
// internal/sync/audit.go so the Decision Matrix reads top-to-bottom like the
// audit pipeline itself.
func DeriveRuleOutcomes(d DetailRow) RuleOutcomes {
	o := RuleOutcomes{}

	// R1: exempt author → treated as waiver when the pipeline accepted it.
	if d.IsExemptAuthor {
		o.R1Exempt = OutcomeWaived
	} else {
		o.R1Exempt = OutcomeSkip
	}

	// R2: empty commit → waiver when pipeline accepted it.
	if d.IsEmptyCommit {
		o.R2Empty = OutcomeWaived
	} else {
		o.R2Empty = OutcomeSkip
	}

	// R3: associated PR. A non-empty, non-exempt commit without a PR fails.
	switch {
	case d.HasPR:
		o.R3HasPR = OutcomePass
	case d.IsExemptAuthor || d.IsEmptyCommit:
		o.R3HasPR = OutcomeNA
	default:
		o.R3HasPR = OutcomeFail
	}

	// R4: final-commit approval. Only meaningful once a PR exists.
	if !d.HasPR {
		o.R4FinalApproval = OutcomeNA
		o.R4bStale = OutcomeNA
		o.R4cPostMergeConcern = OutcomeNA
		o.R5SelfApproval = OutcomeNA
	} else {
		if d.HasFinalApproval {
			o.R4FinalApproval = OutcomePass
		} else {
			o.R4FinalApproval = OutcomeFail
		}
		if d.HasStaleApproval {
			o.R4bStale = OutcomeFail
		} else {
			o.R4bStale = OutcomePass
		}
		if d.HasPostMergeConcern {
			o.R4cPostMergeConcern = OutcomeFail
		} else {
			o.R4cPostMergeConcern = OutcomePass
		}
		switch {
		case d.IsSelfApproved:
			o.R5SelfApproval = OutcomeFail
		case d.HasFinalApproval:
			o.R5SelfApproval = OutcomePass
		default:
			o.R5SelfApproval = OutcomeNA
		}
	}

	// R6: configured required status check. Empty string = not required.
	switch d.OwnerApprovalCheck {
	case "success":
		o.R6OwnerCheck = OutcomePass
	case "failure":
		o.R6OwnerCheck = OutcomeFail
	case "missing":
		o.R6OwnerCheck = OutcomeMissing
	default:
		o.R6OwnerCheck = OutcomeNA
	}

	// R7: overall verdict as reported by the audit pipeline. R8 waiver (if
	// any) is already folded into IsCompliant, so we surface R8 separately.
	if d.IsCompliant {
		o.R7Verdict = OutcomePass
	} else {
		o.R7Verdict = OutcomeFail
	}

	// R8: clean-revert waiver. Only meaningful as a flipping signal — the
	// cell reads waived when a clean revert was detected regardless of
	// whether it actually changed the verdict. Downstream readers can still
	// spot it via the Reasons column.
	if d.IsCleanRevert {
		o.R8RevertWaiver = OutcomeWaived
	} else {
		o.R8RevertWaiver = OutcomeNA
	}

	return o
}

// RequiresAction reports whether a commit belongs on the Action Queue —
// failing verdict without a waiver flip already applied.
func (o RuleOutcomes) RequiresAction() bool {
	if o.R7Verdict != OutcomeFail {
		return false
	}
	if o.R1Exempt == OutcomeWaived || o.R2Empty == OutcomeWaived || o.R8RevertWaiver == OutcomeWaived {
		return false
	}
	return true
}

// SynthesizeContext returns a compact, human-readable string of secondary
// signals about a commit — the "fact pattern" the auditor needs to decide
// what action to take, beyond the single primary failing rule that
// SynthesizeAction picks. Empty when no signals are notable.
//
// Signals are ordered most-to-least decision-relevant, joined with " · ":
//
//   - self-merged          — author == merger (no independent gatekeeper)
//   - squash / merge / rebase — merge strategy when known
//   - revert: <verification> [target <sha8>] — clean-revert classifier ran
//     but didn't grant the §8 waiver; surfaces *why* (diff-mismatch,
//     message-only, etc.) so the auditor knows R8 was attempted
//   - stale                 — approval exists but not on final commit
//   - post-merge concern    — DISMISSED / CHANGES_REQUESTED after merge
//   - multiple PRs          — commit associated with >1 PR (possible
//     cherry-pick or backport)
//   - bot                   — author is a bot account
func SynthesizeContext(d DetailRow) string {
	var parts []string
	if d.AuthorLogin != "" && d.MergedByLogin != "" && d.AuthorLogin == d.MergedByLogin {
		parts = append(parts, "self-merged")
	}
	if d.MergeStrategy != "" && d.MergeStrategy != "unknown" {
		parts = append(parts, d.MergeStrategy)
	}
	if d.RevertVerification != "" && d.RevertVerification != "none" && !d.IsCleanRevert {
		s := "revert: " + d.RevertVerification
		if d.RevertedSHA != "" {
			s += " (target " + truncSHA8(d.RevertedSHA) + ")"
		}
		parts = append(parts, s)
	}
	if d.HasStaleApproval {
		parts = append(parts, "stale")
	}
	if d.HasPostMergeConcern {
		parts = append(parts, "post-merge concern")
	}
	if d.PRCount > 1 {
		parts = append(parts, fmt.Sprintf("%d PRs", d.PRCount))
	}
	if d.IsBot {
		parts = append(parts, "bot")
	}
	return strings.Join(parts, " · ")
}

func truncSHA8(sha string) string {
	if len(sha) <= 8 {
		return sha
	}
	return sha[:8]
}

// SynthesizeAction picks a primary failing rule and returns (severity, rule
// label, prescribed action). Returns ("", "", "") for compliant commits.
//
// Priority is chosen so auditors chase hardest-to-fix failures first: no PR
// > self-approved > required check > missing final approval > stale.
func SynthesizeAction(d DetailRow, o RuleOutcomes) (Severity, string, string) {
	if !o.RequiresAction() {
		return SeverityNone, "", ""
	}

	prTag := ""
	if d.PRNumber > 0 {
		prTag = fmt.Sprintf(" (PR #%d)", d.PRNumber)
	}

	switch {
	case o.R3HasPR == OutcomeFail:
		return SeverityHigh, "R3 HasPR", "Open a retroactive PR or document justification"
	case o.R5SelfApproval == OutcomeFail:
		return SeverityHigh, "R5 SelfApproval", "Find a non-self approver" + prTag
	case o.R6OwnerCheck == OutcomeFail || o.R6OwnerCheck == OutcomeMissing:
		return SeverityHigh, "R6 OwnerCheck", "Re-run / request Owner Approval" + prTag
	case o.R4bStale == OutcomeFail:
		return SeverityMedium, "R4b Stale", "Verify force-push was legitimate; re-approve if so" + prTag
	case o.R4FinalApproval == OutcomeFail:
		return SeverityMedium, "R4 FinalApproval", "Chase missing approval on final commit" + prTag
	case o.R4cPostMergeConcern == OutcomeFail:
		return SeverityLow, "R4c PostMergeConcern", "Investigate post-merge reviewer concern" + prTag
	}

	// Fallthrough for any not-yet-categorized failure — leave severity medium
	// so the row still surfaces.
	reason := strings.TrimSpace(d.Reasons)
	if reason == "" {
		reason = "Investigate non-compliant commit"
	}
	return SeverityMedium, "R7 Verdict", reason
}
