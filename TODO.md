# TODO

## AutoRevert waiver rests on a forgeable commit message

`§8` grants `IsCleanRevert=true` (→ compliant) to any commit whose message
matches `^Automatic revert of <sha>..<sha>` with **no** verification —
`revert.go` classifies `AutoRevert` from the message alone and `caching.go`
sets `RevertVerification = "message-only"` (the field name admits it). The
commit message is forgeable by anyone who can push, so an insider can land
unreviewed code directly on a protected branch under that prefix and have it
auto-waived. This is the one place a *compliance waiver* (not an
informational flag) rests on a forgeable node with no non-forgeable backstop
— see the "Trust model" section of `Architecture.md`.

Rationale for the current behaviour: "trust bot-generated auto-reverts." The
gap is that nothing proves the bot authored it. Two ways to close it (deferred
by decision, tracked here):

- **Gate on a trusted author/committer id.** Only waive when the commit's
  `AuthorID` matches an exempt/trusted account — ties the "trust the bot"
  intent to a non-forgeable id. Cheap (no extra API call).
- **Require diff verification.** Treat `AutoRevert` like `ManualRevert`:
  verify the diff is the exact inverse via `IsCleanRevertDiff`. Strongest,
  costs one `GetCommitFiles` per auto-revert.

Until then the risk is accepted as an operator decision and documented in the
trust model. Contrast `ManualRevert`, which is already diff-verified, and the
`§1` email path, which now requires an associated PR backstop.

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
