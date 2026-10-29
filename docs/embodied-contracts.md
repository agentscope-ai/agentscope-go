# Embodied observation and action contracts

The experimental `embodied` package implements the #67 milestone-2A
observation, structured-plan and simulated action-admission contracts. It is
a deterministic testbed, not a robotics runtime, hardware safety layer or
real-time guarantee.

## Observation freshness and disclosure

`Observation` binds root task, epoch, observation ID/sequence, capture time,
host clock domain, maximum age, coordinate frame, units, calibration, map and
site-state versions, and immutable JSON content. All are required. Capture in
the future, unknown clock domain, non-positive maximum age, expired
observations and mismatches with host `WorldState` fail closed.

`Project` releases only the approved summary or opaque media reference for a
requested tier. A tier with no permitted projection receives an error; no
empty projection is silently substituted. Media references are never fetched
in this slice. Observation IDs are bound to content: reuse with different
content is `ErrObservationConflict`.

## Plans, skills and action identity

A `Plan` references the exact observation and site-state versions and has an
absolute validity deadline. Each `PlanStep` names a registered skill version,
destination, arguments and preconditions. `Gateway` executes one `Action` and
never schedules or advances steps. The host owns step ordering and must stop
after unresolved outcomes.

Canonical action content covers the complete plan, selected step, action ID,
observation, arguments and validity data. The scope is
`skill@destination@version`, allowing both Store dispatch and controller
admission to reject revoked scopes. Changed destination, arguments or other
immutable fields with the same action ID return `ErrActionConflict` instead
of replaying the original action. Gateway metadata is persisted before Store
submission and deep-copied to prevent caller mutation.

Every action uses one attempt and positive allowance. TTL is derived from the
plan's absolute deadline. Gateway and Store share the host clock.

## Dispatch and recovery

`Gateway` composes `taskenvelope.Store`; it does not reimplement its state
machine. Before every submission or replay it checks current observation,
plan, skill, destination and preconditions. Historical records remain
readable, but stale replay is not currently valid authorization for more
work.

`SimController` performs final local admission immediately before applying an
effect, closing the gap between gateway admission and execution. Its durable
log binds attempt ID, full payload hash, command, effect and outcome.
Identical duplicates return the recorded outcome without another command;
conflicts leave evidence unchanged. Commands without durable outcomes remain
uncertain and are never blindly redispatched. Controller records, not
executor self-reports, are grading evidence.

`Recover` reopens the same metadata, Store and controller logs. It cancels
stale roots, reconciles Store state and resolves `UnknownEffect` only from an
accounted controller query. Lost ACK recovery therefore does not repeat the
effect. A reported failure may carry `EffectPartial`; outcome and physical
effect remain separate.

## Validation and exclusions

```bash
go test -race -count=1 ./pkg/agentscope/embodied
```

Tests cover observation freshness/projection, action identity and conflicts,
final-admission revocation, duplicate delivery, lost ACK/restart recovery,
partial effects and cancellation. They establish modeled invariants only.

Real providers, media loading, automatic plan progression, cloud Agents,
physical controllers, fleet leases, distributed quotas, streaming, arrival
rate limiting and hardware/performance validation remain separate work.
