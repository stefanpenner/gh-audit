----------------------------- MODULE Exempt -----------------------------
(***************************************************************************)
(* Soundness of the S1 exempt-author waiver.                              *)
(*                                                                        *)
(* The operator curates an exempt list of ACCOUNT IDS (bot/service        *)
(* accounts allowed to ship without human review). A commit is waived     *)
(* when its author is on that list AND the PR carries no non-exempt human  *)
(* code (the squash carve-out, hasNonExemptPRContributors).               *)
(*                                                                        *)
(* Three author identities per commit:                                    *)
(*   authorId     observable, GitHub-bound. Bot's real commits carry the  *)
(*                exempt id; the attacker's carry their own id or 0 (the   *)
(*                unbound-email case) -- NEVER the exempt id, which would  *)
(*                require compromising the bot account.                    *)
(*   authorEmail  observable, FORGEABLE. Anyone can set git author.email   *)
(*                to the bot's address.                                    *)
(*   realAuthor   ground truth.                                           *)
(*                                                                        *)
(* Variant selects the waiver implementation:                             *)
(*   "id"     id-only match against the exempt list -- the shipped rule   *)
(*            (isExemptCommit).                                           *)
(*   "email"  the RETIRED verified_emails path: waive when the git author *)
(*            email is a curated bot address. Config rejects this key at   *)
(*            load time now; kept here as the red variant.                *)
(*                                                                        *)
(*   Sound == Compliant => realAuthor = Bot for all shipped code          *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS MaxCommits, Variant
ASSUME Variant \in {"id", "email"}

Bot      == "bot"     \* exempt-list account
Atk      == "atk"     \* adversary
ExemptId == "botid"   \* the exempt account's immutable id
AtkId    == "atkid"
NoId     == "0"       \* GitHub could not bind the pusher's email
BotEmail == "bot@ci"  \* the bot's git author email -- forgeable string
AtkEmail == "atk@x"

ExemptIds == {ExemptId}

VARIABLES
    nextId,        \* commit-id allocator
    authorId,      \* cid -> ExemptId | AtkId | NoId        observable
    authorEmail,   \* cid -> BotEmail | AtkEmail            observable, forgeable
    realAuthor,    \* cid -> Bot | Atk                      ground truth
    empty,         \* cid -> BOOLEAN  (GitHub diff stats: no bytes)   observable
    merged

vars == <<nextId, authorId, authorEmail, realAuthor, empty, merged>>

Init ==
    /\ nextId = 1
    /\ authorId = <<>>
    /\ authorEmail = <<>>
    /\ realAuthor = <<>>
    /\ empty = <<>>
    /\ merged = FALSE

Add(aid, email, ra, e) ==
    /\ ~merged
    /\ nextId <= MaxCommits
    /\ authorId' = authorId @@ (nextId :> aid)
    /\ authorEmail' = authorEmail @@ (nextId :> email)
    /\ realAuthor' = realAuthor @@ (nextId :> ra)
    /\ empty' = empty @@ (nextId :> e)
    /\ nextId' = nextId + 1
    /\ UNCHANGED merged

\* Attacker push. Free email (may claim the bot's address), id bound to
\* the attacker or left unbound -- never the exempt id (that needs the
\* bot account). Diff-stat emptiness is GitHub's, so it is honest here.
AtkPush ==
    /\ \E aid \in {AtkId, NoId}, email \in {BotEmail, AtkEmail}, e \in {TRUE, FALSE} :
           Add(aid, email, Atk, e)
    /\ UNCHANGED merged

\* Honest bot push: exempt id, bot email, GitHub-set stats.
BotPush ==
    /\ \E e \in {TRUE, FALSE} : Add(ExemptId, BotEmail, Bot, e)
    /\ UNCHANGED merged

Merge ==
    /\ ~merged
    /\ nextId > 1
    /\ merged' = TRUE
    /\ UNCHANGED <<nextId, authorId, authorEmail, realAuthor, empty>>

Next == AtkPush \/ BotPush \/ Merge

Spec == Init /\ [][Next]_vars

--------------------------------------------------------------------------
(* Auditor -- S1 over observable data. A commit's author is exempt iff... *)

IdExempt(c)    == authorId[c] \in ExemptIds
EmailExempt(c) == authorEmail[c] = BotEmail

AuthorExempt(c) ==
    IF Variant = "id" THEN IdExempt(c) ELSE (IdExempt(c) \/ EmailExempt(c))

\* Non-exempt contributor voids the waiver, UNLESS the commit is verifiably
\* empty (empty commits ship no bytes -- the squash carve-out).
Contributes(c) == ~AuthorExempt(c) /\ ~empty[c]

DomIds == 1..(nextId - 1)

\* The waiver: every non-empty commit in the merge is exempt-authored.
Compliant == merged /\ (\A c \in DomIds : ~Contributes(c))

--------------------------------------------------------------------------
(* Ground truth: every non-empty shipped commit was really the bot.      *)
TrulySafe == merged /\ (\A c \in DomIds : empty[c] \/ realAuthor[c] = Bot)

Sound == Compliant => TrulySafe

TypeOK ==
    /\ nextId \in 1..(MaxCommits + 1)
    /\ merged \in BOOLEAN
    /\ DomIds \subseteq DOMAIN authorId

==========================================================================
