----------------------------- MODULE Revert -----------------------------
(***************************************************************************)
(* Soundness of the S8 clean-revert waiver.                               *)
(*                                                                        *)
(* A commit that undoes an earlier commit may ship without review -- but  *)
(* only if it REALLY is a clean revert. The commit message is a forgeable *)
(* hint: anyone can write "Automatic revert of X..Y" or `Revert "..."` +  *)
(* "This reverts commit <sha>". The trustworthy fact is the DIFF: a clean  *)
(* revert's patch is the exact inverse of the reverted commit's patch      *)
(* (verifyRevertDiff -> RevertVerification = "diff-verified").            *)
(*                                                                        *)
(* Observable per revert commit:                                          *)
(*   claimsRevert  the message parses as a revert (forgeable)             *)
(*   diffVerified  GitHub-fetched patch is the exact inverse (not         *)
(*                 forgeable: a commit whose bytes invert X IS a revert    *)
(*                 of X, whoever wrote it)                                 *)
(* Ground truth:                                                          *)
(*   reallyReverts the commit's real effect is to undo prior shipped code  *)
(*                                                                        *)
(* Variant selects the waiver implementation:                             *)
(*   "diff"     waive only when diffVerified -- the shipped rule.         *)
(*   "message"  the retired AutoRevert leap: waive on the parsed message   *)
(*              alone. Kept as the red variant.                           *)
(*                                                                        *)
(*   Sound == Compliant => reallyReverts (no unreviewed NEW code shipped   *)
(*            under a revert waiver)                                       *)
(***************************************************************************)
EXTENDS Naturals, TLC

CONSTANTS Variant
ASSUME Variant \in {"diff", "message"}

VARIABLES
    pushed,        \* has a candidate commit been pushed
    claimsRevert,  \* observable: message parses as revert   FORGEABLE
    diffVerified,  \* observable: patch is exact inverse      not forgeable
    reallyReverts, \* ground truth: really undoes prior code
    merged

vars == <<pushed, claimsRevert, diffVerified, reallyReverts, merged>>

Init ==
    /\ pushed = FALSE
    /\ claimsRevert = FALSE
    /\ diffVerified = FALSE
    /\ reallyReverts = FALSE
    /\ merged = FALSE

\* One commit is pushed. The adversary chooses the message freely
\* (claimsRevert), but the two facts below are physically linked and NOT
\* under the adversary's control:
\*   diffVerified => reallyReverts   -- if the bytes are the exact inverse
\*                                      of X, the commit truly reverts X.
\*   reallyReverts => diffVerified   -- and if it truly reverts, its diff
\*                                      verifies. (A conflict-resolved GH-UI
\*                                      revert is NOT a pure inverse and is
\*                                      modelled as reallyReverts=FALSE with
\*                                      diffVerified=FALSE: reviewers must look.)
\* So diffVerified and reallyReverts move together; claimsRevert is free.
Push ==
    /\ ~pushed
    /\ \E cr \in {TRUE, FALSE}, real \in {TRUE, FALSE} :
           /\ pushed' = TRUE
           /\ claimsRevert' = cr
           /\ reallyReverts' = real
           /\ diffVerified' = real   \* physically tied to ground truth
    /\ UNCHANGED merged

Merge ==
    /\ pushed /\ ~merged
    /\ merged' = TRUE
    /\ UNCHANGED <<pushed, claimsRevert, diffVerified, reallyReverts>>

Next == Push \/ Merge
Spec == Init /\ [][Next]_vars

--------------------------------------------------------------------------
(* Auditor -- S8 over observable data.                                    *)

Waive ==
    IF Variant = "diff"
    THEN claimsRevert /\ diffVerified
    ELSE claimsRevert                 \* message-only leap

Compliant == merged /\ Waive

TrulySafe == merged /\ reallyReverts

Sound == Compliant => TrulySafe

TypeOK ==
    /\ pushed \in BOOLEAN /\ merged \in BOOLEAN
    /\ claimsRevert \in BOOLEAN /\ diffVerified \in BOOLEAN
    /\ reallyReverts \in BOOLEAN

==========================================================================
