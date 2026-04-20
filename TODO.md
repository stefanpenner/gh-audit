# TODO

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

## Tighten clean-revert rule (R1)

Today `IsCleanRevert=true` flips a commit to compliant standalone. A stricter
version would also require the reverted commit to itself be compliant:
reverting a non-compliant change shouldn't auto-launder the revert. Needs
cross-commit lookup (the old `PriorAuditLookup` plumbing, removed for now)
and probably a fixed-point re-audit pass so chains (`revert-of-revert`, …)
compose. See git history around this removal for the prior implementation.
