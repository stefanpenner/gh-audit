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

### GitHub App tokens

```yaml
tokens:
  - kind: app
    app_id: 12345
    installation_id: 67890
    private_key_path: /path/to/app.pem
    scopes:
      - org: my-org
```

Multiple tokens are load-balanced. The token pool tracks rate limits per token and picks the one with the most remaining quota.

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

The `--format xlsx` output produces a workbook with 5 sheets:

| Sheet | Purpose |
|---|---|
| **Summary** | Rollup by org/repo -- compliance counts and percentages |
| **All Commits** | Every commit with hyperlinked SHA and PR # |
| **Non-Compliant** | Failures only, with empty "Resolution" column for auditor notes |
| **Exemptions** | Bot and empty commits with exemption reasons |
| **Self-Approved** | Commits where the only approval came from a code contributor |

## Architecture

- **Go CLI** with [cobra](https://github.com/spf13/cobra)
- **DuckDB** for local storage and analytics
- **GitHub REST API** for all data fetching (commits, PRs, reviews, check runs)
- **Token pool** with rate-limit-aware selection across multiple PATs and App tokens

## Rate limits

All API calls use the GitHub REST API (5000 requests/hour per token). Typical cost per commit: 1 (detail) + 1 (PRs) + 1 (reviews) + 1 (check runs) = ~4 requests. One token can audit ~1200 commits/hour.

For large orgs, configure multiple tokens to multiply throughput.

## Global flags

```
--config    Config file path (default: ~/.config/gh-audit/config.yaml)
--db        DuckDB database path (default: ~/.local/share/gh-audit/audit.db)
--verbose   Enable debug logging
```
