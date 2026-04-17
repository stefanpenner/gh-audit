package db

// ENUM type DDL statements. DuckDB does not support CREATE TYPE IF NOT EXISTS,
// so the migrate function ignores "already exists" errors for these.
const (
	createReviewStateEnum = `CREATE TYPE review_state AS ENUM (
		'APPROVED', 'CHANGES_REQUESTED', 'COMMENTED', 'DISMISSED'
	)`

	createOwnerApprovalCheckEnum = `CREATE TYPE owner_approval_check AS ENUM (
		'success', 'failure', 'missing'
	)`
)

// Schema DDL statements. All use CREATE TABLE IF NOT EXISTS for idempotency.
const (
	createSyncCursors = `CREATE TABLE IF NOT EXISTS sync_cursors (
		org        TEXT NOT NULL,
		repo       TEXT NOT NULL,
		branch     TEXT NOT NULL DEFAULT '',
		last_date  TIMESTAMP,
		updated_at TIMESTAMP,
		PRIMARY KEY (org, repo, branch)
	)`

	createCommits = `CREATE TABLE IF NOT EXISTS commits (
		org              TEXT NOT NULL,
		repo             TEXT NOT NULL,
		sha              TEXT NOT NULL,
		author_login     TEXT,
		author_email     TEXT,
		committer_login  TEXT,
		committed_at     TIMESTAMP,
		message          TEXT,
		parent_count     INTEGER,
		additions        INTEGER,
		deletions        INTEGER,
		href             TEXT,
		fetched_at       TIMESTAMP DEFAULT current_timestamp,
		PRIMARY KEY (org, repo, sha)
	)`

	createCommitPRs = `CREATE TABLE IF NOT EXISTS commit_prs (
		org       TEXT NOT NULL,
		repo      TEXT NOT NULL,
		sha       TEXT NOT NULL,
		pr_number INTEGER NOT NULL,
		PRIMARY KEY (org, repo, sha, pr_number)
	)`

	createPullRequests = `CREATE TABLE IF NOT EXISTS pull_requests (
		org              TEXT NOT NULL,
		repo             TEXT NOT NULL,
		number           INTEGER NOT NULL,
		title            TEXT,
		merged           BOOLEAN,
		head_sha         TEXT,
		merge_commit_sha TEXT,
		author_login     TEXT,
		merged_by_login  TEXT,
		merged_at        TIMESTAMP,
		href             TEXT,
		fetched_at       TIMESTAMP DEFAULT current_timestamp,
		PRIMARY KEY (org, repo, number)
	)`

	createReviews = `CREATE TABLE IF NOT EXISTS reviews (
		org            TEXT NOT NULL,
		repo           TEXT NOT NULL,
		pr_number      INTEGER NOT NULL,
		review_id      BIGINT NOT NULL,
		reviewer_login TEXT,
		state          review_state,
		commit_id      TEXT,
		submitted_at   TIMESTAMP,
		href           TEXT,
		fetched_at     TIMESTAMP DEFAULT current_timestamp,
		PRIMARY KEY (org, repo, pr_number, review_id)
	)`

	createCheckRuns = `CREATE TABLE IF NOT EXISTS check_runs (
		org           TEXT NOT NULL,
		repo          TEXT NOT NULL,
		commit_sha    TEXT NOT NULL,
		check_run_id  BIGINT NOT NULL,
		check_name    TEXT,
		status        TEXT,
		conclusion    TEXT,
		completed_at  TIMESTAMP,
		fetched_at    TIMESTAMP DEFAULT current_timestamp,
		PRIMARY KEY (org, repo, commit_sha, check_run_id)
	)`

	createCommitBranches = `CREATE TABLE IF NOT EXISTS commit_branches (
		org    TEXT NOT NULL,
		repo   TEXT NOT NULL,
		sha    TEXT NOT NULL,
		branch TEXT NOT NULL,
		PRIMARY KEY (org, repo, sha, branch)
	)`

	createAuditResults = `CREATE TABLE IF NOT EXISTS audit_results (
		org                  TEXT NOT NULL,
		repo                 TEXT NOT NULL,
		sha                  TEXT NOT NULL,
		is_empty_commit      BOOLEAN,
		is_bot               BOOLEAN,
		is_exempt_author     BOOLEAN,
		has_pr               BOOLEAN,
		pr_number            INTEGER,
		has_final_approval   BOOLEAN,
		is_self_approved     BOOLEAN,
		approver_logins      TEXT[],
		owner_approval_check owner_approval_check,
		is_compliant         BOOLEAN,
		reasons              TEXT[],
		commit_href          TEXT,
		pr_href              TEXT,
		audited_at           TIMESTAMP DEFAULT current_timestamp,
		PRIMARY KEY (org, repo, sha)
	)`
)

// enumTypes is the ordered list of CREATE TYPE statements to run during migration.
// DuckDB lacks IF NOT EXISTS for types, so migrate ignores "already exists" errors.
var enumTypes = []string{
	createReviewStateEnum,
	createOwnerApprovalCheckEnum,
}

// addColumnMigrations adds columns introduced after initial release.
// Each ALTER is idempotent: the migrate function ignores "already exists" errors.
var addColumnMigrations = []string{
	`ALTER TABLE commits ADD COLUMN committer_login TEXT`,
}

// allTables is the ordered list of DDL statements to run during migration.
var allTables = []string{
	createSyncCursors,
	createCommits,
	createCommitPRs,
	createCommitBranches,
	createPullRequests,
	createReviews,
	createCheckRuns,
	createAuditResults,
}
