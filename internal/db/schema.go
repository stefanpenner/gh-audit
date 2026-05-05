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
		author_id        BIGINT,
		author_email     TEXT,
		committer_login  TEXT,
		committed_at     TIMESTAMP,
		message          TEXT,
		parent_count     INTEGER,
		additions        INTEGER,
		deletions        INTEGER,
		is_verified      BOOLEAN DEFAULT false,
		href             TEXT,
		fetched_at       TIMESTAMP DEFAULT current_timestamp,
		PRIMARY KEY (org, repo, sha)
	)`

	createCoAuthors = `CREATE TABLE IF NOT EXISTS co_authors (
		org    TEXT NOT NULL,
		repo   TEXT NOT NULL,
		sha    TEXT NOT NULL,
		name   TEXT,
		email  TEXT NOT NULL,
		login  TEXT,
		PRIMARY KEY (org, repo, sha, email)
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
		head_branch      TEXT,
		merge_commit_sha TEXT,
		author_login     TEXT,
		author_id        BIGINT,
		merged_by_login  TEXT,
		merged_by_id     BIGINT,
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
		reviewer_id    BIGINT DEFAULT 0,
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

	// org_repos_cache memoises the result of GitHub's
	// /orgs/{org}/repos enumeration. fetched_at is the same on every
	// row in a given org because cache replacement is atomic per-org
	// (DELETE WHERE org=? then INSERT). Reads filter against
	// fetched_at < now - freshness to skip the cache.
	createOrgReposCache = `CREATE TABLE IF NOT EXISTS org_repos_cache (
		org            TEXT NOT NULL,
		name           TEXT NOT NULL,
		full_name      TEXT,
		default_branch TEXT,
		archived       BOOLEAN,
		fetched_at     TIMESTAMP NOT NULL,
		PRIMARY KEY (org, name)
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
		pr_count             INTEGER DEFAULT 0,
		has_final_approval   BOOLEAN,
		has_stale_approval   BOOLEAN DEFAULT false,
		has_post_merge_concern BOOLEAN DEFAULT false,
		is_clean_revert        BOOLEAN DEFAULT false,
		revert_verification    TEXT,
		reverted_sha           TEXT,
		is_clean_merge         BOOLEAN DEFAULT false,
		merge_verification     TEXT,
		is_self_approved     BOOLEAN,
		approver_logins      TEXT[],
		owner_approval_check owner_approval_check,
		is_compliant         BOOLEAN,
		reasons              TEXT[],
		merge_strategy            TEXT,
		pr_commit_author_logins   TEXT[],
		commit_href               TEXT,
		pr_href              TEXT,
		annotations          TEXT[],
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
	`ALTER TABLE audit_results ADD COLUMN has_stale_approval BOOLEAN DEFAULT false`,
	`ALTER TABLE audit_results ADD COLUMN pr_count INTEGER DEFAULT 0`,
	`ALTER TABLE audit_results ADD COLUMN merge_strategy TEXT`,
	`ALTER TABLE audit_results ADD COLUMN pr_commit_author_logins TEXT[]`,
	`ALTER TABLE pull_requests ADD COLUMN head_branch TEXT`,
	`ALTER TABLE audit_results ADD COLUMN has_post_merge_concern BOOLEAN DEFAULT false`,
	`ALTER TABLE audit_results ADD COLUMN is_clean_revert BOOLEAN DEFAULT false`,
	`ALTER TABLE audit_results ADD COLUMN revert_verification TEXT`,
	`ALTER TABLE audit_results ADD COLUMN reverted_sha TEXT`,
	`ALTER TABLE audit_results ADD COLUMN is_clean_merge BOOLEAN DEFAULT false`,
	`ALTER TABLE audit_results ADD COLUMN merge_verification TEXT`,
	`ALTER TABLE audit_results ADD COLUMN annotations TEXT[]`,
	`ALTER TABLE commits ADD COLUMN is_verified BOOLEAN DEFAULT false`,
	`ALTER TABLE commits ADD COLUMN author_id BIGINT`,
	`ALTER TABLE reviews ADD COLUMN reviewer_id BIGINT DEFAULT 0`,
	`ALTER TABLE pull_requests ADD COLUMN author_id BIGINT`,
	`ALTER TABLE pull_requests ADD COLUMN merged_by_id BIGINT`,
}

// allTables is the ordered list of DDL statements to run during migration.
var allTables = []string{
	createSyncCursors,
	createCommits,
	createCoAuthors,
	createCommitPRs,
	createCommitBranches,
	createPullRequests,
	createReviews,
	createCheckRuns,
	createAuditResults,
	createOrgReposCache,
}
