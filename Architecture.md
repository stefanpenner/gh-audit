# Architecture

## What gh-audit detects

For every commit on a protected branch, gh-audit evaluates a decision tree (in order). Rules 1–6 are the primary check; rules 7–8 are fallbacks that can still flip a non-compliant verdict to compliant. A separate `HasPostMergeConcern` flag is orthogonal to compliance and tracks reviews submitted *after* merge (see rule 4).

### 1. Exempt author

If the commit author's GitHub login is in the configured `exemptions.authors` list (case-insensitive match against `commit.author_login` — not email, not display name), the commit is **compliant** immediately. No further checks run.

`author_login` is the GitHub handle (e.g. `stefanpenner`, `dependabot[bot]`) — not `author.name` or `author.email`. Empty if the commit email isn't linked to a GitHub account, in which case no exemption can match.

```
config.yaml: exemptions.authors      ← SOT: user-configured list
      │
      ▼
GET /repos/{o}/{r}/commits/{sha}
      → commit.author_login           ← SOT: GitHub REST API
      │
      ▼
EvaluateCommit (audit.go)
      → case-insensitive match?
          yes → IsCompliant=true, IsExemptAuthor=true, reason="exempt: configured author"
          no  → continue to rule 2
```

### 2. Empty commit

If `additions == 0 && deletions == 0`, the commit is **compliant** (flagged for visibility, no review required).

`applyEmptyCommitFallback` (`audit.go`) runs lazily — only on paths heading to non-compliant: once when there's no PR, and again after all PRs fail. Already-compliant commits skip the `GetCommitDetail` REST call entirely.

```
GET /repos/{o}/{r}/commits/{sha}
      → commit.additions, commit.deletions   ← SOT: GitHub REST API (commit detail)
      │
      ▼
EvaluateCommit (audit.go)
      → additions == 0 && deletions == 0?
          yes → IsCompliant=true, IsEmptyCommit=true, reason="empty commit"
          no  → continue to rule 3
```

### 3. Has associated PR

If the commit has no merged PR (direct push), it is **non-compliant** with reason `no associated pull request`.

```
GET /repos/{o}/{r}/commits/{sha}/pulls
      → []PullRequest (merged only)           ← SOT: GitHub REST API (best-effort index)
      │
      ▼
EvaluateCommit (audit.go)
      → len(PRs) == 0?
          yes → recover via parse + canonical verify (see below)
                → still 0? IsCompliant=false, HasPR=false, reason="no associated pull request"
          no  → PRCount=len(PRs), continue to rule 4
```

#### Commit→PR index gap

GitHub exposes the commit→PR relationship through two distinct surfaces:

| Direction | Source | Trustworthiness |
|---|---|---|
| Commit → PR | `GET /commits/{sha}/pulls` (and GraphQL `Commit.associatedPullRequests`, REST/GraphQL search by SHA) | **Best-effort reverse index.** Asynchronous, computed by GitHub from refs and ref-events. Has empirically observed gaps from indexer races on burst merges, schema migrations, and squash/rebase commit-SHA chases. No SLA. |
| PR → commit | `PullRequest.merge_commit_sha` | **Canonical.** Set by GitHub atomically at merge time. Immutable. Never the gap. |

Empirically (last full sweep across 242 repos × 30 days), ~0.12% of commits surfaced as "no associated pull request" despite the PR clearly existing — `/commits/{sha}/pulls` returned `[]` while `/pulls/{N}.merge_commit_sha` matched the audited SHA. None of GitHub's alternative discovery APIs (GraphQL search, REST search) recovered the link either.

##### Mitigation: parse + canonical verify

When `/commits/{sha}/pulls` returns empty, gh-audit's caching layer (`recoverPRFromMergeMessage` in `internal/github/caching.go`) attempts a recovery:

1. **Parse** the trailing `(#N)` token from the squash-merge commit's first line via `ParsePRReference` (`internal/github/merge.go`). Strict regex `\(#(\d+)\)\s*$` against the first line, so revert-of-squash titles like `Revert "Foo (#100)" (#101)` resolve to `101`, not `100`.
2. **Fetch** PR #N via `getPR` (DB-frozen for previously-synced merged PRs; one extra `GET /pulls/N` if cold).
3. **Verify canonically**: accept the link **only if** `pr.merged && pr.merge_commit_sha == sha`.

The split is deliberate. The parse step is a forgeable hint — a commit author can write any `(#N)` they want into the message. The verify step is unforgeable: only GitHub sets `merge_commit_sha`, and only on a real merge event for the actual PR. A commit message claiming `(#1234)` cannot make `pulls/1234.merge_commit_sha` equal that commit's SHA.

Failure modes that still fire §3:
- Commit message has no `(#N)` (cross-fork PRs without an annotation, manual merges from local).
- Parsed PR exists but isn't merged or its `merge_commit_sha` doesn't match the audited SHA — verification rejects the hint.
- Fetch error — fail-closed; never silently accept an unverified link.

Telemetry exposes recovery counts as `pr_recovered` in the per-endpoint breakdown so we can track how often the index gap fires in production.

### 4. Approval on final commit

For each associated merged PR, gh-audit builds a per-reviewer state map on the PR's head SHA. Only reviews targeting the final commit count.

Per-reviewer resolution: if the same reviewer submits multiple reviews on the final commit, only the latest state-changing review wins. A `DISMISSED` or `CHANGES_REQUESTED` at 11:00 overrides an `APPROVED` at 10:00. A later plain `COMMENTED` review does **not** clobber an earlier `APPROVED` from the same reviewer — matching GitHub's UI, where commenting after approving leaves the approval intact.

**Post-merge cutoff.** Reviews submitted after `pr.merged_at` are excluded from compliance. A post-merge `DISMISSED` or `CHANGES_REQUESTED` instead sets `HasPostMergeConcern=true` so auditors can review the concern on the dedicated XLSX sheet without the commit itself flipping state.

```
GET /repos/{o}/{r}/pulls/{n}/reviews
      → []Review (reviewer_login, state, commit_id, submitted_at)   ← SOT: GitHub REST API
      │
      ▼
Filter: review.commit_id == pr.head_sha?
      │                          │
      yes (on final commit)      no (stale)
      │                          │
      ▼                          ▼
Per-reviewer latest state    Stale approval check:
map (by submitted_at)        any APPROVED on older SHA
      │                      from non-self reviewer?
      ▼                          │
Any APPROVED (non-self)?     yes → HasStaleApproval=true
      │                          reason="approval is stale —
      yes → continue to          not on final commit"
            rule 5/6         no  → reason="no approval on
      no  → check stale ────→    final commit"
```

**Stale approval detection**: When no approval exists on the final commit but an approval exists on an earlier SHA, the reason is `approval is stale — not on final commit` instead of `no approval on final commit`. This distinguishes "never reviewed" from "reviewed, then code changed."

### 
A review is self-approval if the reviewer's **immutable numeric GitHub ID** matches any of:
- PR author (`AuthorID`)
- Commit author (`AuthorID`) — skipped for `CleanMerge` commits (see below)
- Any **PR-branch commit author** (`AuthorID`) with a non-empty contribution — catches squash-merge cases where the reviewer pushed real code that landed in the squash. Authors whose every PR-branch commit is zero-diff (the prototypical "Empty commit to rerun check") are dropped from this set; see "Empty-commit exclusion" below.

**ID-only matching**: All identity comparison uses immutable numeric GitHub account IDs — never login strings. IDs are never reused, never transferred by renames, and not forgeable. A review with `ReviewerID == 0` (deleted/ghost account, unresolved identity) is not trusted: it cannot count as self-approval, nor as an independent approval. This eliminates login-rename attacks and casing ambiguities.

**CleanMerge exclusion**: A `CleanMerge` (2 parents + `Merge pull request #…` message + `web-flow` committer + verified GitHub signature — see [ClassifyMerge](#classifymerge-internalgithubmergego)) cannot contain author-contributed code. GitHub's merge button refuses to produce one when there are conflicts, and the verified `web-flow` signature can't be forged locally. For these commits the author is just "who clicked merge," so skipping the `AuthorID` check avoids false positives. `DirtyMerge` (any 2-parent merge missing one of those signals) and `OctopusMerge` (3+ parents) may contain conflict-resolution or edits authored by the commit author, so the check still runs.

**Empty-commit exclusion** (PR-branch authors only): a reviewer who pushed only zero-diff commits onto the PR branch — typically `Empty commit to rerun check` to re-trigger CI — has not contributed code and must not invalidate their own review. The check verifies emptiness against the commit's actual `additions`/`deletions`. The `/pulls/{n}/commits` listing endpoint omits diff stats, so when an author's listed contributions all *appear* zero locally, `GetCommitDetail` is fetched lazily (DB-cached) to disambiguate a truly empty commit from un-fetched stats. Any non-zero stat short-circuits before any API call. A fetch error fails open (treats as contributor) so we never silently downgrade a real contributor.

**Excluded identity sources** (intentionally not checked):
- **Committer login** — GitHub does not provide a CommitterID on the commit API object. Login-only comparison is forgery-prone and mutable.
- **Co-authored-by trailers** — unvalidated commit message text; trivially forgeable. No API-resolved ID available.

If the only approvals are self-approvals (or all approvals are from unresolved identities), the commit is **non-compliant**.

```
review.ReviewerID                    ← SOT: GitHub REST API (reviews → user.id)
      │
      ▼
isSelfApproval (audit.go) — ID-only matching via sameUser():
      │
      ├── pr.AuthorID                    ← SOT: GET /commits/{sha}/pulls → user.id
      ├── commit.AuthorID                ← SOT: GET /commits/{sha} → author.id (skip if CleanMerge)
      └── pr_branch_commits[].AuthorID   ← SOT: GET /pulls/{n}/commits → author.id
                                              (filtered: drop authors whose every contribution is empty;
                                              GetCommitDetail fetched lazily when local stats are zero)
      │
      ▼
All approvals are self (or ReviewerID==0)?
      yes → IsSelfApproved=true, reason="self-approved (reviewer is code author)"
      no  → at least one verified independent approval exists, continue to rule 6
```

### 6. Required status checks

Configured checks (e.g. `Owner Approval`) must appear on the PR's head SHA with the expected conclusion. Missing or failed checks make the commit **non-compliant**.

```
config.yaml: audit_rules.required_checks   ← SOT: user-configured list
      │
      ▼
GET /repos/{o}/{r}/commits/{head_sha}/check-runs
      → []CheckRun (check_name, conclusion)   ← SOT: GitHub REST API
      │
      ▼
evaluateRequiredChecks (audit.go)
      → for each required check:
          found with matching conclusion? → "success"
          found with wrong conclusion?   → "failure"
          not found?                     → "missing"
      │
      ▼
All "success"?
      yes → continue to verdict
      no  → reason="Owner Approval check missing/failed"
```

### 7. Compliance verdict

A commit is **compliant** if at least one associated PR has:
- A non-self approval on the final commit, AND
- All required checks passed

If multiple PRs exist for a commit, gh-audit picks the one closest to compliant for reporting. The total number of associated PRs is recorded (`pr_count`) and commits with `pr_count > 1` appear in the dedicated "Multiple PRs" report sheet.

### 8. Clean-revert waiver (standalone)

If the primary verdict above is **non-compliant**, one last check runs. It is evaluated per-commit — it does not look at the reverted commit's audit verdict (see `TODO.md` for the deferred cross-commit variant).

A `IsCleanRevert=true` commit is **compliant**. The signal means one of:
- `AutoRevert` — bot-generated, trusted by construction.
- `ManualRevert` whose diff was verified as the exact inverse of the reverted commit (`revert_verification = "diff-verified"`).

Every other revert shape — conflict-resolved (`diff-mismatch`), message-only, revert-of-revert, hand-crafted — falls through to the normal PR-approval rules. Provenance signals like `committer == web-flow` or a verified signature are **not** sufficient on their own: if the diff isn't a pure inverse, there are bytes on master that weren't there before, and those bytes deserve review.

```
non-compliant verdict from rules 1–7
      │
      ▼
IsCleanRevert == true?
      │
      yes ──▶ IsCompliant=true, reason="clean revert of <sha12>"
      no  ──▶ stay non-compliant (PR-approval reasons preserved)
```

```
EvaluateCommit (audit.go) — final decision:
      │
      ▼
For each associated PR:
      has non-self approval on final commit?
      AND all required checks passed?
          yes → IsCompliant=true, reason="compliant", return early
          no  → track as candidate (fewest reasons = closest to compliant)
      │
      ▼
No PR satisfied all checks:
      → IsCompliant=false
      → report best PR's reasons
      → set IsSelfApproved, HasStaleApproval flags
      │
      ▼
Write to audit_results table → surface in report
```

## Data flow

```
GitHub REST API
      │
      ▼
┌────────────-─┐     ┌───────────┐     ┌──────────┐     ┌──────────┐
│  Token Pool  │────▶│  REST     │────▶│  Sync    │────▶│  DuckDB  │
│  (rate-limit │     │  Client   │     │ Pipeline │     │          │
│   aware)     │     └───────────┘     └──────────┘     └──────────┘
└─────────────-┘                             │                │
                                             ▼                ▼
                                     ┌────────────┐   ┌────────────┐
                                     │  Audit     │   │  Report    │
                                     │  Evaluator │   │  (table,   │
                                     │            │   │  csv,json, │
                                     └────────────┘   │  xlsx)     │
                                                      └────────────┘
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

**`commit_branches` column provenance:**

| Column | Source |
|--------|--------|
| `org` | YAML config — the organisation key under `orgs:` |
| `repo` | YAML config repos list, or auto-discovered via `GET /orgs/{org}/repos` |
| `sha` | Each commit's SHA returned by `GET /repos/{o}/{r}/commits?sha={branch}&since=…&until=…` (fully paginated) |
| `branch` | YAML config `branches:` list for the org; falls back to the repo's default branch if unset |

### Phase 2: Enrich

For each unaudited commit (no row in `audit_results`), the enricher calls five REST endpoints:

```
commit SHA
  │
  ├──▶ GET /repos/{o}/{r}/commits/{sha}
  │      → additions, deletions, co-authors
  │
  ├──▶ GET /repos/{o}/{r}/commits/{sha}/pulls
  │      → merged PRs (number, head_sha, author)
  │
  ├──▶ GET /repos/{o}/{r}/pulls/{n}             (per PR)
  │      → merged_by, full head_sha (backfills fields missing from /commits/{sha}/pulls)
  │
  ├──�� GET /repos/{o}/{r}/pulls/{n}/reviews     (per PR)
  │      → reviewer, state, commit_id, submitted_at
  │
  ├──▶ GET /repos/{o}/{r}/commits/{head}/check-runs  (per PR head SHA)
  │      → check name, conclusion
  │
  ├──▶ GET /repos/{o}/{r}/pulls/{n}/commits          (per PR, for self-approval expansion)
  │      → distinct PR-branch commit authors
  │
  └──▶ GET /repos/{o}/{r}/commits/{sha}              (only for revert classification)
         → file diffs for clean-revert verification (ManualRevert only)
```

Enrichment runs in parallel batches (25 commits/batch, bounded by `enrich_concurrency`). Inside a batch, commits are enriched concurrently (bounded by `enrichCommitFanout`, default 10), and PRs within a single commit are fetched concurrently as well (bounded by `enrichPRFanout`, default 5). All REST endpoints are fully paginated — no silent truncation.

Enrichment goes through `CachingEnricher` (see [Caching layer](#caching-layer)), which resolves many of these calls from the DB instead of the API and tracks per-endpoint hit/miss counts in `APIStats`.

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

DuckDB with 9 tables:

| Table | Primary Key | Purpose |
|---|---|---|
| `sync_cursors` | (org, repo, branch) | Incremental sync progress |
| `commits` | (org, repo, sha) | Git commits from GitHub |
| `co_authors` | (org, repo, sha, email) | Co-authors parsed from "Co-authored-by:" trailers |
| `commit_branches` | (org, repo, sha, branch) | Which branches a commit appears on |
| `commit_prs` | (org, repo, sha, pr_number) | Commit → PR associations |
| `pull_requests` | (org, repo, number) | GitHub pull requests |
| `reviews` | (org, repo, pr_number, review_id) | PR reviews with per-reviewer state |
| `check_runs` | (org, repo, commit_sha, check_run_id) | CI/CD check results |
| `audit_results` | (org, repo, sha) | Compliance verdicts with reasons |

Bulk writes use the DuckDB Appender API (staging table → INSERT OR REPLACE). All writes go through a serialized `DBWriter` — DuckDB supports concurrent reads but single-writer.

## Token pool

The token pool manages a heterogeneous set of GitHub credentials. Two token kinds are supported:

| Kind | Config fields | Auth mechanism |
|------|---------------|----------------|
| **PAT** (`kind: pat`) | `env` (env var name) | Bearer token header |
| **App** (`kind: app`) | `app_id`, `installation_id`, `private_key_path` or `private_key_env` | JWT → installation access token via [ghinstallation](https://github.com/bradleyfalzon/ghinstallation); auto-refreshes before expiry |

Each token carries a list of **scopes** (`org` + optional `repos`) that restrict which org/repo pairs it may be used for. Scope matching is case-insensitive; an empty repos list means all repos in that org.

Auto-detection fallback (when no tokens are configured): `GH_TOKEN` → `GITHUB_TOKEN` → `gh auth token`. The first found is added as a wildcard token with no scope restriction.

### Required GitHub permissions

gh-audit is read-only. The minimum permissions for each token:

| Permission | Scope | Endpoints |
|---|---|---|
| **Contents** | Read | `GET /repos/{o}/{r}/commits`, `GET /repos/{o}/{r}/commits/{sha}` |
| **Pull requests** | Read | `GET /repos/{o}/{r}/commits/{sha}/pulls`, `GET /repos/{o}/{r}/pulls/{n}/reviews` |
| **Checks** | Read | `GET /repos/{o}/{r}/commits/{ref}/check-runs` |
| **Metadata** | Read | `GET /repos/{o}/{r}`, `GET /orgs/{org}/repos` |

Classic PAT: the `repo` scope covers all of the above. Fine-grained PAT or GitHub App: enable Contents, Pull requests, Checks, and Metadata — all read-only.

### Rate limit handling

- Tracks `x-ratelimit-remaining` and `x-ratelimit-reset` from response headers
- Scores each token by `rateRemaining - inFlight` and picks the highest; the in-flight counter prevents concurrent `Pick` calls from herding onto a single token before any response has landed
- Blocks and waits for reset when all matching tokens are exhausted (threshold: 100 remaining)
- Retries on 429 (respects `Retry-After`, defaults to 60s)
- Detects 403 abuse/secondary rate limit responses
- Disables tokens permanently on 401

## Caching layer

Enrichment goes through `CachingEnricher` (`internal/github/caching.go`), which sits between the sync pipeline and the raw REST `Client`. It exists to keep enrichment idempotent and cheap: running `sync` again, or `re-audit`, or `backfill-missing-prs` should not re-fetch data already on disk.

```
enrich(sha)
  │
  ▼
in-memory map (per-run)        ── hit ──▶ APIStats.CacheHits++
  │ miss
  ▼
DB (commits, pull_requests,    ── hit ──▶ APIStats.DBHits++
     reviews, check_runs,                 + populate in-memory map
     commit_prs, co_authors)
  │ miss
  ▼
REST Client ── hit ──▶ per-endpoint APIStats counter
               (CommitDetail / CommitPRs / PRDetail / Reviews /
                CheckRuns / PRCommits / RevertVerification)
```

Key design points:
- **Reverse PR index.** A PR fetched for commit A may also be the merge PR for commit B. `indexPR` populates a reverse map so B's enrichment finds A's PR work without a second API round-trip.
- **Lazy commit detail.** `commits` written by phase 1 already carry most of what the audit needs. `GetCommitDetail` is only called when the decision tree actually needs stats (empty-commit fallback) — saving roughly 16% of REST traffic on a typical run.
- **Fan-out bounds.** `enrichCommitFanout = 10` (per batch) and `enrichPRFanout = 5` (per commit) cap goroutine growth without flooding the token pool.
- **Revert-verification telemetry.** `GetCommitFiles` calls made to diff-verify manual reverts are tracked separately in `APIStats.RevertVerification`, because they're the most expensive per-commit call and worth monitoring on their own.

## Revert & merge classification

Two small classifiers feed the audit tree and the XLSX report.

### `ParseRevert` (`internal/github/revert.go`)

| Kind | Trigger | Clean? |
|---|---|---|
| `NotRevert` | Message has no recognised revert prefix | — |
| `AutoRevert` | `Automatic revert of <new>..<old>` | **Yes** (trusted by construction) |
| `ManualRevert` | `Revert "..."` with `This reverts commit <sha>.` | **Only if** `IsCleanRevertDiff` confirms the diff is the exact inverse of the reverted commit |
| `RevertOfRevert` | Revert-of-revert (re-application) | No — treated as fresh code |

`IsCleanRevertDiff` compares file patches as multisets of added/removed lines; order is ignored. A `ManualRevert` with a failing diff check becomes `revert_verification = "diff-mismatch"` (or `"message-only"` when no trailer SHA was found) and does **not** qualify for rule 8's clean-revert waiver — it falls through to the normal PR-approval rules.

### `ClassifyMerge` (`internal/github/merge.go`)

| Kind | Parents | Extra signals |
|---|---|---|
| `NotMerge` | 0–1 | — |
| `CleanMerge` | 2 | `Merge pull request #…` message AND `committer_login == web-flow` AND `is_verified == true`. All three are required. |
| `DirtyMerge` | 2 | Any missing signal — non-matching message, non-web-flow committer, or unverified signature. Could hide committer-authored code. |
| `OctopusMerge` | 3+ | Rare; typically tooling-generated. Not auto-trusted. |

The `CleanMerge` signal is deliberately strict. Message-only matching is forgeable — anyone can craft a `Merge pull request #…` commit locally. Requiring the `web-flow` committer with a GitHub-verified signature is what makes it trustworthy: only GitHub holds the web-flow signing key, so the signal can't be produced outside GitHub's merge button.

`is_verified` is read from the GitHub REST API's `commit.verification.verified` field (available on both `GET /commits/{sha}` and `GET /repos/{o}/{r}/commits`). It's persisted in the `commits` table so the enrichment DB-read path preserves it.

These flags drive the **Clean Reverts** and **Clean Merges** XLSX sheets, the rule-8 fallback, and the self-approval CleanMerge exclusion (rule 5). They are **informational for compliance** except when `IsCleanRevert` is true and the reverted commit is itself compliant.

### `classifyMergeStrategy` (`internal/sync/audit.go`)

Informational label recorded on every `audit_results` row. Does not affect compliance.

| Strategy | Detection | Typical source |
|---|---|---|
| `initial` | `parent_count == 0` | Repository root commit |
| `merge` | `parent_count > 1` | GitHub's "Create a merge commit" button |
| `squash` | 1 parent, has PR, `committer_login == web-flow` | GitHub's "Squash and merge" button |
| `rebase` | 1 parent, has PR, `committer_login != web-flow` | GitHub's "Rebase and merge" (fast-forward) |
| `direct-push` | 1 parent, no PR | `git push` without a pull request |

**Ambiguity:** Non-fast-forward rebase merges get `committer=web-flow` (GitHub replays the commits), making them indistinguishable from squash merges at the commit level. Feature-branch commits visible on main via a 2-parent merge commit also appear as `rebase` since their original committer is preserved.

## Annotations

`internal/sync/annotations.go` computes a list of informational annotations from each commit's message. They are attached to every `audit_results` row regardless of the compliance path taken, and are **not** load-bearing for compliance today.

- `detectAutomationTag` flags automation/dep-bump markers (Dependabot, Renovate, etc.) so auditors can cross-check against exempt-author configuration.

The `annotate-commits` CLI recomputes these for every existing `audit_results` row without hitting the API — useful after adding a new detector.

## Report layer

The `report` command queries `audit_results` joined with `commits` and `pull_requests`. Four output formats:

- **table**: ASCII summary + details to stdout
- **csv**: Per-commit rows with all fields
- **json**: `{ summary: [...], details: [...] }`
- **xlsx**: 8-sheet workbook organized as three layers — Action → Overview → Trace/Evidence. Each sheet has a single, distinct purpose; the same commit is never fragmented across multiple sheets.

  **Layer 1 — Action**
  1. **README** — legend for rule codes (R1..R8), cell-outcome values, and report period. Static; one-screen orientation for new auditors.
  2. **Action Queue** — prioritized list of commits requiring action. Rows are non-compliant commits with no waiver (R1 exempt / R2 empty / R8 clean revert). Sorted by severity desc, then repo, then commit date desc. Columns: Priority, Severity (High/Medium/Low), Repo, SHA, PR #, Author, Merged By, Failing Rule, Prescribed Action, Days Since Commit, Resolution, Notes. Severity and action are synthesized by `SynthesizeAction` (`internal/report/rules.go`) from the primary failing rule.

  **Layer 2 — Overview (filterable totals)**
  3. **Summary** — per-repo rollup with `Total = Compliant + Non-Compliant`. Beyond the primary partition, columns cover waived (R1/R2/R8 + clean-merge), per-rule fire counts (R3 NoPR, R4 NoFinal, R6 OwnerFail), and informational tags (Self-Approved, Stale, Post-Merge, Clean Reverts, Clean Merges, Bots, Exempt, Empty, Multiple PRs). Compliance % is color-coded; a TOTAL row carries SUM/IF formulas.
  4. **By Rule** — triage pivot with one row per rule (R1..R8) showing fires, compliant vs non-compliant outcomes, waived, top repo, top author. Answers "which rule drags the fleet?".
  5. **By Author** — per-author rollup (Commits / Non-Compliant / Self-Approved / Stale / Post-Merge / Compliance %). Sorted by non-compliant desc. Coaching/pattern view.

  **Layer 3 — Trace & Evidence**
  6. **Decision Matrix** — one row per commit, one column per rule. Cells are `pass` / `fail` / `skip` / `n/a` / `missing` / `waived`, color-coded. Freezes first 3 columns (Repo / SHA / PR #) so rule columns scroll horizontally against fixed identity. Autofilter on any rule column produces per-rule drill-downs — replaces the old dedicated Self-Approved / Stale / Post-Merge / Clean Reverts / Clean Merges sheets.
  7. **Waivers Log** — one row per waiver tag (exempt-author / empty-commit / clean-revert / clean-merge / bot) with the evidence that led the tool to skip full evaluation. Required for defending the report: shows what the tool did NOT flag and why.
  8. **Multiple PRs** — one row per commit-PR pair for commits with `pr_count > 1`.

Rule outcomes in the Decision Matrix are derived by `DeriveRuleOutcomes` (`internal/report/rules.go`) from the stored `audit_results` booleans — no additional SQL runs. The derivation mirrors the decision order in `internal/sync/audit.go` (R1 → R2 → R3 → R4 → R5 → R6 → R7 → R8); any change to the audit logic must be reflected there.

## Package structure

```
cmd/
  root.go                    Cobra root + flag wiring
  sync.go                    `sync` — fetch + enrich + audit (the main loop)
  report.go                  `report` — table / csv / json / xlsx output
  config.go                  `config` — show effective config, list tokens
  reaudit.go                 `re-audit` — re-evaluate audit_results from DB (no API, single pass)
  backfill.go                `backfill-missing-prs` — recover PR attribution for "no associated pull request" rows via time-windowed merge_commit_sha lookup
  annotate_commits.go        `annotate-commits` — recompute informational annotations on every row from commit messages (no API)
internal/
  config/                    YAML config loading, validation, defaults
  db/
    db.go                    DuckDB open, connection wiring
    schema.go                Table DDL + migrations
    appender.go              Bulk Appender-API upsert helpers
    commits.go               Commit / co-author / commit-branch queries
    prs.go                   PR, review, check-run queries
    cursor.go                Sync-cursor read/write
    audit.go                 audit_results read/write
  github/
    client.go                REST API client (all endpoints)
    tokenpool.go             Multi-token management with rate limiting
    caching.go               CachingEnricher (in-memory + DB-first fallback, APIStats, fanout)
    revert.go                ParseRevert + IsCleanRevertDiff (clean-revert detection)
    merge.go                 ClassifyMerge (CleanMerge / DirtyMerge / OctopusMerge)
  model/
    types.go                 Domain types (Commit, PR, Review, CheckRun, AuditResult, EnrichmentResult)
  report/
    report.go                Summary/detail/by-author queries, table/csv/json formatting
    rules.go                 DeriveRuleOutcomes + SynthesizeAction — per-commit rule trace and action synthesis
    xlsx.go                  8-sheet XLSX generation (README, Action Queue, Summary, By Rule, By Author, Decision Matrix, Waivers Log, Multiple PRs)
  sync/
    pipeline.go              Orchestration (discover → fetch → enrich → audit)
    audit.go                 EvaluateCommit decision tree (rules 1–8)
    annotations.go           ComputeAnnotations — informational flags from commit messages
    dbwriter.go              Serialized write channel for DuckDB
    progress.go              Sync phase tracking
```

## Concurrency model

- **Repo sync**: `concurrency` goroutines via `errgroup` (default 32)
- **Enrichment**: `enrich_concurrency` batch goroutines per repo (default 16); each batch additionally fans out across commits (≤10) and PRs (≤5)
- **DB writes**: Single `DBWriter` goroutine per pipeline run — all writes serialized through a buffered channel
- **DB reads**: Safe to run concurrently (DuckDB MVCC)

## Rate limits

GitHub REST API: 5,000->15,000 requests/hour per token (PAT or App). Cost per commit: ~5 requests (detail + PRs list + PR detail + reviews + check runs). One token audits ~1,000 commits/hour. Multiple tokens multiply throughput linearly — the token pool routes requests to the least-loaded scoped token automatically. See [Token pool](#token-pool) for details.
