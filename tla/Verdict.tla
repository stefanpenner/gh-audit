----------------------------- MODULE Verdict ----------------------------
(***************************************************************************)
(* Soundness of the S7 cross-PR landing-scope verdict.                    *)
(*                                                                        *)
(* A commit on the audited branch (say `main`) may be associated with     *)
(* several PRs. gh-audit credits a PR's independent approval to the        *)
(* commit. The scope question: does an approval on a PR that merged into   *)
(* a SIBLING branch (gitflow feat -> dev) vouch for the landing on main?   *)
(*                                                                        *)
(* The review is genuine (this module is not about forged approvals --     *)
(* Approval.tla covers those). The gap is positional: the feat->dev review *)
(* covered the code, but nobody independently reviewed its promotion onto  *)
(* main.                                                                   *)
(*                                                                        *)
(* Observable per PR: approved (independent approval on final) and         *)
(* observedBase (pr.base.ref -- GitHub-set, may be missing on legacy rows).*)
(* Ground truth per PR: realBase in {Audited, Sibling}.                    *)
(*                                                                        *)
(* Variant selects the verdict scope (prDelivers):                        *)
(*   "landing"   credit iff approved AND observedBase = Audited (fail      *)
(*               CLOSED on unknown) -- the soundness-strict rule.          *)
(*   "failopen"  the SHIPPED rule: also credit an approved PR with a       *)
(*               MISSING base (prDelivers returns true on empty base, a    *)
(*               deliberate availability tradeoff). This config documents   *)
(*               the residual assumption: an unknown base must never be     *)
(*               attacker-suppressible. Expect a violation reachable ONLY   *)
(*               through the unknown-base state.                           *)
(*   "content"   the legacy rule: credit any approved PR regardless of      *)
(*               base. The sibling-branch credit gap. Red variant.         *)
(*                                                                        *)
(*   Sound == Compliant => some approved PR really merged into an audited  *)
(*            branch                                                        *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS NPRs, Variant
ASSUME Variant \in {"landing", "failopen", "content"}

Audited == "main"
Sibling == "dev"
Unknown == "?"

PRs == 1..NPRs

VARIABLES
    approved,      \* PR -> BOOLEAN   observable (genuine approval)
    realBase,      \* PR -> {Audited, Sibling}   ground truth
    observedBase,  \* PR -> {Audited, Sibling, Unknown}   observable
    setup,         \* PRs configured
    merged

vars == <<approved, realBase, observedBase, setup, merged>>

Init ==
    /\ approved = [p \in PRs |-> FALSE]
    /\ realBase = [p \in PRs |-> Sibling]
    /\ observedBase = [p \in PRs |-> Unknown]
    /\ setup = FALSE
    /\ merged = FALSE

\* Configure all PRs at once (their approval and base facts). observedBase
\* is realBase or Unknown -- GitHub records the base, but a legacy/missing
\* row can leave it blank. It can never show Audited when the real base is
\* Sibling: base.ref is GitHub-set, not forgeable.
Setup ==
    /\ ~setup
    /\ \E ap \in [PRs -> BOOLEAN],
          rb \in [PRs -> {Audited, Sibling}],
          ob \in [PRs -> {Audited, Sibling, Unknown}] :
           /\ \A p \in PRs : ob[p] = rb[p] \/ ob[p] = Unknown
           /\ approved' = ap
           /\ realBase' = rb
           /\ observedBase' = ob
    /\ setup' = TRUE
    /\ UNCHANGED merged

Merge ==
    /\ setup /\ ~merged
    /\ merged' = TRUE
    /\ UNCHANGED <<approved, realBase, observedBase, setup>>

Next == Setup \/ Merge
Spec == Init /\ [][Next]_vars

--------------------------------------------------------------------------
Credits(p) ==
    CASE Variant = "landing"  -> approved[p] /\ observedBase[p] = Audited
      [] Variant = "failopen" -> approved[p] /\ observedBase[p] \in {Audited, Unknown}
      [] Variant = "content"  -> approved[p]

Compliant == merged /\ (\E p \in PRs : Credits(p))

\* Ground truth: some approved PR really delivered to the audited branch.
TrulyAuthorized == merged /\ (\E p \in PRs : approved[p] /\ realBase[p] = Audited)

Sound == Compliant => TrulyAuthorized

TypeOK ==
    /\ setup \in BOOLEAN /\ merged \in BOOLEAN
    /\ approved \in [PRs -> BOOLEAN]
    /\ realBase \in [PRs -> {Audited, Sibling}]
    /\ observedBase \in [PRs -> {Audited, Sibling, Unknown}]

==========================================================================
