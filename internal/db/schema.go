package db

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
		org            TEXT NOT NULL,
		repo           TEXT NOT NULL,
		sha            TEXT NOT NULL,
		author_login   TEXT,
		author_email   TEXT,
		committed_at   TIMESTAMP,
		message        TEXT,
		parent_count   INTEGER,
		additions      INTEGER,
		deletions      INTEGER,
		href           TEXT,
		fetched_at     TIMESTAMP DEFAULT current_timestamp,
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
		state          TEXT,
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
		has_pr               BOOLEAN,
		pr_number            INTEGER,
		has_final_approval   BOOLEAN,
		approver_logins      TEXT[],
		owner_approval_check TEXT,
		is_compliant         BOOLEAN,
		reasons              TEXT[],
		commit_href          TEXT,
		pr_href              TEXT,
		audited_at           TIMESTAMP DEFAULT current_timestamp,
		PRIMARY KEY (org, repo, sha)
	)`
)

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
