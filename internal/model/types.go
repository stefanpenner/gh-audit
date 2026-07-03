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
		// A trailer like `Co-authored-by: x < >` regex-matches with a
		// blank address. Empty emails are useless for attribution and
		// would collide in the co_authors primary key (org, repo, sha,
		// email) — drop them.
		if email == "" {
			continue
		}
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

// GhostUserID is GitHub's shared sentinel account ("ghost") substituted
// for every deleted user on PR and review user fields. It is not a real
// identity: two different deleted people both surface as this id, and an
// approval attributed to it is unverifiable.
const GhostUserID int64 = 10137

// TrustedID reports whether a numeric account id is a usable identity:
// non-zero (resolved) and not the shared ghost sentinel. Every identity
// claim in the audit (§4 approval counting, §5 self-approval, the report's
// self-merged signal) must gate on this — never on id != 0 alone.
func TrustedID(id int64) bool {
	return id != 0 && id != GhostUserID
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
	// the commit's email isn't bound to a verified GH user — commit
	// ingestion (internal/github/client.go::resolveAuthor) logs a
	// fix-it warning and keeps the row, so zero-ID rows do reach the
	// audit. The ID is the only forgery-resistant author identity
	// GitHub exposes (logins can be renamed and reclaimed; numeric
	// IDs are immutable per account and never reused), so it is the
	// SOLE matching signal for both §1 (exempt author) and §5
	// (self-approval). A zero ID never matches — there is no email or
	// login fallback (both are forgeable). AuthorEmail below is
	// informational/display only.
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
	// FilesChanged is the number of files the commit touches, from the
	// commit-detail endpoint. Load-bearing for the §2 empty-commit waiver:
	// pure renames and mode-only changes report 0/0 line stats but a
	// non-zero file count, and must not be waived as "empty".
	FilesChanged int
	// StatsVerified is true when Additions/Deletions/FilesChanged came
	// from a real GET /commits/{sha} fetch (commits.detail_fetched_at IS
	// NOT NULL). Distinguishes "verified zero" from "never fetched" so
	// offline re-audits read the same facts the sync-time audit verified.
	StatsVerified bool
	// ParentSHAs are the commit's parent hashes in order (first parent
	// first). Load-bearing for §4's positional post-approval check: the
	// first-parent walk from a PR's head down to an approval's CommitID
	// defines "committed after the approval" by GRAPH ANCESTRY, which —
	// unlike committer timestamps — cannot be backdated.
	ParentSHAs []string
	Branch     string
	Href       string
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
// BaseBranch is the target ref the PR merged into (e.g. "main"). It is
// the delivery destination: §7's landing-scoped verdict credits a PR's
// approval only when BaseBranch is an audited branch, so a review scoped
// to a sibling branch (gitflow `feat → dev`) cannot vouch for a
// protected-branch landing. GitHub sets it from `pull_request.base.ref`.
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
	BaseBranch     string
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
	// DismissedAt / DismissedState resolve the in-place mutation GitHub
	// performs on dismissal (State flips to DISMISSED while SubmittedAt
	// keeps the original submission time): the issue-timeline
	// `review_dismissed` event supplies WHEN the dismissal happened and
	// what the review's state was at that moment ("approved",
	// "changes_requested", "commented"). Zero/empty when the review was
	// never dismissed or the event wasn't resolved (legacy rows) — the
	// audit then fails closed and surfaces the ambiguity.
	DismissedAt    time.Time
	DismissedState string
	Href           string
}

// A ReviewDismissal is the timeline fact about one dismissed review:
// when it was dismissed and the state it held until then.
//
//	GET /issues/{n}/events (review_dismissed) ──→ ReviewDismissal ──→ Review.DismissedAt/-State
type ReviewDismissal struct {
	At            time.Time
	OriginalState string // approved, changes_requested, commented
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
	Org              string
	Repo             string
	SHA              string
	IsEmptyCommit    bool
	IsBot            bool // author name ends with [bot] — informational only
	IsExemptAuthor   bool // author is on the configured exemption list
	HasPR            bool
	PRNumber         int
	PRCount          int
	HasFinalApproval bool
	HasStaleApproval bool // approval exists but on a pre-force-push commit
	// HasPostMergeConcern is true when a review event challenges the
	// merge-time verdict after the fact: a CHANGES_REQUESTED or DISMISSED
	// review submitted after the merge, a resolved post-merge dismissal of
	// a merge-time review (the original state is restored for the verdict;
	// the dismissal itself is the concern), or a surviving DISMISSED state
	// whose dismissal time is unknown (legacy rows — ambiguous, surfaced
	// for an auditor). Informational — does not affect IsCompliant
	// (compliance is evaluated point-in-time at merge).
	HasPostMergeConcern bool
	// IsCleanRevert is true when the commit is a clean revert of a prior
	// commit. Both bot auto-reverts and manual reverts must be diff-verified:
	// it is set only when RevertVerification == "diff-verified". A forgeable
	// revert message never sets it on its own.
	// Compliance-bearing: rule §8 (clean-revert waiver) flips an otherwise
	// non-compliant verdict to compliant when this is true.
	IsCleanRevert bool
	// RevertVerification records how IsCleanRevert was determined.
	// One of: "" / "none" (not a revert), "message-only" (a revert — auto or
	// manual — whose referenced commit could not be resolved or fetched, so
	// the diff was never compared), "diff-verified" (the revert's diff is the
	// exact inverse of the referenced commit), "diff-mismatch" (the diff is
	// not a clean inverse). Only "diff-verified" waives.
	RevertVerification string
	// RevertedSHA is the SHA of the commit being reverted, extracted from
	// the revert commit's message. Empty if not a revert.
	RevertedSHA string
	// IsCleanMerge is true when the commit is a two-parent merge produced
	// by GitHub's merge button: `Merge pull request #…` message, committer
	// web-flow, and a GitHub-verified signature (see github.ClassifyMerge).
	// The button refuses to merge under conflicts, so such a commit carries
	// no committer-authored code. Informational for compliance, but it
	// exempts the commit-author check in rule §5 (self-approval).
	IsCleanMerge bool
	// MergeVerification records how the merge was classified.
	// One of: "" / "none" (squash or single-parent), "verified-merge-bot"
	// (CleanMerge — 2 parents, merge-button message, web-flow committer,
	// verified signature), "dirty" (2 parents, missing one of those
	// signals — may carry conflict-resolution or edits), "octopus"
	// (3+ parents, not auto-classified).
	MergeVerification    string
	IsSelfApproved       bool // true if only approvals are from code contributors
	ApproverLogins       []string
	OwnerApprovalCheck   string // success, failure, missing
	IsCompliant          bool
	Reasons              []string
	MergeStrategy        string // initial, merge, squash, rebase, direct-push
	PRCommitAuthorLogins []string
	CommitHref           string
	PRHref               string
	// Annotations are informational tags attached by the audit's detector
	// pass (see internal/sync/annotations.go). They describe structural
	// patterns — automation/dep-bump markers, etc. — without affecting
	// IsCompliant. Reviewers can filter by tag to triage automated or
	// automation-adjacent PRs. Format is "<family>:<kv>" (e.g.
	// "automation:depex") so the XLSX can filter by prefix.
	Annotations []string
	AuditedAt   time.Time
}

// A SyncCursor tracks incremental sync progress for a single
// repo+branch pair. On each run, only commits after LastDate are
// fetched from GitHub.
//
//	RepoInfo ──→ SyncCursor
type SyncCursor struct {
	Org    string
	Repo   string
	Branch string
	// LastDate is the newest committer date seen on the branch — the
	// date-window resume point (with overlap) when LastSHA can't drive a
	// graph-based compare.
	LastDate time.Time
	// LastSHA is the branch tip observed at the end of the last sync.
	// When set, the next incremental sync prefers the compare API
	// (last_sha...head), which is graph-based and immune to
	// committer-date backdating. Empty on legacy cursors.
	LastSHA   string
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
//   - ID is the ONLY matching key. It's the immutable numeric GitHub
//     account ID — never reused across deletions, never transferred by
//     renames, not forgeable. An entry without an id never matches.
//   - Login, Type, and Name are display-only metadata captured at
//     resolution time. Login is never used for matching — renames +
//     90-day cooldown can transfer a username to a different account.
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
	// VerifiedEmails is RETIRED (2026-06). A git-author email is set by
	// the pushing client and GitHub does not bind it to an account when it
	// can't verify it, so matching it let a forged email waive unreviewed
	// code. The field is kept only so config validation can DETECT it in
	// existing configs and reject loudly with a migration message — it is
	// never consulted for matching. Exempt by immutable account id.
	VerifiedEmails []string `yaml:"verified_emails,omitempty"`
}
