State-transition table v2, reflecting the five tightening points from review. Scope unchanged: experimental reference implementation; delegation-state only; deterministic fake services; device remains action authority; the envelope is an optional contract testbed — direct model access (device Agent + optional edge/cloud model services) is the primary scenario and does not depend on this.

## Record, identity, and attempt reservations

A subtask record holds: `id` (root-allocated), `root_task_id`, `parent_id`, `task_epoch`, `scope`, `bounds` (TTL, max attempts), `state`, `result`, `content_hash`, and:

- **`effect: none | partial | unknown`** — what physically happened, as attested by the executor. Outcome (completed/failed) and effect are different statements: an executor may report failure after a partial effect occurred. (The field value `unknown` describes un-attested effect on any record; the `UnknownEffect` *state* is the record-level uncertainty state — same word, different scope.)
- **An allowance ledger** `{reserved, consumed, released_unused, locked}` — see the accounting model below; outcome evidence and usage evidence are separate dimensions.
- **Attempt reservations** `{attempt_id, reserved_at}` — persisted *before* any external call (see the commit boundary). Provenance for every evidence-driven resolution.

The record is persisted at submission; the attempt reservation is persisted before dispatch. Both writes go through the serialized commit boundary.

## States

| State | Meaning | Terminal? |
|---|---|---|
| `Proposed` | Envelope recorded; admission checks not finished | no |
| `Accepted` | Admission passed; allowance reserved; work not started | no |
| `InProgress` | An attempt reservation was persisted and the external call may be in flight | no |
| `Canceling` | Cancel requested or parent epoch advanced; stop not yet attested | no |
| `Completed` | Result recorded | yes |
| `Failed` | Failure recorded. Says nothing by itself about effect: `effect` is recorded separately (`none`/`partial`/`unknown`) | yes |
| `Canceled` | Stop attested. Combined with `effect`: attested stop with `none` is "nothing happened"; attested stop with `partial` is a recorded partial effect, not a clean outcome | yes |
| `Expired` | TTL elapsed **before any attempt reservation existed** — usage known-zero, effect `none` | yes |
| `UnknownEffect` | Outcome or effect uncertain: result/ACK lost after possible completion, TTL elapsed with an attempt in flight, restart found unconfirmed work, or a crash-window ambiguity (below) | no automatic exit; resolvable only with validated evidence |

Key invariant, unchanged: **`Expired` is reachable only when nothing was dispatched.** "We stopped waiting" is never conflated with "nothing happened" — in-flight uncertainty always routes to `UnknownEffect`.

## Transitions and guards

| # | Transition | Guard |
|---|---|---|
| T1 | `Proposed → Accepted` | scope within authority; TTL remaining; allowance available; not a duplicate; epoch current; not canceled |
| T2 | `Proposed → Failed` | any admission check fails — rejection recorded so identical retries replay it (usage known-zero) |
| T2b | `Proposed → Canceled` | cancel requested while still `Proposed` — nothing dispatched, stop trivially attested, usage known-zero |
| T3 | `Accepted → InProgress` | **serialized dispatch decision**: recheck current epoch, cancellation, authority, deadline, and allowance *at the decision point* (not relying on T1); then persist the attempt reservation **before** the external call |
| T4 | `InProgress → Completed` | executor result recorded, with usage evidence when present. Recording a late completion is truthful — but the root revalidates current epoch/state separately before using any plan or skill; the record never authorizes that by itself |
| T5 | `InProgress → Failed` | executor reports failure; `effect` set from attestation (`none`/`partial`/`unknown`) — a reported failure does not imply no effect |
| T6 | `Accepted/InProgress → Canceling` | root cancel request, or parent epoch advanced past the record's epoch |
| T7 | `Canceling → Canceled` | nothing was dispatched, or the executor attests stop **and** reports the effect (`none` or `partial`, recorded) |
| T8 | `Canceling → UnknownEffect` | an attempt was dispatched and no such attestation is available (cancel-ACK is never proof by itself) |
| T9 | `Proposed/Accepted → Expired` | TTL elapses before any attempt reservation exists |
| T10 | `InProgress/Canceling → UnknownEffect` | TTL elapses with an attempt in flight; result/ACK lost; restart finds unconfirmed work; crash-window ambiguity |

(T2b and T9 can race on a `Proposed` record; the single-writer decision point makes whichever is recorded first win. Both outcomes are terminal, truthful, and known-zero — the nondeterminism is safe, not a bug.)
| T11 | `Canceling → Completed` | completion evidence is recordable **whenever it occurred** — before the cancel was processed, or after it but before the stop took effect; provenance recorded; the result remains ineligible to authorize further work on the canceled root |
| (boundary) A completion reported **after an attested stop** contradicts the attestation; it is logged as an executor-integrity event, never recorded into the record. |
| T12 | `UnknownEffect → Completed/Failed/Canceled` | only with **validated evidence** (executor attestation via authenticated query, or operator resolution); validation may be automated — what is prohibited is resolution *without* evidence and any blind redispatch. Does not by itself unlock allowance whose usage is still unknown |

## Forbidden transitions

- No exit from `Completed`/`Failed`/`Expired`/`Canceled`. Work in these states never reopens.
- No direct `Proposed → Completed`.
- No resolution out of `UnknownEffect` without validated evidence; no blind re-dispatch from any state, ever.
- Child records never mutate the parent's allowance or bounds — usage flows up as accounting, never as a limit reset.

## Commit boundary and crash windows

All transitions for a record serialize through a **single-writer decision point**; the reference store is a single-writer, single-snapshot ledger. (`FileStorage` does atomic snapshot replacement but offers no envelope transactions or compare-and-swap, so the serialization lives in the writer, not the file layer.)

Write order per attempt: submission record → dispatch decision → **persist attempt reservation `{attempt_id}`** → external call → persist outcome.

Two crash windows look identical locally and must be handled the same way:

- crash after the reservation write but before the call was sent;
- crash after the call was sent but before the outcome was recorded.

Recovery queries the executor with the **same `attempt_id`**. An executor that cannot account for the attempt does *not* imply the call never happened — the record goes to `UnknownEffect` and is never re-dispatched. Concurrent duplicate submissions serialize at the decision point: the first wins admission, later ones get the recorded status or a conflict rejection.

## Allowance accounting model

`{reserved, consumed, released_unused, locked}`, settled per dimension:

- **Consumed is never refunded** — spent attempts stay charged to the root, including after restart and repeated reconciliation. Settlement itself is recorded and idempotent: re-reconciliation is a no-op, so `consumed` never double-counts.
- On terminal settlement with known usage `u`: `consumed += u`; `released_unused += reserved − consumed − locked`.
- Usage that is uncertain stays `locked` until validated usage evidence arrives. **Outcome evidence is not usage evidence**: a `Completed` record may still have unknown usage. An operator resolution must not silently refund uncertain usage — it settles only what the evidence covers.
- Anchor test: reserve 3, consume 2 → at most 1 reusable unit, stable across restart and repeated reconciliation.

## Reconciliation rules

1. Restart: load persisted records; reconcile from disk.
2. `InProgress` at restart (including both crash windows): query by `attempt_id`. Confirmed outcome → `Completed`/`Failed` with effect recorded. No account → `UnknownEffect`.
3. `Accepted` at restart: no dispatch has happened; re-run the full T3 guard set before any dispatch — if epoch/cancel/authority/deadline no longer hold, settle as `Expired` or `Canceled`.
4. `Canceling` at restart: stay `Canceling` until T7/T8 resolves via the executor.
5. Uncertain-usage allowance stays locked (accounting model above); outcome reconciliation never auto-unlocks it.
6. Reconnect alone is not restart recovery: the fixture kills the process and recreates the service against the same files.

## Mapping to the agreed test list

1. Identity/epoch/authority/expiry/allowance → T1/T2 guards, bounds, accounting model (incl. the 3-reserved/2-consumed anchor).
2. Identical retry / conflicting ID / expired reopen → dedupe rules + terminal prohibition.
3. Parent cancel / epoch advance rejecting late plans → T6 + stale-epoch rejection at T1 **and** the root's separate current-state check before using a T4-recorded result (T4's guard).
4. Cancel-ACK not proof of stop → T8; cancellation while `Proposed` → T2b; cancellation after dispatch cannot guarantee the executor stops (stated bound).
5. Restart with uncertain allowance or lost result → reconciliation rules 1–6; crash-window tests around dispatch and settlement; concurrent duplicate submission.
6. ACK-lost action → `UnknownEffect` + T12 with validated evidence; device keeps action authority.
7. Routing vs delegation separation → structural: no routing concepts here.

## Answers to the five tightening points

1. **Settle, don't refund** — three-way ledger; consumed stays charged; outcome≠usage evidence; operator resolution refunds nothing uncertain. Anchor test included.
2. **Dispatch/crash boundary** — attempt reservation persisted before the call; both crash windows recover via `attempt_id` or uncertainty; single-writer serialization; crash + concurrent-duplicate tests added.
3. **Epoch/cancel races** — T3 rechecks everything at the serialized decision point; `Proposed` cancellation is T2b; `Accepted` restart recovery re-runs the guards; T4 records truthfully while the root's current-state check stays separate and is now in the test mapping.
4. **Outcome vs effect vs usage** — `effect` field added; `Failed`/`Canceled` no longer imply absence of effect; T11 widened so completion evidence is recordable whenever it occurred before the stop took effect, while never authorizing further work on a canceled root.
5. **Evidence-driven reconciliation** — reworded: prohibited is resolution *without* validated evidence, not automation; authenticated executor queries are valid evidence; T12 also covers confirmed-stop-without-completion (→ `Canceled`); no automatic unlock of unknown usage.

Scope noted: this slice stays an optional contract testbed; the primary scenario (device Agent + edge/cloud model services, no cloud Agent required) does not depend on it, and hardware acceptance targets stay open.

If these distinctions land as intended, I'll start the reference implementation with the fault tests mapping 1:1 to this table.


## Implementation interpretations

Decisions the reference implementation made where the table above leaves
room. Recorded so the maintainer can veto; each is implemented as written
here.

a. **Canceling + executor reports failure** — the table has no
   `Canceling → Failed` entry. The implementation settles it as an attested
   stop with the reported effect (T7 → `Canceled`): a failure report from a
   dispatched attempt attests that the attempt has ended. The effect is
   recorded, not reinterpreted as a clean outcome.
b. **`RecordUsageEvidence` is a complete accounting of the locked remainder** —
   do not call it with partial evidence: the uncovered remainder would be
   released as unused. Partial usage evidence has no representation; uncertain
   usage stays locked until complete evidence arrives.
c. **Dedupe row 3 deviation** — an identical retry (same ID, same content)
   against an `Expired` (terminal) record replays the recorded terminal status
   and returns nil rather than a literal rejection. The never-reopen
   invariant is unchanged: no admission re-runs and no state moves.
d. **Late submission after `CancelRoot`** is recorded and immediately settled
   `Canceled` (T2b path: root canceled while still Proposed) rather than
   returned as an error — the rejection is replayable like any recorded
   status.
e. **`MaxAttempts` is effectively unreachable above 1 in this slice**: a
   record dispatches once (`Accepted → InProgress`) and never returns to
   `Accepted`, so values above 1 have no effect. The bound is enforced at the
   decision point regardless (`0` = never dispatchable) so the field's
   contract holds if multi-attempt flows arrive later.
