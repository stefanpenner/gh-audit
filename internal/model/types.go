// Package model defines the core domain types for gh-audit.
// See README.md for a rendered UML class diagram of type relationships.
package model

import (
	"regexp"
	"strings"
	"time"
)

var coAuthorRe = regexp.MustCompile(`(?i)co-authored-by:\s*(.+?)\s*<([^>]+)>`)

// noreplyRe extracts a GitHub login from noreply email addresses.
// Handles both "user@users.noreply.github.com" and "12345+user@users.noreply.github.com".
var noreplyRe = regexp.MustCompile(`^(?:\d+\+)?([^@]+)@users\.noreply\.github\.com$`)

// ParseCoAuthors extracts co-authors from "Co-authored-by" trailers in commit messages.
// GitHub login is resolved from noreply email addresses when possible.
// Duplicate trailers (same email, compared case-insensitively) are collapsed
// to the first occurrence — commit messages frequently repeat a co-author
// across the body and the final trailer block.
func ParseCoAuthors(message string) []CoAuthor {
	if !strings.Contains(strings.ToLower(message), "co-authored-by") {
		return nil
	}
	matches := coAuthorRe.FindAllStringSubmatch(message, -1)
	if len(matches) == 0 {
		return nil
	}
	coAuthors := make([]CoAuthor, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		email := strings.TrimSpace(m[2])
		key := strings.ToLower(email)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		coAuthors = append(coAuthors, CoAuthor{
			Name:  strings.TrimSpace(m[1]),
			Email: email,
			Login: LoginFromNoreplyEmail(email),
		})
	}
	return coAuthors
}

// LoginFromNoreplyEmail extracts a GitHub login from a noreply email address.
// Returns empty string if the email is not a GitHub noreply address.
func LoginFromNoreplyEmail(email string) string {
	m := noreplyRe.FindStringSubmatch(strings.ToLower(email))
	if m == nil {
		return ""
	}
	return m[1]
}

// A Commit is a git commit synced from GitHub.
//
// Modeled as a pure git object — knows parents, author, and committer.
// Branch records the ref context at sync time (transient; not persisted).
//
//	CoAuthor ←── Commit ──→ CheckRun
//	                ├──→ PullRequest (via merge commit or head SHA)
//	                ├──→ AuditResult
//	                └──→ EnrichmentResult
type Commit struct {
	Org         string
	Repo        string
	SHA         string
	AuthorLogin string
	// AuthorID is the immutable numeric GitHub account ID. Zero when
	// the commit's email isn't bound to a verified GH user — but
	// commit ingestion (internal/github/client.go::requireAuthor)
	// refuses such commits with a fix-it message, so by the time a
	// row reaches the audit it is guaranteed non-zero. The ID is the
	// only forgery-resistant author identity GitHub exposes (logins
	// can be renamed and reclaimed; numeric IDs are immutable per
	// account and never reused), so it's the sole signal used by §1
	// (exempt author) and §5 (self-approval) matching.
	AuthorID       int64
	AuthorEmail    string
	CommitterLogin string
	CoAuthors      []CoAuthor
	CommittedAt    time.Time
	Message        string
	IsVerified     bool
	ParentCount    int
	Additions      int
	Deletions      int
	Branch         string
	Href           string
}

// A FileDiff is a per-file change in a commit's diff, used for clean-revert
// verification. Transient — not persisted in the DB.
type FileDiff struct {
	Filename  string
	Status    string // added, modified, removed, renamed, copied, changed, unchanged
	Additions int
	Deletions int
	Patch     string
}

// A CoAuthor is an additional commit author extracted from
// "Co-authored-by" trailers in the commit message.
//
//	CoAuthor ←── Commit
type CoAuthor struct {
	Login string
	Email string
	Name  string
}

// A PullRequest is a GitHub pull request.
//
// HeadSHA is the tip of the PR's source branch — the last commit the
// author pushed. MergeCommitSHA is the commit GitHub created on the
// base branch when merged (merge, squash, or last rebase commit).
// HeadBranch is the source branch ref (e.g. "feature/xyz").
//
//	Commit ──→ PullRequest ──→ Review
//	                 └──→ EnrichmentResult
type PullRequest struct {
	Org            string
	Repo           string
	Number         int
	Title          string
	Merged         bool
	HeadSHA        string
	HeadBranch     string
	MergeCommitSHA string
	AuthorLogin    string
	AuthorID       int64
	MergedByLogin  string
	MergedByID     int64
	MergedAt       time.Time
	Href           string
}

// A Review is a GitHub pull request review (approval, request for
// changes, comment, or dismissal). CommitID records the SHA the
// reviewer saw, enabling staleness detection after force-pushes.
//
//	PullRequest ──→ Review
//	                  └──→ AuditResult
type Review struct {
	Org           string
	Repo          string
	PRNumber      int
	ReviewID      int64
	ReviewerLogin string
	ReviewerID    int64
	State         string // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED
	CommitID      string
	SubmittedAt   time.Time
	Href          string
}

// A CheckRun is a GitHub Actions or third-party CI check result,
// tied to a specific commit SHA.
//
//	Commit ──→ CheckRun
//	             └──→ AuditResult
type CheckRun struct {
	Org         string
	Repo        string
	CommitSHA   string
	CheckRunID  int64
	CheckName   string
	Status      string // queued, in_progress, completed
	Conclusion  string // success, failure, neutral, cancelled, skipped, timed_out, action_required
	CompletedAt time.Time
}

// An AuditResult is the compliance verdict for a single commit.
// Produced by the audit engine after evaluating a commit against
// configured rules. Primary output of the system.
//
//	Commit ──┐
//	PullRequest ──→ AuditResult ──→ report
//	Review ──┘
//	CheckRun ──┘
type AuditResult struct {
	Org                string
	Repo               string
	SHA                string
	IsEmptyCommit      bool
	IsBot              bool // author name ends with [bot] — informational only
	IsExemptAuthor     bool // author is on the configured exemption list
	HasPR              bool
	PRNumber           int
	PRCount            int
	HasFinalApproval   bool
	HasStaleApproval   bool // approval exists but on a pre-force-push commit
	// HasPostMergeConcern is true when a reviewer submitted a CHANGES_REQUESTED
	// or DISMISSED review after the PR merged. Informational — does not affect
	// IsCompliant (compliance is evaluated point-in-time at merge).
	HasPostMergeConcern bool
	// IsCleanRevert is true when the commit is a clean revert of a prior
	// commit. For bot auto-reverts this is trusted by message pattern; for
	// manual reverts it is set only when RevertVerification == "diff-verified".
	// Informational — does not affect IsCompliant (policy for clean reverts
	// is not yet codified).
	IsCleanRevert      bool
	// RevertVerification records how IsCleanRevert was determined.
	// One of: "" / "none" (not a revert), "message-only" (bot auto-revert
	// trusted by pattern, or manual revert whose referenced commit could not
	// be fetched), "diff-verified" (manual revert whose diff is the exact
	// inverse of the referenced commit), "diff-mismatch" (manual revert whose
	// diff is not a clean inverse).
	RevertVerification string
	// RevertedSHA is the SHA of the commit being reverted, extracted from
	// the revert commit's message. Empty if not a revert.
	RevertedSHA        string
	// IsCleanMerge is true when the commit is a two-parent merge that
	// introduced no new content beyond its parents — i.e., no conflict
	// resolution or post-merge edit. Informational — does not affect
	// IsCompliant (policy for clean merges is not yet codified).
	IsCleanMerge      bool
	// MergeVerification records how the merge was classified.
	// One of: "" / "none" (squash or single-parent), "clean" (2 parents,
	// files[] empty — merge introduced no diff of its own), "dirty"
	// (2 parents, merge commit has conflict-resolution or extra content),
	// "octopus" (3+ parents, not auto-classified).
	MergeVerification string
	IsSelfApproved     bool // true if only approvals are from code contributors
	ApproverLogins     []string
	OwnerApprovalCheck string // success, failure, missing
	IsCompliant        bool
	Reasons            []string
	MergeStrategy         string // initial, merge, squash, rebase, direct-push
	PRCommitAuthorLogins  []string
	CommitHref            string
	PRHref             string
	// Annotations are informational tags attached by the audit's detector
	// pass (see internal/sync/annotations.go). They describe structural
	// patterns — automation/dep-bump markers, etc. — without affecting
	// IsCompliant. Reviewers can filter by tag to triage automated or
	// automation-adjacent PRs. Format is "<family>:<kv>" (e.g.
	// "automation:depex") so the XLSX can filter by prefix.
	Annotations          []string
	AuditedAt          time.Time
}

// A SyncCursor tracks incremental sync progress for a single
// repo+branch pair. On each run, only commits after LastDate are
// fetched from GitHub.
//
//	RepoInfo ──→ SyncCursor
type SyncCursor struct {
	Org       string
	Repo      string
	Branch    string
	LastDate  time.Time
	UpdatedAt time.Time
}

// A RepoInfo is a GitHub repository discovered during sync.
// Provides the metadata needed to scope sync and audit operations.
//
//	RepoInfo ──→ SyncCursor
//	    └──→ Commit
type RepoInfo struct {
	Org           string
	Name          string
	FullName      string
	DefaultBranch string
	Archived      bool
}

// An EnrichmentResult bundles a commit with all its related GitHub
// data. Intermediate structure: after fetching a commit, the enricher
// populates PRs, reviews, check runs, and PR branch commits before persisting.
//
// PRBranchCommits maps PR number → commits on that PR's feature branch.
// Used to detect all contributors in squash-merged PRs where the squash
// commit hides the original per-commit authorship.
//
//	Commit ──┐
//	PullRequest ──→ EnrichmentResult ──→ AuditResult
//	Review ──┘
//	CheckRun ──┘
type EnrichmentResult struct {
	Commit          Commit
	PRs             []PullRequest
	Reviews         []Review
	CheckRuns       []CheckRun
	PRBranchCommits map[int][]Commit // PR number → commits on the PR's branch
	// Clean-revert signal computed during enrichment (see AuditResult
	// fields of the same name for semantics).
	IsCleanRevert      bool
	RevertVerification string
	RevertedSHA        string
	// Clean-merge signal computed during enrichment (see AuditResult
	// fields of the same name for semantics).
	IsCleanMerge      bool
	MergeVerification string
}

// ExemptAuthor is a single entry in the exempt-author list (see
// Architecture.md §1). Loaded from `~/.config/gh-audit/config.yaml`
// and consulted on every commit's audit by sync.applyExemptAuthorRule.
//
// The matching contract:
//
//   - ID, when non-zero, is the canonical key. It's the immutable
//     numeric GitHub account ID — never reused across deletions,
//     never transferred by renames, not forgeable.
//   - Login is the fallback for commits whose author.id couldn't be
//     resolved (commit's git author email isn't bound to a verified
//     GH user; service accounts often hit this). Forgery-prone:
//     renames + 90-day cooldown can transfer the username to a
//     different account.
//   - Type and Name are display-only metadata captured at resolution
//     time.
//   - Comment is a user-supplied annotation preserved through the
//     YAML round-trip; useful for "was: <old-login>, renamed
//     YYYY-MM" notes.
//
// New entries enter the config as bare logins via tooling that calls
// GET /users/{login} once to populate ID; the populated form is what
// gh-audit consumes thereafter. The schema is structured-only —
// bare-string entries are no longer accepted (see migration
// 2026-05-04).
type ExemptAuthor struct {
	Login   string `yaml:"login"`
	ID      int64  `yaml:"id,omitempty"`
	Type    string `yaml:"type,omitempty"`
	Name    string `yaml:"name,omitempty"`
	Comment string `yaml:"comment,omitempty"`
}
