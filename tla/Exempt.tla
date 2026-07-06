----------------------------- MODULE Exempt -----------------------------
(***************************************************************************)
(* Soundness of the S1 exempt-author waiver, under the signing policy.    *)
(*                                                                        *)
(* The operator curates an exempt list of ACCOUNT IDS (bot/service        *)
(* accounts allowed to ship without human review). A commit is waived     *)
(* when its author is on that list AND the PR carries no non-exempt human  *)
(* code (the squash carve-out, hasNonExemptPRContributors).               *)
(*                                                                        *)
(* THE TRAP this module now models (the 2026-07 gap-hunt headline): a      *)
(* commit's authorId is resolved by GitHub from the git-author EMAIL, and  *)
(* a noreply address `<id>+name@users.noreply.github.com` resolves to      *)
(* `<id>` even on an UNSIGNED commit. So an attacker can stamp the exempt  *)
(* account's id onto the author field of a commit they wrote. authorId is  *)
(* a HINT, not proof. The earlier spec asserted "AtkPush can never pick    *)
(* the exempt id" -- a false axiom that hid this attack.                   *)
(*                                                                        *)
(* The only identity a commit carries that a client cannot forge is a      *)
(* VERIFIED SIGNATURE, which binds the committer to the signing account.   *)
(* Modelled per commit:                                                    *)
(*   authorId     observable, FORGEABLE (client-set email -> id).          *)
(*   committerId  observable; trustworthy ONLY when verified (a signature  *)
(*                cryptographically binds it to the signer's account).     *)
(*   verified     observable BOOLEAN (verification.verified).              *)
(*   realSigner   ground truth: who actually produced/signed the commit.   *)
(*                An attacker cannot produce a verified signature as the    *)
(*                bot, so verified => committerId matches realSigner.       *)
(*                                                                        *)
(* Variant selects the audit rule (audit.go exemptStatus, signing_policy): *)
(*   "signer"    verified && committerId in exempt  -- required policy.    *)
(*   "optional"  signer OR authorId in exempt       -- DEFAULT policy;     *)
(*               the author path is forgeable (flagged trust:forgeable-    *)
(*               exemption) -- a documented tradeoff (amber).              *)
(*   "author"    authorId in exempt only            -- RETIRED author-id-  *)
(*               only rule the gap-hunt broke (red). The retired           *)
(*               verified_emails path is the same forgeable-identity class.*)
(*                                                                        *)
(*   Sound == Compliant => realSigner = Bot for all shipped (non-empty) code*)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS MaxCommits, Variant
ASSUME Variant \in {"signer", "optional", "author"}

Bot      == "bot"     \* exempt-list account
Atk      == "atk"     \* adversary
ExemptId == "botid"   \* the exempt account's immutable id
AtkId    == "atkid"
NoId     == "0"       \* GitHub could not bind the pusher's email

ExemptIds == {ExemptId}
AnyId     == {ExemptId, AtkId, NoId}

VARIABLES
    nextId,        \* commit-id allocator
    authorId,      \* cid -> AnyId          observable, FORGEABLE (email->id)
    committerId,   \* cid -> AnyId          observable; trusted only if verified
    verified,      \* cid -> BOOLEAN        observable (verification.verified)
    realSigner,    \* cid -> Bot | Atk      ground truth
    empty,         \* cid -> BOOLEAN        observable (GitHub diff stats)
    merged

vars == <<nextId, authorId, committerId, verified, realSigner, empty, merged>>

Init ==
    /\ nextId = 1
    /\ authorId = <<>>
    /\ committerId = <<>>
    /\ verified = <<>>
    /\ realSigner = <<>>
    /\ empty = <<>>
    /\ merged = FALSE

Add(aid, cid, v, rs, e) ==
    /\ ~merged
    /\ nextId <= MaxCommits
    /\ authorId' = authorId @@ (nextId :> aid)
    /\ committerId' = committerId @@ (nextId :> cid)
    /\ verified' = verified @@ (nextId :> v)
    /\ realSigner' = realSigner @@ (nextId :> rs)
    /\ empty' = empty @@ (nextId :> e)
    /\ nextId' = nextId + 1
    /\ UNCHANGED merged

\* Attacker push (realSigner = Atk). Two capabilities:
\*  - UNSIGNED (verified=FALSE): forge ANY authorId and ANY committerId,
\*    including the exempt id (noreply-email trick). This is the attack.
\*  - SIGNED with the attacker's OWN key (verified=TRUE): the signature
\*    binds committerId to the attacker (AtkId) -- NEVER the exempt id,
\*    which needs the bot's key. The author field is still forgeable
\*    (cherry-pick / --author), so authorId may still claim the exempt id.
AtkPush ==
    \/ /\ \E aid \in AnyId, cid \in AnyId, e \in {TRUE, FALSE} :
              Add(aid, cid, FALSE, Atk, e)
       /\ UNCHANGED merged
    \/ /\ \E aid \in AnyId, e \in {TRUE, FALSE} :
              Add(aid, AtkId, TRUE, Atk, e)
       /\ UNCHANGED merged

\* Honest bot push: it signs, so verified with committerId = exempt id.
\* The author id is the bot's too.
BotPush ==
    /\ \E e \in {TRUE, FALSE} : Add(ExemptId, ExemptId, TRUE, Bot, e)
    /\ UNCHANGED merged

Merge ==
    /\ ~merged
    /\ nextId > 1
    /\ merged' = TRUE
    /\ UNCHANGED <<nextId, authorId, committerId, verified, realSigner, empty>>

Next == AtkPush \/ BotPush \/ Merge

Spec == Init /\ [][Next]_vars

--------------------------------------------------------------------------
(* Auditor -- S1 over observable data. *)

\* Sound anchor: a verified signature binding the committer to an exempt id.
SignerExempt(c) == verified[c] /\ committerId[c] \in ExemptIds
\* Forgeable hint: the author field claims an exempt id.
AuthorExempt(c) == authorId[c] \in ExemptIds

Exempt(c) ==
    CASE Variant = "signer"   -> SignerExempt(c)
      [] Variant = "optional" -> SignerExempt(c) \/ AuthorExempt(c)
      [] Variant = "author"   -> AuthorExempt(c)

\* Non-exempt contributor voids the waiver, UNLESS the commit is verifiably
\* empty (empty commits ship no bytes -- the squash carve-out).
Contributes(c) == ~Exempt(c) /\ ~empty[c]

DomIds == 1..(nextId - 1)

\* The waiver: every non-empty commit in the merge is exempt.
Compliant == merged /\ (\A c \in DomIds : ~Contributes(c))

--------------------------------------------------------------------------
(* Ground truth: every non-empty shipped commit was really produced (and  *)
(* thus vouched for) by the bot -- realSigner = Bot. An attacker cannot    *)
(* forge a verified signature as the bot, so the verified-signer anchor is *)
(* the honest observable proxy for this.                                   *)
TrulySafe == merged /\ (\A c \in DomIds : empty[c] \/ realSigner[c] = Bot)

Sound == Compliant => TrulySafe

\* Bait: claims no compliant state is reachable. TLC must VIOLATE this
\* and print a witness trace -- proof Sound is not holding vacuously.
Bait == ~Compliant

TypeOK ==
    /\ nextId \in 1..(MaxCommits + 1)
    /\ merged \in BOOLEAN
    /\ DomIds \subseteq DOMAIN authorId

==========================================================================
