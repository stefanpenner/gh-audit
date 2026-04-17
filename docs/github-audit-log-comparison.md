# GitHub Audit Log vs gh-audit

GitHub provides an [organization audit log](https://docs.github.com/en/organizations/keeping-your-organization-secure/managing-security-settings-for-your-organization/reviewing-the-audit-log-for-your-organization) that tracks administrative actions across an org. This document explains how it relates to gh-audit and where each tool fits in a compliance program.

## Different questions, different tools

| | GitHub Audit Log | gh-audit |
|---|---|---|
| **Question answered** | "Who changed the rules?" | "Did everyone follow the rules?" |
| **Unit of analysis** | Administrative action (setting change, access grant, feature toggle) | Individual commit on a protected branch |
| **Examples** | Branch protection disabled, deploy key added, team membership changed, secret scanning toggled | Commit pushed without PR, PR merged with stale approval, self-approved change |
| **Retention** | 180 days | Unlimited (local DuckDB) |
| **Access** | Org owners only | Anyone with read access to the repos and a configured token |
| **Output** | JSON/CSV export of events | Auditor-ready XLSX with summary, violations, exemptions, and self-approvals |

## What the audit log tracks

The GitHub audit log records org-level operations:

- Repository lifecycle (creation, deletion, visibility changes)
- Branch protection and ruleset changes
- Team and membership management
- Authentication events (SSH keys, deploy keys, PATs)
- Security feature toggles (code scanning, secret scanning, Dependabot)
- GitHub Actions and workflow runs
- Billing and webhook changes

Each entry includes the actor, timestamp, action type, affected resource, and geographic location. Filtering uses qualifier syntax (`actor:username`, `action:team.create`, `created:>2026-01-01`) -- no full-text search.

## What the audit log does not do

- **Commit-level compliance** -- it has no concept of "was this commit reviewed before merge?"
- **Stale approval detection** -- it doesn't know that a force-push invalidated a previous review
- **Self-approval detection** -- it doesn't check whether the reviewer was also the author
- **Required check verification** -- it doesn't confirm that CI passed on the final PR head
- **Compliance reporting** -- no rollup by repo, no violation breakdowns, no auditor-oriented export

These are the core problems gh-audit solves.

## How they complement each other

The audit log watches the guardrails; gh-audit watches the code flowing through them.

Consider this scenario: an admin temporarily disables branch protection at 2am, pushes a commit directly, then re-enables protection. The audit log records the `protected_branch.destroy` and `protected_branch.create` events. gh-audit catches the commit that slipped through without a PR or review.

Neither tool alone gives the full picture. Together they cover both the control plane (rules and access) and the data plane (actual code changes).

## Future integration

gh-audit's roadmap includes using `protected_branch.policy_override` events from the audit log to detect admin bypasses and annotate affected commits in compliance reports. This would surface not just "this commit had no review" but "this commit had no review because an admin overrode branch protection."
