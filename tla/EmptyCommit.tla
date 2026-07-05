--------------------------- MODULE EmptyCommit --------------------------
(***************************************************************************)
(* Soundness of the S2 empty-commit waiver.                               *)
(*                                                                        *)
(* A commit that changed nothing may ship without review. The subtlety:   *)
(* "nothing" must mean zero LINES and zero FILES. A pure rename or a       *)
(* mode-only change reports 0 added / 0 deleted LINES but touches files    *)
(* -- content moved that a reviewer never saw.                            *)
(*                                                                        *)
(* Observable (GitHub diff stats): addLines, delLines, filesChanged.      *)
(* Ground truth: reallyEmpty -- the commit truly introduced no reviewable  *)
(* change.                                                                 *)
(*                                                                        *)
(* Variant selects the waiver implementation:                             *)
(*   "linesfiles"  waive only when lines AND files are zero -- shipped.   *)
(*   "lines"       the naive rule: waive on zero lines alone. Kept as the  *)
(*                 red variant; a rename-only commit launders through it.  *)
(*                                                                        *)
(*   Sound == Compliant => reallyEmpty                                    *)
(***************************************************************************)
EXTENDS Naturals, TLC

CONSTANTS Variant, MaxCount
ASSUME Variant \in {"linesfiles", "lines"}

VARIABLES pushed, addLines, delLines, filesChanged, reallyEmpty, merged
vars == <<pushed, addLines, delLines, filesChanged, reallyEmpty, merged>>

Init ==
    /\ pushed = FALSE
    /\ addLines = 0 /\ delLines = 0 /\ filesChanged = 0
    /\ reallyEmpty = FALSE
    /\ merged = FALSE

\* One commit. The diff stats are GitHub's, not the adversary's; the point
\* is that a rename shows up as (0 lines, >=1 file). We enumerate the honest
\* stat/ground-truth combinations:
\*   truly empty:           0 lines, 0 files, reallyEmpty
\*   rename / mode-only:    0 lines, >=1 file, NOT reallyEmpty
\*   ordinary change:       >=1 line, reallyEmpty FALSE
Push ==
    /\ ~pushed
    /\ \E al \in 0..MaxCount, dl \in 0..MaxCount, fc \in 0..MaxCount :
           /\ pushed' = TRUE
           /\ addLines' = al /\ delLines' = dl /\ filesChanged' = fc
           /\ reallyEmpty' = (al = 0 /\ dl = 0 /\ fc = 0)
    /\ UNCHANGED merged

Merge ==
    /\ pushed /\ ~merged
    /\ merged' = TRUE
    /\ UNCHANGED <<pushed, addLines, delLines, filesChanged, reallyEmpty>>

Next == Push \/ Merge
Spec == Init /\ [][Next]_vars

--------------------------------------------------------------------------
Waive ==
    IF Variant = "linesfiles"
    THEN addLines = 0 /\ delLines = 0 /\ filesChanged = 0
    ELSE addLines = 0 /\ delLines = 0        \* ignores file count

Compliant == merged /\ Waive
TrulySafe == merged /\ reallyEmpty
Sound == Compliant => TrulySafe

TypeOK ==
    /\ pushed \in BOOLEAN /\ merged \in BOOLEAN /\ reallyEmpty \in BOOLEAN
    /\ addLines \in 0..MaxCount /\ delLines \in 0..MaxCount
    /\ filesChanged \in 0..MaxCount

==========================================================================
