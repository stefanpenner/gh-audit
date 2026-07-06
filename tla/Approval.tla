---------------------------- MODULE Approval ----------------------------
(***************************************************************************)
(* Soundness of the S4/S5 approval verdict under an active adversary.     *)
(*                                                                        *)
(* The model plays a game on one pull request:                            *)
(*                                                                        *)
(*   - The ATTACKER is the PR author. They may push commits with any      *)
(*     parent (force-push), any committer timestamp (backdating), and     *)
(*     an author id GitHub either binds to them ("atk") or cannot bind    *)
(*     at all ("none", the author_id == 0 case). They may also submit     *)
(*     a review themselves (self-approval).                              *)
(*   - GITHUB / honest parties own the trusted actions: the exempt bot    *)
(*     pushes branch-sync commits, the human reviewer approves (the       *)
(*     review is bound to the branch head at that moment -- the client     *)
(*     cannot choose review.commit_id), and GitHub merges.                *)
(*                                                                        *)
(* Two views of every state:                                              *)
(*                                                                        *)
(*   GROUND TRUTH  realAuthor -- who really performed each push. The      *)
(*                 model knows this because it executed the action.       *)
(*   OBSERVABLE    parent, authorId, ctime, reviews, mergedHead -- what    *)
(*                 the GitHub API would return. This is all the auditor   *)
(*                 sees. ctime is forgeable; parent is content-addressed  *)
(*                 (fixed at creation, never mutated); authorId is        *)
(*                 GitHub-bound.                                          *)
(*                                                                        *)
(* Compliant below is Architecture.md S4+S5 restated over the             *)
(* observable view, including the stale-approval carve-out               *)
(* (isApprovalRefreshable in internal/sync/audit.go). The CarveOut        *)
(* constant selects the implementation:                                   *)
(*                                                                        *)
(*   "positional"  first-parent graph walk, fail closed -- the shipped     *)
(*                 rule (postApprovalByGraph).                            *)
(*   "timestamp"   committer-timestamp comparison -- the retired rule.     *)
(*                                                                        *)
(* The invariant:                                                         *)
(*                                                                        *)
(*   Sound == Compliant => TrulyAuthorized                                *)
(*                                                                        *)
(* TLC explores every interleaving of the actions. In "timestamp" mode    *)
(* it finds the backdated GIT_COMMITTER_DATE laundering attack; in        *)
(* "positional" mode no attack exists within the model bounds.            *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
    MaxCommits,   \* commits the adversary game may create (model bound)
    MaxTime,      \* clock ceiling (model bound)
    MaxReviews,   \* reviews the game may submit (model bound)
    CarveOut      \* "positional" | "timestamp"

ASSUME CarveOut \in {"positional", "timestamp"}

Atk  == "atk"    \* PR author, the adversary
Rev  == "rev"    \* independent human reviewer (honest)
Bot  == "bot"    \* exempt-list account (honest, e.g. branch-sync CI)
NoId == "none"   \* GitHub could not bind the pusher's email (author_id = 0)

ExemptIds == {Bot}   \* operator-curated exempt list -- account ids only

VARIABLES
    clock,       \* wall clock GitHub stamps trusted events with
    nextId,      \* next commit id to allocate (ids grow, parents point down)
    parent,      \* commit id -> first-parent id (0 = repo root)   observable, content-addressed
    authorId,    \* commit id -> Atk | Bot | NoId                  observable, GitHub-bound
    realAuthor,  \* commit id -> Atk | Bot                         GROUND TRUTH
    ctime,       \* commit id -> committer timestamp               observable, FORGEABLE
    head,        \* current PR branch head
    reviews,     \* set of [rev, cid, t] -- cid is GitHub-bound to head at submit time
    merged,      \* has GitHub merged the PR
    mergedHead   \* head at merge time (0 until merged)

vars == <<clock, nextId, parent, authorId, realAuthor, ctime,
          head, reviews, merged, mergedHead>>

--------------------------------------------------------------------------
(* Actions *)

Init ==
    /\ clock = 0
    /\ nextId = 2
    /\ parent = (1 :> 0)          \* the PR's initial commit
    /\ authorId = (1 :> Atk)
    /\ realAuthor = (1 :> Atk)
    /\ ctime = (1 :> 0)
    /\ head = 1
    /\ reviews = {}
    /\ merged = FALSE
    /\ mergedHead = 0

Tick ==
    /\ ~merged
    /\ clock < MaxTime
    /\ clock' = clock + 1
    /\ UNCHANGED <<nextId, parent, authorId, realAuthor, ctime,
                   head, reviews, merged, mergedHead>>

NewCommit(p, aid, ra, t) ==
    /\ nextId <= MaxCommits
    /\ parent' = parent @@ (nextId :> p)
    /\ authorId' = authorId @@ (nextId :> aid)
    /\ realAuthor' = realAuthor @@ (nextId :> ra)
    /\ ctime' = ctime @@ (nextId :> t)
    /\ head' = nextId
    /\ nextId' = nextId + 1

\* Adversary push. Parent is free (p # head models a force-push that
\* rewrites history), the committer timestamp is free (backdating and
\* future-dating), and GitHub binds the author id to the attacker or
\* not at all -- the attacker can never make it Bot's id.
AtkPush ==
    /\ ~merged
    /\ \E p \in 0..(nextId - 1), aid \in {Atk, NoId}, t \in 0..MaxTime :
           NewCommit(p, aid, Atk, t)
    /\ UNCHANGED <<clock, reviews, merged, mergedHead>>

\* Honest exempt-bot push (branch-sync merge): fast-forwards the head,
\* honest timestamp, id bound by GitHub.
BotPush ==
    /\ ~merged
    /\ NewCommit(head, Bot, Bot, clock)
    /\ UNCHANGED <<clock, reviews, merged, mergedHead>>

\* A review submission. GitHub binds cid to the head at this moment --
\* the one field the client cannot choose. The attacker reviewing their
\* own PR is allowed here; rejecting it is the auditor's job (S5).
Approve ==
    /\ ~merged
    /\ Cardinality(reviews) < MaxReviews
    /\ \E who \in {Rev, Atk} :
           reviews' = reviews \cup {[rev |-> who, cid |-> head, t |-> clock]}
    /\ UNCHANGED <<clock, nextId, parent, authorId, realAuthor, ctime,
                   head, merged, mergedHead>>

\* GitHub merges the PR at the current head.
Merge ==
    /\ ~merged
    /\ merged' = TRUE
    /\ mergedHead' = head
    /\ UNCHANGED <<clock, nextId, parent, authorId, realAuthor, ctime,
                   head, reviews>>

Next == Tick \/ AtkPush \/ BotPush \/ Approve \/ Merge

Spec == Init /\ [][Next]_vars

--------------------------------------------------------------------------
(* The auditor -- Architecture.md S4+S5 over the OBSERVABLE view only.   *)

\* First-parent chain from c down to the root. Parents always point at
\* lower ids, so this terminates.
RECURSIVE ChainSet(_)
ChainSet(c) == IF c = 0 THEN {} ELSE {c} \cup ChainSet(parent[c])

\* S5 self-approval: id-only -- the PR author's review never counts.
Independent(r) == r.rev # Atk

\* S4: an independent approval bound to the merged head.
ApprovalOnFinal ==
    \E r \in reviews : r.cid = mergedHead /\ Independent(r)

\* S4 carve-out, positional (postApprovalByGraph): walk first parents
\* from the merged head; the walk must REACH the approved commit (fail
\* closed on force-pushed history), and every commit strictly after the
\* approved snapshot must carry an exempt author id. NoId (author_id 0)
\* is never exempt (TrustedID).
PromotablePositional(r) ==
    /\ r.cid \in ChainSet(mergedHead)
    /\ \A c \in ChainSet(mergedHead) \ ChainSet(r.cid) :
           authorId[c] \in ExemptIds

\* S4 carve-out, RETIRED timestamp rule: "every commit committed after
\* the approval is exempt-authored", decided by the forgeable committer
\* timestamp. Kept as the red variant -- TLC finds the backdating attack.
PromotableTimestamp(r) ==
    \A c \in ChainSet(mergedHead) :
        ctime[c] > r.t => authorId[c] \in ExemptIds

Promotable(r) ==
    IF CarveOut = "positional"
    THEN PromotablePositional(r)
    ELSE PromotableTimestamp(r)

StalePromoted ==
    \E r \in reviews : r.cid # mergedHead /\ Independent(r) /\ Promotable(r)

\* The verdict the auditor emits from observable data alone.
Compliant == merged /\ (ApprovalOnFinal \/ StalePromoted)

--------------------------------------------------------------------------
(* Ground truth -- uses realAuthor, which no auditor can see.            *)

\* The merge really was authorized: some independent review exists such
\* that every merged commit the reviewer never saw (not an ancestor of
\* the snapshot they approved) was REALLY authored by the exempt bot.
TrulyAuthorized ==
    /\ merged
    /\ \E r \in reviews :
           /\ Independent(r)
           /\ \A c \in ChainSet(mergedHead) \ ChainSet(r.cid) :
                  realAuthor[c] = Bot

--------------------------------------------------------------------------
(* Invariants *)

\* No forgeable input may flip a verdict to compliant: if the auditor
\* says compliant, the merge was truly authorized.
Sound == Compliant => TrulyAuthorized

\* Bait: claims no compliant state is reachable. TLC must VIOLATE this
\* and print a witness trace — proof Sound is not holding vacuously.
Bait == ~Compliant

TypeOK ==
    /\ clock \in 0..MaxTime
    /\ nextId \in 2..(MaxCommits + 1)
    /\ head \in 1..(nextId - 1)
    /\ merged \in BOOLEAN
    /\ mergedHead \in 0..(nextId - 1)
    /\ \A r \in reviews :
           /\ r.rev \in {Rev, Atk}
           /\ r.cid \in 1..(nextId - 1)
           /\ r.t \in 0..MaxTime

==========================================================================
