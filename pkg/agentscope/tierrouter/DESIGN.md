# Tier-router harness (experimental, issue #67)

Milestone-1 slice: three-tier fake providers plus an operation-level routing
policy, with fault evidence. Sibling of `taskenvelope`: that package models
the delegation-state contract; this one models the routing side. The two are
structurally separate — selecting another model endpoint does not create a
remote-Agent subtask, so nothing here carries scope, allowance, or
root/parent identity. An operation that needs a remote *agent* goes through
`taskenvelope`; an operation that needs a remote *model endpoint* goes
through this harness.

## Scope

- Deterministic fake providers for three tiers: device, edge, cloud.
- A routing policy mapping an operation to an ordered tier preference.
- A persisted decision log: every dispatch decision and its outcome is
  recorded; grading reads the log and the providers' request records, never
  a provider's self-report.
- Fault tests mapping 1:1 to the invariants below.

Non-goals (later, separately reviewed): allowance accounting (envelope
territory), arrival-rate bounding (the flood case raised in #67), real
provider integration, multimodal operations, hardware targets.

## Model

An **operation** is one routable inference unit:
`{id, task_epoch, kind, local_only, content, ttl, submitted_at}`.
`id` is caller-allocated and is the idempotency key; the dedupe hash covers
`kind`, `local_only` and `content`.

A **policy** answers one question: for this operation, which tiers are
allowed, in what order? The first entry is preferred. `local_only`
operations are filtered to the device tier *after* the policy answers — the
guard binds at the decision point regardless of policy content.

A **decision record** per operation:
`{id, content_hash, task_epoch, state, reject_code, attempts[],
submitted_at}`. An **attempt** is `{seq, tier, reason, outcome, result,
error, recorded_at, settled}`. Reasons: `policy-preferred`,
`fallback-after-failure:<tier>`.

Attempt outcomes:

| Outcome | Meaning | Fallback allowed? |
|---|---|---|
| `completed` | provider returned a result | — (operation done) |
| `failed` | clean failure; the request provably did not produce a result | yes, as a new recorded attempt |
| `unknown` | timeout/cancel mid-call; the request may have been processed | **no** |

Operation states:

| State | Meaning | Terminal? |
|---|---|---|
| `pending` | admitted; an attempt may be in flight | no |
| `completed` | an attempt completed; result recorded | yes |
| `rejected` | never dispatched: stale epoch, expired TTL, or no route | yes |
| `exhausted` | every candidate tier failed cleanly | yes |
| `superseded` | admitted, then the guards ceased to hold before a later dispatch or when an in-flight call settled; prior attempts remain as evidence | yes |
| `uncertain` | an attempt outcome is unknown | no automatic exit; retry means a new operation id |

## Invariants

- **R1 Recorded decisions** — every dispatch has an attempt record persisted
  *before* the provider call (commit boundary). Grading reads the decision
  log and provider request records.
- **R2 Epoch binding** — dispatch requires `task_epoch == current epoch` and
  remaining TTL, checked at admission and rechecked at every dispatch
  decision point (each attempt's commit boundary). A stale or expired
  operation at admission is rejected and the rejection is recorded; if the
  guards cease to hold *after* admission — the epoch advanced or the TTL
  elapsed while an attempt was in flight — the in-flight attempt settles
  truthfully; when the in-flight call ends in a clean failure and a further
  dispatch decision is reached, the guards block it and the record is
  `superseded` with the stop reason recorded, and when the in-flight call
  succeeds the guards are rechecked at the settle point itself, so the
  attempt stays as `completed` evidence while the record is `superseded`
  and the stale result is never returned. (If no candidate remains, the
  loop instead settles as `exhausted` on the settled evidence.) Neither is
  silently re-evaluated later.
- **R3 Idempotent identity** — same `id` + same content replays the recorded
  outcome without re-dispatch, including across restart. Same `id` +
  different content is a conflict rejection. Concurrent duplicates serialize
  at the decision point: the first wins; later ones observe the in-flight
  record (`ErrInFlight`) and may replay once it settles.
- **R4 Guarded fallback** — fallback to the next preferred tier only after a
  clean failure, as a new recorded attempt carrying the failure as evidence.
  `local_only` never leaves the device tier; a device failure is terminal
  for the operation.
- **R5 Uncertainty is not retryable** — a timeout or cancellation mid-call
  records the attempt `unknown` and the operation `uncertain`; the router
  does not fall back. The request may have been processed: silently
  re-issuing it against a billed endpoint doubles cost, and against an
  action-capable endpoint doubles effect. Retry means a new operation id.
- **R6 Restart reconciliation** — at `Open`, any record still `pending` is
  crash residue (nothing can legitimately be in flight across a restart) and
  is marked `uncertain`: the process died before the first attempt, between
  the commit boundary and the outcome, or between attempts. Unsettled
  attempts within it are recorded `unknown`; settled outcomes are preserved
  as evidence. Reconnect alone is not restart recovery: the fixture
  recreates the router against the same log file.

## Fault tests (1:1)

| # | Fault | Invariant |
|---|---|---|
| F1 | Normal route to the preferred tier; grading from the log plus provider request records | R1 |
| F2 | Route under epoch N; advance the epoch; the stale operation is rejected and recorded; a fresh operation under N+1 routes | R2 |
| F3 | Duplicate id with same content (provider request count stays 1); conflicting id rejected; duplicates still replay after restart | R3 |
| F4 | Cloud fails cleanly → edge attempt recorded with the failure as evidence; `local_only` + device failure → terminal, edge/cloud request records stay empty even when policy prefers edge | R4 |
| F5 | Cloud hangs → caller deadline → `unknown`; no fallback attempted; a new operation id retries cleanly | R5 |
| F6 | TTL elapsed at the decision point → rejected, providers untouched | R2 |
| F7 | Kill between the commit boundary and the outcome → reopen → `uncertain`, never silently re-dispatched | R6 |
| F8 | Concurrent duplicate while the first call is in flight → `ErrInFlight`, no double dispatch; replay after it settles | R3 |
| F9 | Policy tier with no attached provider → clean failed attempt with configuration evidence; chain continues | R4, interpretation d |
| F10 | Kill after admission but before the first attempt (pending, zero attempts) → reopen → `uncertain`, replay `ErrUncertain` | R6 |
| F11 | Kill between attempts (pending, all attempts settled after a clean failure) → reopen → `uncertain`, replay `ErrUncertain` | R6 |
| F12 | Epoch advances after admission but before the first dispatch → `superseded`, nothing dispatched | R2 |
| F13 | Epoch advances, or TTL elapses, while the preferred provider is in flight → clean failure: in-flight attempt settles, fallback never dispatched; success: attempt stays `completed` evidence, record `superseded`, stale result never returned | R2 |
| F14 | Replaying a completed record after the root epoch advanced returns the historical record unchanged | R2, interpretation h |
| F15 | Guards cease after admission and the remaining candidates are unattached → `superseded`, no missed-tier record after the guard failure | R2 |

`TestFaultCorruptLog` additionally pins that a corrupt log file fails `Open`
loudly instead of silently starting empty.

## Implementation interpretations

Decisions where the model leaves room; recorded so they can be vetoed. Each
is implemented as written here.

a. Caller-context cancellation during a provider call records the attempt
   `unknown` exactly like a deadline — the caller gave up waiting, which
   says nothing about whether the provider processed the request.
b. `rejected` records carry no attempt; replaying a rejected duplicate
   returns the recorded rejection (`reject_code` maps back to the sentinel),
   it does not re-run the guards — a stale-epoch rejection replayed after
   further epoch advances still reports the original rejection.
c. An empty policy answer is a `no-route` rejection, not a policy error — a
   policy may legitimately refuse an operation kind.
d. A policy tier with no attached provider is recorded as a clean failed
   attempt (`provider not attached`) and the chain continues — the harness
   treats it as configuration evidence, not a panic.
e. A `pending` record is observable only to a concurrent duplicate (the
   deciding goroutine holds the operation until it settles) or after a
   crash (reconciled to `uncertain` at `Open`, never re-dispatched). A
   provider panic leaves the record `pending` in-process; a caller that
   recovers the panic observes `ErrInFlight` until restart, when R6
   reconciles it — the harness keeps this as evidence rather than guessing
   the call's effect.
f. Persist failures roll back only state whose external effect has not
   happened: a new or rejected record and a not-yet-dispatched attempt are
   dropped from memory so a transient disk error cannot poison an operation
   ID with a wrong signal. After a provider call there is no rollback — the
   in-memory outcome is the most truthful evidence available, and the caller
   sees the wrapped persist error.
   A guard-driven supersession is also kept: the guard outcome is
   re-derivable from the epoch source and clock, so memory and disk cannot
   diverge in substance.
g. Error classification here is a fakes simplification: every non-context
   provider error is a clean failure (fallback allowed). Real transport or
   decoding errors do not necessarily prove no result was produced — a
   connection reset mid-body is ambiguous. Real provider integration needs
   typed outcome evidence distinguishing provably-no-result failures from
   ambiguous ones; the latter must classify as `unknown` (R5).
h. Replay preserves history, not permission. A replayed record is evidence
   of what happened under its epoch; acting on a replayed result requires a
   separate current-state check by the caller (compare the record's
   `task_epoch` with the current epoch), mirroring the taskenvelope T4
   guard.
