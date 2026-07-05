----------------------------- MODULE Checks -----------------------------
(***************************************************************************)
(* Faithfulness of the S6 required-status-check rule.                     *)
(*                                                                        *)
(* A required check can have MANY runs on one head SHA: re-runs mint new   *)
(* check_run ids and the DB accumulates them across syncs. GitHub's own    *)
(* UI semantics are "the LATEST run wins" -- an old green run followed by   *)
(* a red re-run means the check is red on the current code.               *)
(*                                                                        *)
(* Observable: the set of runs, each [t, concl]. concl is GitHub/CI-set.  *)
(* Ground truth we must faithfully compute: does the required check pass   *)
(* on the merged code? = the latest run concluded success.                *)
(*                                                                        *)
(* Variant selects the implementation (evaluateRequiredChecks):           *)
(*   "latest"  success iff the latest run per check is success -- shipped. *)
(*   "any"     the naive rule: success iff ANY run passed. An earlier      *)
(*             green run masks a later red one. Kept as the red variant.   *)
(*                                                                        *)
(*   Sound == Compliant => check truly passes on the merged code          *)
(***************************************************************************)
EXTENDS Naturals, Sequences, TLC

CONSTANTS Variant, MaxRuns, MaxTime
ASSUME Variant \in {"latest", "any"}

Concls == {"success", "failure", "none"}   \* none = queued/in_progress

VARIABLES runs, merged     \* runs: Seq of [t |-> Nat, concl |-> Concls]
vars == <<runs, merged>>

Init == runs = <<>> /\ merged = FALSE

AddRun ==
    /\ ~merged
    /\ Len(runs) < MaxRuns
    /\ \E t \in 0..MaxTime, c \in Concls :
           runs' = Append(runs, [t |-> t, concl |-> c])
    /\ UNCHANGED merged

Merge ==
    /\ ~merged
    /\ merged' = TRUE
    /\ UNCHANGED runs

Next == AddRun \/ Merge
Spec == Init /\ [][Next]_vars

--------------------------------------------------------------------------
Idx == DOMAIN runs

\* Latest run: max completedAt, ties broken by later index (higher run id).
IsLatest(i) ==
    \A j \in Idx : \/ runs[j].t < runs[i].t
                   \/ (runs[j].t = runs[i].t /\ j <= i)

LatestConcl == IF Idx = {} THEN "none"
               ELSE runs[CHOOSE i \in Idx : IsLatest(i)].concl

CheckPasses ==
    IF Variant = "latest"
    THEN Idx # {} /\ LatestConcl = "success"
    ELSE \E i \in Idx : runs[i].concl = "success"    \* any-run leak

Compliant == merged /\ CheckPasses

\* Ground truth: the check passes on the merged code iff its latest run
\* concluded success -- GitHub's own latest-wins semantics.
TrulySafe == merged /\ Idx # {} /\ LatestConcl = "success"

Sound == Compliant => TrulySafe

TypeOK ==
    /\ merged \in BOOLEAN
    /\ Len(runs) \in 0..MaxRuns
    /\ \A i \in Idx : runs[i].t \in 0..MaxTime /\ runs[i].concl \in Concls

==========================================================================
