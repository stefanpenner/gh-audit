----------------------------- MODULE History -----------------------------
(***************************************************************************)
(* Soundness of history-rewrite (force-push) detection.                   *)
(*                                                                        *)
(* SLSA Source Track requires that a protected branch only ever advances  *)
(* to a DESCENDANT revision -- `git push --force` is prohibited. A        *)
(* malicious insider with push access can force-push to REWRITE history:  *)
(* orphan (hide) commits that were once on the branch -- laundering away  *)
(* unreviewed code or removing evidence -- and leave a clean-looking tree.*)
(*                                                                        *)
(* THE TRAP: an auditor that only inspects the CURRENT snapshot cannot    *)
(* see this. After the rewrite every commit it can reach traces to an     *)
(* approved PR, because the offending commits are simply gone. Detecting  *)
(* the rewrite requires the auditor's OWN memory of prior branch heads    *)
(* plus content-addressed ancestry (a forged parent changes the commit's  *)
(* own SHA, so reachability is non-forgeable).                            *)
(*                                                                        *)
(* We model a branch head moving over sync time. Each head is abstracted  *)
(* to the SET of commits reachable from it (its history) -- exactly what  *)
(* content-addressed ancestry yields, and non-forgeable. `observed` is    *)
(* the sequence of head-histories the auditor recorded across syncs.      *)
(*   Extend  fast-forward: new history is a SUPERSET of the last.         *)
(*   Rewrite force-push:   new history DROPS a previously-reachable commit.*)
(*                                                                        *)
(* Variant selects the detector:                                          *)
(*   "recorded"  every recorded head must remain reachable from the       *)
(*               current head (its history ⊆ the current history) -- the  *)
(*               sound rule (uses the retained prior heads).              *)
(*   "snapshot"  inspect only the current head -- the history-blind rule  *)
(*               every single-snapshot auditor uses. Sees no rewrite.     *)
(*                                                                        *)
(*   Sound == Compliant => nothing was laundered by a rewrite             *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets, TLC

CONSTANTS Commits, MaxHeads, Variant
ASSUME Variant \in {"recorded", "snapshot"}

VARIABLES
    observed,   \* Seq of SUBSET Commits: each recorded head's history
    rewrote     \* ground truth: a non-fast-forward move ever happened

vars == <<observed, rewrote>>

Init ==
    \* The branch starts at some initial head with an arbitrary history.
    /\ observed = << {} >>
    /\ rewrote = FALSE

Current == observed[Len(observed)]

\* Honest fast-forward: the new head's history is a superset of the current
\* -- every previously-reachable commit is still reachable.
Extend ==
    /\ Len(observed) < MaxHeads
    /\ \E h \in SUBSET Commits :
           /\ Current \subseteq h
           /\ observed' = Append(observed, h)
    /\ UNCHANGED rewrote

\* Force-push rewrite: the new head DROPS at least one commit that was
\* reachable before -- a non-fast-forward move.
Rewrite ==
    /\ Len(observed) < MaxHeads
    /\ \E h \in SUBSET Commits :
           /\ ~(Current \subseteq h)         \* something was orphaned
           /\ observed' = Append(observed, h)
    /\ rewrote' = TRUE

Next == Extend \/ Rewrite
Spec == Init /\ [][Next]_vars

--------------------------------------------------------------------------
Idx == DOMAIN observed

\* Auditor view. A commit is LAUNDERED if it appeared on some recorded head
\* but is not reachable from the current head -- hidden by a rewrite.
Laundered == UNION {observed[i] : i \in Idx} \ Current

\* "recorded": every recorded head must remain reachable from the current
\* head. Uses the auditor's retained memory of prior heads.
\* "snapshot": inspect only the current head -- nothing to compare against.
Intact ==
    IF Variant = "recorded"
    THEN \A i \in Idx : observed[i] \subseteq Current
    ELSE TRUE

Compliant == Intact

\* Ground truth: no commit was hidden by a rewrite.
TrulySafe == Laundered = {}

Sound == Compliant => TrulySafe

\* Bait: claims no compliant state is reachable. TLC must VIOLATE this and
\* print a witness -- proof Sound is not holding vacuously.
Bait == ~Compliant

TypeOK ==
    /\ observed \in Seq(SUBSET Commits)
    /\ Len(observed) \in 1..MaxHeads
    /\ rewrote \in BOOLEAN

==========================================================================
