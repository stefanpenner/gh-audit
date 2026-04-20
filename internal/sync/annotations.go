package sync

import (
	"regexp"
	"strings"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// Annotations are informational tags on an audit result that describe
// *why* the commit has the shape it does, independent of compliance
// verdict. The main audit decides "compliant / not"; annotations add
// metadata like "this commit is an automated dep-bump". Surfaced in
// the XLSX as a column and (for discovery) as a dedicated sheet.
//
// Annotations are purely informational and never affect IsCompliant.
// See TODO.md for deferred work (revert-chain claim parsing, diff-verified
// re-apply auto-flip) that would promote some annotations to
// compliance-bearing rules.

// ComputeAnnotations runs every detector against the commit and returns a
// flat list of tag strings in stable order. Tag grammar: "<family>:<kv...>"
// or just "<family>" for valueless tags, joined with "=" and ",". Example:
//
//	automation:depex
//
// Rationale for string-bag-of-tags over a typed struct: the report layer
// only needs to filter / display; structured fields are over-engineered
// while we're still discovering which patterns matter.
func ComputeAnnotations(commit model.Commit, _ model.EnrichmentResult) []string {
	var out []string

	// Automation/dep-bump detection (bracketed tool prefix in the PR title /
	// commit subject). These annotations don't waive review — they give the
	// reviewer context for why the PR looks the way it does (e.g., why
	// "self-approval" fires on a PR whose reviewer is also a co-author via
	// the automation's author-list convention).
	if tag := detectAutomationTag(commit.Message); tag != "" {
		out = append(out, "automation:"+tag)
	}

	return out
}

// Bracketed tool prefixes that LinkedIn's tooling ecosystem emits on the
// first line of an automated PR. Matched case-insensitively at the start
// of the commit subject so a title like "[DepEx] Update X" maps to
// automation:depex. Add to this list as new tools are spotted.
var automationPrefixes = []struct {
	tag    string
	prefix string
}{
	{"depex", "[depex]"},
	{"renovate", "[renovate]"},
	{"dependabot", "[dependabot]"},
	{"auto-merge", "[auto-merge]"},
	{"autoupgrade", "[autoupgrade]"},
	{"merge-queue", "[merge-queue]"},
}

// descriptionCheckOverrideRe matches LinkedIn's internal
// DESCRIPTIONCHECKOVERRIDE marker, which automated tools emit to bypass
// the description-quality check. Its presence is a strong signal that
// the PR went through an automated workflow.
var descriptionCheckOverrideRe = regexp.MustCompile(`(?im)\bDESCRIPTIONCHECKOVERRIDE\b`)

// detectAutomationTag returns a short tag identifying the automation
// that produced the commit, or "" when no signal is found. Precedence:
// subject-line bracket prefix (most specific) → DESCRIPTIONCHECKOVERRIDE
// body marker (fallback).
func detectAutomationTag(message string) string {
	subject := message
	if i := strings.IndexByte(subject, '\n'); i >= 0 {
		subject = subject[:i]
	}
	lowSubject := strings.ToLower(strings.TrimSpace(subject))
	for _, p := range automationPrefixes {
		if strings.HasPrefix(lowSubject, p.prefix) {
			return p.tag
		}
	}
	if descriptionCheckOverrideRe.MatchString(message) {
		return "description-check-override"
	}
	return ""
}
