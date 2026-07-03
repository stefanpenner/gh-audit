# TODO

## §7 delivering-PR refinement (base check is live; positional walk is the follow-up)

§7 is now **landing-scoped by default**: a PR's approval counts only when the PR
merged into an audited branch (`prDelivers`, `base_branch` glob-match). Config
`audit_rules.review_scope: content` opts out. See Architecture.md §7.

Possible refinement: the base-branch check credits *every* branch commit of a
PR that merged into an audited branch — including intermediate commits that a
later commit in the same PR overwrote. A stricter (positional) variant would
also confirm the commit lies on the first-parent path the PR actually
introduced onto the branch, using the parent-SHA graph already persisted for the
§4 carve-out (`postApprovalByGraph`). Low priority — the base check already
closes the cross-branch credit gap; this only tightens same-PR intermediate
commits, which were reviewed in that PR anyway.

## Revert-chain claim detection

Deferred work around revert chains that require cross-commit / cross-PR
analysis. Today each commit is evaluated standalone.

- **Prose-claim parsing.** Detect revert-chain claims in commit messages —
  `Original was: #N`, `Reverts the revert #N`, `Re-apply #N`, `Restores #N` —
  and attach them as informational annotations. Original implementation lived
  in `internal/sync/revert_chain.go` (removed).
- **Diff-verified re-apply auto-flip.** When a commit cites `Original was: #N`
  AND the current commit's +/- lines byte-match #N's merge-commit diff AND #N
  is compliant, flip the commit to compliant. The prose is untrusted, but
  diff equality is a strong provenance guard (cherry-pick / re-application,
  not drift).

## Tighten the clean-revert waiver

Today `IsCleanRevert=true` flips a commit to compliant standalone. A stricter
version would also require the reverted commit to itself be compliant:
reverting a non-compliant change shouldn't auto-launder the revert. Needs
cross-commit lookup (the old `PriorAuditLookup` plumbing, removed for now)
and probably a fixed-point re-audit pass so chains (`revert-of-revert`, …)
compose. See git history around this removal for the prior implementation.

## Revive the GH-UI revert waiver with content verification

An earlier "R2" waived conflict-resolved GH-Revert-button commits based on
provenance alone (committer==`web-flow` + verified signature). Dropped
because conflict resolution introduces unreviewed bytes onto master. A
safer revival would require *both* provenance and a bounded content check —
e.g., allow the waiver only if the non-inverse delta is small (< N lines)
and flagged for reviewer acknowledgement rather than full review.

## Revert-of-revert as a re-apply waiver

A `RevertOfRevert` that diff-verifies as a pure inverse of the revert it
undoes is — transitively — the exact bytes of the original reviewed PR. If
the original PR is compliant AND the diff-inverse check passes, the
revert-of-revert could auto-waive. Currently we don't even run the diff
check for this kind (`internal/github/caching.go` groups it with
`NotRevert`). This is the code-analogue of the prose "Original was: #N"
claim parsing in the section above.
