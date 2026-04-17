# gh-audit

Proof of Concept: GitHub commit compliance auditor. Verifies that every commit on protected branches was properly code-reviewed, approved on its final commit, and passed required status checks. Built for SOX/SOC2 audit evidence.

## What it checks

For every commit on configured branches:

1. **Has an associated merged PR** -- direct pushes are flagged
2. **Approved on the final commit** -- stale approvals (before force-push) don't count
3. **Not self-approved** -- reviewer must not be the PR author, commit author, committer, or co-author
4. **Required checks passed** -- e.g. "Owner Approval" check ran successfully on the PR's head commit
5. **Bot exemptions** -- configurable list of bot accounts that skip review requirements
6. **Empty commits** -- flagged but not counted as violations

## Install

```
go install github.com/stefanpenner/gh-audit@latest
```

## Quick start

No config file needed. If you have `gh` CLI authenticated, just run:

```bash
# Audit a specific repo
gh-audit sync --repo my-org/my-repo

# Audit an entire org
gh-audit sync --org my-org

# Specific date range
gh-audit sync --repo my-org/my-repo --since 2026-01-01 --until 2026-04-01

# Generate a report
gh-audit report --only-failures
```

Token is auto-detected from: `GH_TOKEN` env, `GITHUB_TOKEN` env, then `gh auth token`.

## How the audit trace works

For each commit on a branch, gh-audit follows a four-step REST API trace to collect the evidence needed for a compliance decision:

```
commit (SHA)
  |
  +-- GET /repos/{owner}/{repo}/commits/{sha}
  |     -> additions, deletions (empty commit detection)
  |
  +-- GET /repos/{owner}/{repo}/commits/{sha}/pulls
  |     -> associated merged PRs (number, author, head SHA, merge commit SHA)
  |
  +-- for each merged PR:
  |     |
  |     +-- GET /repos/{owner}/{repo}/pulls/{number}/reviews
  |     |     -> reviewer login, state (APPROVED/DISMISSED/...), commit ID
  |     |     -> only reviews on the PR's head SHA count (stale approval protection)
  |     |
  |     +-- GET /repos/{owner}/{repo}/commits/{head_sha}/check-runs
  |           -> check name, conclusion (success/failure/...)
  |           -> matched against configured required checks
  |
  +-- compliance decision:
        has_pr?  ->  has_approval_on_final_commit?  ->  required_checks_passed?
           |                    |                              |
           no: FAIL            no: FAIL                       no: FAIL
           yes: continue       yes: continue                  yes: COMPLIANT
```

Every REST endpoint is fully paginated. No data is silently truncated -- if a pagination boundary is hit, all pages are fetched.

### Per-reviewer state tracking

For SOX compliance, a DISMISSED review overrides an earlier APPROVED review from the same reviewer. gh-audit tracks the latest review state per reviewer on the PR's head commit:

```
reviewer A: APPROVED (10:00)  -> DISMISSED (11:00)  -> final state: DISMISSED
reviewer B: COMMENTED (09:00) -> APPROVED (10:30)   -> final state: APPROVED
```

### Self-approval detection

A review is considered self-approval if the reviewer matches any of:
- PR author
- Commit author
- Committer (excluding GitHub merge bots: "web-flow", "github")
- Co-authors (from `Co-authored-by` trailers)

If the only approvals are self-approvals, the commit is non-compliant.

## Config file (optional)

For advanced use (multi-token, audit rules, exemptions), create `~/.config/gh-audit/config.yaml`:

```yaml
orgs:
  - name: my-org
    exclude_repos: [deprecated-thing]

tokens:
  - kind: pat
    env: GH_AUDIT_TOKEN
    scopes:
      - org: my-org

audit_rules:
  required_checks:
    - name: "Owner Approval"
      conclusion: success

sync:
  concurrency: 10
  initial_lookback_days: 90

exemptions:
  authors:
    - "dependabot[bot]"
    - "renovate[bot]"
```

## Reports

```bash
# Terminal table
gh-audit report

# Only failures
gh-audit report --only-failures

# XLSX for auditors
gh-audit report --format xlsx --output compliance-q1-2026.xlsx

# JSON / CSV
gh-audit report --format json
gh-audit report --format csv --output audit.csv

# Filter by org, repo, or date range
gh-audit report --repo my-org/my-repo --since 2026-01-01 --until 2026-04-01
```

### XLSX output

The `--format xlsx` output produces a workbook with 7 sheets:

| Sheet | Purpose |
|---|---|
| **Summary** | Rollup by org/repo -- compliance counts and percentages |
| **All Commits** | Every commit with hyperlinked SHA and PR # |
| **Non-Compliant** | Failures only, with empty "Resolution" column for auditor notes |
| **Exemptions** | Bot and empty commits with exemption reasons |
| **Self-Approved** | Commits where the only approval came from a code contributor |
| **Stale Approvals** | Commits merged after approval became stale (force-push after review) |
| **Multiple PRs** | Commits associated with more than one PR (one row per commit-PR pair) |

## Architecture

- **Go CLI** with [cobra](https://github.com/spf13/cobra)
- **DuckDB** for local storage and analytics
- **GitHub REST API** for all data fetching (commits, PRs, reviews, check runs)
- **Token pool** with rate-limit-aware selection across multiple PATs and App tokens

## Authentication

gh-audit supports three token sources. You can mix PAT and App tokens in the same pool.

### Auto-detected token (zero config)

With no config file, gh-audit tries in order: `GH_TOKEN` env var, `GITHUB_TOKEN` env var, then `gh auth token` (GitHub CLI). The first one found is used as a wildcard token for all orgs/repos.

### Personal Access Tokens (PATs)

```yaml
tokens:
  - kind: pat
    env: GH_AUDIT_TOKEN          # env var holding the token
    scopes:
      - org: my-org              # restrict to this org
      - org: other-org
        repos: [repo-a, repo-b]  # restrict to specific repos
```

### GitHub App installation tokens

```yaml
tokens:
  - kind: app
    app_id: 12345
    installation_id: 67890
    private_key_path: /path/to/app.pem   # or private_key_env: APP_KEY
    scopes:
      - org: my-org
```

App tokens are generated at runtime via JWT exchange using the [ghinstallation](https://github.com/bradleyfalzon/ghinstallation) library. They auto-refresh before expiry.

### Required GitHub permissions

gh-audit calls read-only REST endpoints. The minimum permissions needed:

| Permission | Scope | Why |
|---|---|---|
| **Contents** | Read | `GET /repos/{o}/{r}/commits` — list and fetch commit details |
| **Pull requests** | Read | `GET /repos/{o}/{r}/commits/{sha}/pulls` — find associated PRs; `GET /repos/{o}/{r}/pulls/{n}/reviews` — fetch review states |
| **Checks** | Read | `GET /repos/{o}/{r}/commits/{ref}/check-runs` — verify required status checks |
| **Metadata** | Read | `GET /repos/{o}/{r}` and `GET /orgs/{org}/repos` — repo discovery and default branch detection |

For a **classic PAT**, the `repo` scope covers all of the above.

For a **fine-grained PAT**, enable: Contents (read), Pull requests (read), Checks (read), and Metadata (read).

For a **GitHub App**, set these repository permissions during app creation: Contents (read), Pull requests (read), Checks (read), and Metadata (read). Install the app on the target org/repos.

### Token pool and rate limits

Multiple tokens (PAT, App, or a mix) can be configured. The token pool:

- Tracks `x-ratelimit-remaining` and `x-ratelimit-reset` from every response
- Routes each request to the token with the most remaining quota that matches the target org/repo scope
- Blocks and waits for reset when all matching tokens are exhausted
- Retries on 429 (respecting `Retry-After` header)
- Disables tokens permanently on 401 (invalid credentials)

GitHub REST API: 5,000 requests/hour per token. Typical cost per commit: ~4 requests (detail + PRs + reviews + check runs). One token audits ~1,200 commits/hour. Multiple tokens multiply throughput linearly.

## Global flags

```
--config    Config file path (default: ~/.config/gh-audit/config.yaml)
--db        DuckDB database path (default: ~/.local/share/gh-audit/audit.db)
--verbose   Enable debug logging
```
