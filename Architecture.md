# Architecture

## What gh-audit detects

For every commit on a protected branch, gh-audit evaluates a decision tree (in order):

### 1. Exempt author

If the commit author is in the configured `exemptions.authors` list (case-insensitive match), the commit is **compliant** immediately. No further checks run.

### 2. Empty commit

If `additions == 0 && deletions == 0`, the commit is **compliant** immediately. These are flagged for auditor visibility but don't require review.

### 3. Has associated PR

If the commit has no merged PR (direct push), it is **non-compliant** with reason `no associated pull request`.

### 4. Approval on final commit

For each associated merged PR, gh-audit builds a per-reviewer state map on the PR's head SHA. Only reviews targeting the final commit count — stale approvals (from before a force-push) are ignored.

Per-reviewer resolution: if the same reviewer submits multiple reviews on the final commit, only the latest wins. A `DISMISSED` at 11:00 overrides an `APPROVED` at 10:00.

### 5. Self-approval detection

A review is self-approval if the reviewer matches any of:
- PR author
- Commit author
- Committer (excluding GitHub merge bots: `web-flow`, `github`)
- Any co-author (from `Co-authored-by:` trailers)

If the only approvals are self-approvals, the commit is **non-compliant**.

### 6. Required status checks

Configured checks (e.g. `Owner Approval`) must appear on the PR's head SHA with the expected conclusion. Missing or failed checks make the commit **non-compliant**.

### 7. Compliance verdict

A commit is **compliant** if at least one associated PR has:
- A non-self approval on the final commit, AND
- All required checks passed

If multiple PRs exist, gh-audit picks the one closest to compliant for reporting.

## Data flow

```
GitHub REST API
      │
      ▼
┌─────────────┐     ┌───────────┐     ┌─────────┐     ┌──────────┐
│  Token Pool  │────▶│  REST     │────▶│  Sync   │────▶│  DuckDB  │
│  (rate-limit │     │  Client   │     │ Pipeline │     │          │
│   aware)     │     └───────────┘     └─────────┘     └──────────┘
└─────────────┘                             │                │
                                            ▼                ▼
                                     ┌────────────┐   ┌───────────┐
                                     │  Audit     │   │  Report   │
                                     │  Evaluator │   │  (table,  │
                                     │            │   │  csv,json, │
                                     └────────────┘   │  xlsx)    │
                                                      └───────────┘
```

## Sync pipeline

The sync pipeline runs per-repo, per-branch. Repos sync in parallel (bounded by `concurrency`). Each branch follows these phases:

### Phase 1: Fetch commits

```
ListCommits(org, repo, branch, since, until)
  │
  ▼
UpsertCommits ──▶ commits table
UpsertCommitBranches ──▶ commit_branches table
```

The `since` date comes from (in priority order):
1. `--since` CLI flag
2. Stored sync cursor for this org/repo/branch
3. `initial_lookback_days` config (default 90)

### Phase 2: Enrich

For each unaudited commit (no row in `audit_results`), the enricher calls four REST endpoints:

```
commit SHA
  │
  ├──▶ GET /repos/{o}/{r}/commits/{sha}
  │      → additions, deletions, co-authors
  │
  ├──▶ GET /repos/{o}/{r}/commits/{sha}/pulls
  │      → merged PRs (number, head_sha, author, merged_by)
  │
  ├──▶ GET /repos/{o}/{r}/pulls/{n}/reviews     (per PR)
  │      → reviewer, state, commit_id, submitted_at
  │
  └──▶ GET /repos/{o}/{r}/commits/{head}/check-runs  (per PR head SHA)
         → check name, conclusion
```

Enrichment runs in parallel batches (25 commits/batch, bounded by `enrich_concurrency`). All REST endpoints are fully paginated — no silent truncation.

Results are deduplicated by primary key before writing:

```
UpsertPullRequests ──▶ pull_requests table
UpsertReviews ──▶ reviews table
UpsertCheckRuns ──▶ check_runs table
UpsertCommitPRs ──▶ commit_prs table
```

### Phase 3: Audit

Each unaudited commit is evaluated by `EvaluateCommit()` using the enrichment data. Results are written to `audit_results`.

### Phase 4: Cursor update

The sync cursor is updated to the latest commit date, so the next sync picks up where this one left off.

## Database schema

DuckDB with 8 tables:

| Table | Primary Key | Purpose |
|---|---|---|
| `sync_cursors` | (org, repo, branch) | Incremental sync progress |
| `commits` | (org, repo, sha) | Git commits from GitHub |
| `commit_branches` | (org, repo, sha, branch) | Which branches a commit appears on |
| `commit_prs` | (org, repo, sha, pr_number) | Commit → PR associations |
| `pull_requests` | (org, repo, number) | GitHub pull requests |
| `reviews` | (org, repo, pr_number, review_id) | PR reviews with per-reviewer state |
| `check_runs` | (org, repo, commit_sha, check_run_id) | CI/CD check results |
| `audit_results` | (org, repo, sha) | Compliance verdicts with reasons |

Bulk writes use the DuckDB Appender API (staging table → INSERT OR REPLACE). All writes go through a serialized `DBWriter` — DuckDB supports concurrent reads but single-writer.

## Token pool

Multiple GitHub tokens (PAT or App) can be configured, each scoped to specific orgs/repos. The pool:

- Tracks `x-ratelimit-remaining` and `x-ratelimit-reset` from response headers
- Picks the token with the most remaining quota for each request
- Blocks and waits for reset when all tokens are exhausted
- Handles 429 (retry after delay) and 403 abuse detection
- Disables tokens permanently on 401

Auto-detection fallback: `GH_TOKEN` → `GITHUB_TOKEN` → `gh auth token`.

## Report layer

The `report` command queries `audit_results` joined with `commits` and `pull_requests`. Four output formats:

- **table**: ASCII summary + details to stdout
- **csv**: Per-commit rows with all fields
- **json**: `{ summary: [...], details: [...] }`
- **xlsx**: 5-sheet workbook (Summary, All Commits, Non-Compliant, Exemptions, Self-Approved) with hyperlinks, conditional formatting, and auto-filters

Summary columns are partitioned: `Total = Compliant + Non-Compliant`. Bots, Exempt, Empty, and Self-Approved are cross-cutting annotations that overlap with the primary partition.

## Package structure

```
cmd/                     CLI commands (sync, report, config)
internal/
  config/                YAML config loading, validation, defaults
  db/                    DuckDB schema, migrations, bulk upsert
  github/
    client.go            REST API client (all endpoints)
    tokenpool.go         Multi-token management with rate limiting
  model/
    types.go             Domain types (Commit, PR, Review, CheckRun, AuditResult)
  report/
    report.go            Summary/detail queries, table/csv/json formatting
    xlsx.go              Multi-sheet XLSX generation
  sync/
    pipeline.go          Orchestration (discover → fetch → enrich → audit)
    audit.go             EvaluateCommit decision tree
    dbwriter.go          Serialized write channel for DuckDB
    progress.go          Sync phase tracking
```

## Concurrency model

- **Repo sync**: `concurrency` goroutines via `errgroup` (default 10)
- **Enrichment**: `enrich_concurrency` goroutines per repo (default 4)
- **DB writes**: Single `DBWriter` goroutine per pipeline run — all writes serialized through a buffered channel
- **DB reads**: Safe to run concurrently (DuckDB MVCC)

## Rate limits

GitHub REST API: 5000 requests/hour per token. Cost per commit: ~4 requests (detail + PRs + reviews + check runs). One token audits ~1250 commits/hour. Multiple tokens multiply throughput linearly.
