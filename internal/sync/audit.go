package sync

import (
	"fmt"
	"strings"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// RequiredCheck describes a status check that must pass for compliance.
type RequiredCheck struct {
	Name       string
	Conclusion string
}

// EvaluateCommit determines compliance for a single commit given its enrichment data.
func EvaluateCommit(commit model.Commit, enrichment model.EnrichmentResult, exemptAuthors []string, requiredChecks []RequiredCheck) model.AuditResult {
	result := model.AuditResult{
		Org:        commit.Org,
		Repo:       commit.Repo,
		SHA:        commit.SHA,
		CommitHref: commit.Href,
	}

	// Check exempt author (bot)
	for _, exempt := range exemptAuthors {
		if strings.EqualFold(commit.AuthorLogin, exempt) {
			result.IsBot = true
			result.IsCompliant = true
			result.Reasons = []string{"exempt: bot author"}
			return result
		}
	}

	// Check empty commit
	if commit.Additions == 0 && commit.Deletions == 0 {
		result.IsEmptyCommit = true
		result.IsCompliant = true
		result.Reasons = []string{"empty commit"}
		return result
	}

	// Check for associated PRs
	if len(enrichment.PRs) == 0 {
		result.HasPR = false
		result.IsCompliant = false
		result.Reasons = []string{"no associated pull request"}
		return result
	}

	result.HasPR = true

	// Evaluate each PR for compliance
	var bestPR *model.PullRequest
	var bestReasons []string
	var bestApprovers []string

	for i := range enrichment.PRs {
		pr := &enrichment.PRs[i]
		prReasons := []string{}
		prApprovers := []string{}

		// Check for approval on final commit
		hasApprovalOnFinal := false
		for _, review := range enrichment.Reviews {
			if review.PRNumber == pr.Number && review.State == "APPROVED" && review.CommitID == pr.HeadSHA {
				hasApprovalOnFinal = true
				prApprovers = append(prApprovers, review.ReviewerLogin)
			}
		}

		if !hasApprovalOnFinal {
			prReasons = append(prReasons, fmt.Sprintf("no approval on final commit (PR #%d)", pr.Number))
		}

		// Check Owner Approval (required checks on PR's head commit)
		ownerApprovalStatus := "missing"
		for _, rc := range requiredChecks {
			for _, cr := range enrichment.CheckRuns {
				if cr.CommitSHA == pr.HeadSHA && cr.CheckName == rc.Name {
					if strings.EqualFold(cr.Conclusion, rc.Conclusion) {
						ownerApprovalStatus = "success"
					} else {
						ownerApprovalStatus = "failure"
					}
				}
			}
		}

		// If no required checks configured, treat Owner Approval as not required (success)
		if len(requiredChecks) == 0 {
			ownerApprovalStatus = "success"
		}

		if ownerApprovalStatus != "success" {
			prReasons = append(prReasons, fmt.Sprintf("Owner Approval check missing/failed (PR #%d)", pr.Number))
		}

		// If this PR satisfies all checks, commit is compliant
		if hasApprovalOnFinal && ownerApprovalStatus == "success" {
			result.IsCompliant = true
			result.HasFinalApproval = true
			result.ApproverLogins = prApprovers
			result.OwnerApprovalCheck = ownerApprovalStatus
			result.PRNumber = pr.Number
			result.PRHref = pr.Href
			result.Reasons = []string{"compliant"}
			return result
		}

		// Track best PR (fewest reasons = closest to compliant)
		if bestPR == nil || len(prReasons) < len(bestReasons) {
			bestPR = pr
			bestReasons = prReasons
			bestApprovers = prApprovers
		}
	}

	// No PR satisfied all checks
	result.IsCompliant = false
	if bestPR != nil {
		result.PRNumber = bestPR.Number
		result.PRHref = bestPR.Href
		result.ApproverLogins = bestApprovers
		// Set approval fields based on best PR
		for _, review := range enrichment.Reviews {
			if review.PRNumber == bestPR.Number && review.State == "APPROVED" && review.CommitID == bestPR.HeadSHA {
				result.HasFinalApproval = true
				break
			}
		}
		// Owner Approval check status for best PR
		ownerApprovalStatus := "missing"
		for _, rc := range requiredChecks {
			for _, cr := range enrichment.CheckRuns {
				if cr.CommitSHA == bestPR.HeadSHA && cr.CheckName == rc.Name {
					if strings.EqualFold(cr.Conclusion, rc.Conclusion) {
						ownerApprovalStatus = "success"
					} else {
						ownerApprovalStatus = "failure"
					}
				}
			}
		}
		if len(requiredChecks) == 0 {
			ownerApprovalStatus = "success"
		}
		result.OwnerApprovalCheck = ownerApprovalStatus
	}
	result.Reasons = bestReasons
	return result
}
