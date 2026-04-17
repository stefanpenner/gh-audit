# gh-audit Test Scenarios

Test scenarios for compliance auditing. Each scenario describes a commit
state, expected audit result, and how to reproduce it in a GitHub fixture
repo (`stefanpenner/gh-audit-test-fixtures`).

---

## 1. Compliant Scenarios

### 1.1 Normal merge commit

- **What it tests**: Standard PR workflow with review and merge.
- **Expected**: Compliant.
- **Fixture**: Create branch, push commit, open PR, get approval from
  another user on the head commit, merge via merge commit.

### 1.2 Squash merge

- **What it tests**: PR merged via squash-and-merge. The merge commit SHA
  differs from the head SHA, but `MergeCommitSHA` maps back to the commit.
- **Expected**: Compliant.
- **Fixture**: Same as 1.1 but merge using "Squash and merge".

### 1.3 Rebase merge

- **What it tests**: PR merged via rebase. The final commit on the base
  branch is the rebased version of the head commit.
- **Expected**: Compliant.
- **Fixture**: Same as 1.1 but merge using "Rebase and merge".

### 1.4 Multiple reviewers all approved on final commit

- **What it tests**: More than one independent reviewer approved.
- **Expected**: Compliant.
- **Fixture**: Two non-author users each submit APPROVED reviews on the
  PR's head SHA.

### 1.5 Bot commit (exempt author)

- **What it tests**: Commit authored by an exempt bot (e.g.
  `dependabot[bot]`) bypasses all review requirements.
- **Expected**: Compliant, `IsBot=true`, `IsExemptAuthor=true`.
- **Fixture**: Let Dependabot create and merge a PR, or push a commit
  attributed to `dependabot[bot]`. Configure the bot in `exemptAuthors`.

### 1.6 Non-bot exempt author

- **What it tests**: A service account in the exempt list that does not
  have the `[bot]` suffix.
- **Expected**: Compliant, `IsBot=false`, `IsExemptAuthor=true`.
- **Fixture**: Push a commit as the service account login. Configure it
  in `exemptAuthors`.

### 1.7 Empty commit

- **What it tests**: A commit with zero additions and zero deletions.
- **Expected**: Compliant, `IsEmptyCommit=true`.
- **Fixture**: `git commit --allow-empty -m "empty"` and push directly.

### 1.8 No required checks configured

- **What it tests**: When `requiredChecks` is nil/empty, Owner Approval
  check is not enforced.
- **Expected**: Compliant (assuming review exists on final commit).
- **Fixture**: Standard PR with approval, no Owner Approval check run
  needed.

### 1.9 CHANGES_REQUESTED from one reviewer, APPROVED from another on final

- **What it tests**: A single approval on the final commit is sufficient
  even when another reviewer requested changes.
- **Expected**: Compliant.
- **Fixture**: reviewer1 submits CHANGES_REQUESTED on head SHA, reviewer2
  submits APPROVED on head SHA.

### 1.10 Self-approval exists but independent approval also exists

- **What it tests**: Self-approval is ignored when a legitimate
  independent approval is present.
- **Expected**: Compliant, `IsSelfApproved=false`.
- **Fixture**: PR author approves their own PR, then an independent
  reviewer also approves on the same head SHA.

### 1.11 Stale approval from one reviewer, fresh approval from another

- **What it tests**: One reviewer approved an old (force-pushed) commit.
  A second reviewer approved the final head SHA.
- **Expected**: Compliant (fresh approval is sufficient).
- **Fixture**: reviewer1 approves, author force-pushes, reviewer2 approves
  the new head SHA.

### 1.12 Re-approval after force-push (same reviewer)

- **What it tests**: Reviewer approved old commit, author force-pushes,
  same reviewer re-approves the new head SHA.
- **Expected**: Compliant.
- **Fixture**: reviewer1 approves, author force-pushes, reviewer1 approves
  again on new head SHA.

### 1.13 Commit in multiple PRs, one compliant

- **What it tests**: A commit associated with two PRs. One PR has no
  reviews, the other has a valid approval.
- **Expected**: Compliant (best PR wins).
- **Fixture**: Cherry-pick the same commit into two branches and open
  separate PRs. Approve only one.

### 1.14 Merge commit treated normally

- **What it tests**: A merge commit (ParentCount=2) goes through the
  same compliance checks as a regular commit.
- **Expected**: Compliant if the associated PR has approval.
- **Fixture**: Standard merge-commit PR.

---

## 2. Non-Compliant Scenarios

### 2.1 Direct push (no PR)

- **What it tests**: Commit pushed directly to the default branch with no
  associated pull request.
- **Expected**: Non-compliant, `HasPR=false`,
  reason: `no associated pull request`.
- **Fixture**: Push a commit directly to `main` without a PR.

### 2.2 PR exists but no reviews

- **What it tests**: PR was merged without any reviews.
- **Expected**: Non-compliant,
  reason: `no approval on final commit (PR #N)`.
- **Fixture**: Open PR, merge immediately without requesting review.

### 2.3 Stale approval (force-push)

- **What it tests**: Approval was given on a commit that was later
  force-pushed away. No approval exists on the final head SHA.
- **Expected**: Non-compliant,
  reason: `no approval on final commit (PR #N)`.
- **Fixture**: reviewer1 approves, author force-pushes new commits, merge
  without re-approval.

### 2.4 Self-approval: PR author is reviewer

- **What it tests**: The PR author approves their own PR with no other
  approvals.
- **Expected**: Non-compliant, `IsSelfApproved=true`,
  reason: `self-approved (reviewer is code author) (PR #N)`.
- **Fixture**: PR author submits an APPROVED review on their own PR.

### 2.5 Self-approval: commit author is reviewer

- **What it tests**: The git commit author (different from the PR author)
  reviews and approves.
- **Expected**: Non-compliant, `IsSelfApproved=true`.
- **Fixture**: User A opens PR, User B pushes a commit, User B approves.

### 2.6 Self-approval: co-author is reviewer

- **What it tests**: A co-author listed in "Co-authored-by" trailers
  submits the approval.
- **Expected**: Non-compliant, `IsSelfApproved=true`.
- **Fixture**: Include `Co-authored-by: codev <codev@example.com>` in the
  commit message, have `codev` approve.

### 2.7 Self-approval: committer is reviewer

- **What it tests**: The committer (non-bot) is the same person who
  approved.
- **Expected**: Non-compliant, `IsSelfApproved=true`.
- **Fixture**: Have a non-bot committer login that matches the reviewer.

### 2.8 Missing required check (Owner Approval)

- **What it tests**: PR has approval but the required check run is absent.
- **Expected**: Non-compliant,
  reason: `Owner Approval check missing/failed (PR #N)`.
- **Fixture**: Approve PR but do not run the Owner Approval check.

### 2.9 Failed required check (Owner Approval)

- **What it tests**: Owner Approval check ran but concluded with failure.
- **Expected**: Non-compliant,
  reason: `Owner Approval check missing/failed (PR #N)`.
- **Fixture**: Approve PR, have Owner Approval check report `failure`.

### 2.10 Bot not in exempt list

- **What it tests**: A bot author (login ending in `[bot]`) that is NOT
  in the configured `exemptAuthors` list.
- **Expected**: Non-compliant, `IsBot=true`, `IsExemptAuthor=false`.
- **Fixture**: Push a commit as `some-ci[bot]`, do not add it to
  `exemptAuthors`.

### 2.11 Same reviewer: old APPROVED then CHANGES_REQUESTED on final

- **What it tests**: Reviewer approved an old commit, then submitted
  CHANGES_REQUESTED on the final head SHA. No APPROVED review exists on
  the final commit.
- **Expected**: Non-compliant,
  reason: `no approval on final commit (PR #N)`.
- **Fixture**: reviewer1 approves old SHA, author pushes new commits,
  reviewer1 requests changes on head SHA.

### 2.12 Multiple PRs, all non-compliant

- **What it tests**: Commit is associated with multiple PRs, none of
  which have valid approval or passing checks.
- **Expected**: Non-compliant.
- **Fixture**: Cherry-pick commit into two PRs, do not approve either.

---

## 3. Edge Cases

### 3.1 DISMISSED review on final commit

- **What it tests**: Only review on the final commit has state DISMISSED.
- **Expected**: Non-compliant (DISMISSED is not APPROVED).
- **Fixture**: Approve PR, then have an admin dismiss the review before
  merging.

### 3.2 APPROVED then DISMISSED by same reviewer on final (known gap)

- **What it tests**: Reviewer approved, then the review was dismissed.
  Both review records reference the head SHA.
- **Expected (current behavior)**: Compliant -- the engine sees the
  APPROVED review and does not track that a later DISMISSED review
  supersedes it. **This is a known gap.** If per-reviewer last-state
  tracking is added, this should become non-compliant.
- **Fixture**: reviewer1 approves on head SHA, admin dismisses reviewer1's
  review.

### 3.3 COMMENTED review only

- **What it tests**: The only review is a COMMENTED review (not an
  approval).
- **Expected**: Non-compliant (COMMENTED is not APPROVED).
- **Fixture**: Reviewer leaves a comment-only review.

### 3.4 Review on a different PR number

- **What it tests**: A review exists but its `PRNumber` does not match any
  associated PR. Should be ignored.
- **Expected**: Non-compliant (no matching review for the PR).
- **Fixture**: Unlikely in practice, but tests filter logic.

### 3.5 Case-insensitive login matching for self-approval

- **What it tests**: Reviewer login differs in case from commit author
  (e.g. `Developer` vs `developer`). Self-approval detection uses
  `strings.EqualFold`.
- **Expected**: Detected as self-approval.
- **Fixture**: Author login `developer`, reviewer login `Developer`.

### 3.6 Case-insensitive login matching for exempt authors

- **What it tests**: Exempt author list has `Dependabot[bot]` but commit
  author is `dependabot[bot]`.
- **Expected**: Exempt (case-insensitive match).
- **Fixture**: Configure exempt author with different casing than the
  actual commit author.

### 3.7 Committer is web-flow (GitHub merge bot)

- **What it tests**: `web-flow` as committer is excluded from
  self-approval checks. A reviewer named `web-flow` is not treated as
  a self-approver.
- **Expected**: Compliant (web-flow is not a code contributor).
- **Fixture**: Standard GitHub merge where committer is `web-flow`.

### 3.8 Committer is github (GitHub merge bot)

- **What it tests**: `github` as committer login is also excluded from
  self-approval checks, same as `web-flow`.
- **Expected**: Compliant.
- **Fixture**: Merge via GitHub where committer is `github`.

### 3.9 Empty reviewer login

- **What it tests**: A review with an empty `ReviewerLogin` should not
  trigger self-approval detection.
- **Expected**: Not treated as self-approval.
- **Fixture**: Edge case in API response; unlikely in practice.

### 3.10 Commit with additions but zero deletions

- **What it tests**: A commit with only additions (deletions=0) is NOT
  empty.
- **Expected**: Non-compliant if no PR.
- **Fixture**: Add a new file, push directly.

### 3.11 Multiple required checks (partial pass)

- **What it tests**: Two required checks configured, one passes, one
  fails.
- **Expected**: Non-compliant (all required checks must pass).
- **Fixture**: Configure two required checks, only provide one successful
  check run.

### 3.12 Owner Approval check on wrong commit SHA

- **What it tests**: Owner Approval check ran on a different SHA than the
  PR's head SHA.
- **Expected**: Non-compliant (check not found for the head SHA).
- **Fixture**: Run Owner Approval on an old commit, not the head SHA.

---

## 4. Multi-Branch Scenarios

### 4.1 Commit on non-default branch

- **What it tests**: Commit is on a release branch, not the default
  branch. Compliance rules still apply.
- **Expected**: Same compliance logic regardless of branch.
- **Fixture**: Create a release branch, push commits, open PR against
  that branch.

### 4.2 Same commit on multiple branches

- **What it tests**: A commit cherry-picked to both `main` and a release
  branch. Each branch context is audited independently.
- **Expected**: Compliance depends on the PRs/reviews found per branch
  context.
- **Fixture**: Cherry-pick a commit to two branches, open PRs on each.

### 4.3 Commit on default branch via fast-forward from feature branch

- **What it tests**: Feature branch is fast-forwarded into main (no merge
  commit). The original commit SHA is now on the default branch.
- **Expected**: Compliant if PR with approval exists for that SHA.
- **Fixture**: Merge feature branch via fast-forward (rebase merge).

---

## Fixture Repository Setup

The test fixture repo (`stefanpenner/gh-audit-test-fixtures`) should:

1. Have branch protection on `main` disabled (to allow direct pushes for
   non-compliant scenarios).
2. Include at least two GitHub users: one for authoring, one for reviewing.
3. Have a GitHub Actions workflow that creates an "Owner Approval" check
   run (can be a trivial workflow that always passes).
4. Tag each scenario commit with a descriptive tag (e.g.
   `scenario/2.1-direct-push`) for easy reference.
5. Include a `scenarios.json` mapping scenario IDs to commit SHAs for
   automated test validation.
