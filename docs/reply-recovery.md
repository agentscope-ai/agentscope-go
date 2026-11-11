# Reply recovery

Reply recovery is an opt-in UnifiedAgent feature that persists a typed,
versioned recovery snapshot through the configured `StateSaver`, so an
interrupted reply can be resumed without losing its built-in reply-budget
counters or its remaining iteration budget. It complements the existing
checkpoint mechanism: checkpoints already carry conversation state, while
recovery adds the per-reply execution state needed to continue the same
logical reply after a crash.

## Enable and resume

Add `agent.WithReplyRecovery()` and a `StateSaver` to opt into durable built-in
reply-budget state. Restore through `LoadCheckpoint` and `WithState`, then call
`ResumeReplyStream(ctx)` to continue the same reply without appending user input.
Ordinary `Reply`/`ReplyStream` starts a fresh logical reply with fresh counters.
Missing, completed, corrupt or unsupported recovery snapshots fail before work.

Recovery persists an owned, versioned `ReplyRecoveryState`: reply ID, active
status, next iteration and built-in token/USD budget counters. Safe points cover
initialization, successful model responses, tool batches, parked interactions and
accepted terminal events. A terminal swallowed by middleware does not complete
the checkpoint. Save failures stop recovery execution and surface as errors,
including when middleware swallows a core terminal. `SaveCheckpoint(ctx) error`
exposes persistence failures directly; legacy `Checkpoint` still logs them.

## Schema versions

Legacy checkpoints keep schema 1. Checkpoints carrying recovery state use schema
2; older readers reject them. Checkpoints of an agent holding experimental thread
state (`AgentState.PeerSummary` or `ThreadCursor`) use schema 3, with or without
recovery state; readers from before schema 3 reject them.
`agent.StateSchemaVersion` is the newest schema `LoadCheckpoint` accepts, not the
version every write carries. New readers accept older conversation state, but
explicit resume needs the typed recovery snapshot. Roll back to an older binary
using a pre-recovery checkpoint or start a fresh conversation; do not strip
budget fields to simulate a successful resume. Budget snapshots reject unknown
versions, negative/nonfinite counters and oversized namespaces on save/resume.

## Limits of the contract

Recovery budgets account for successful logical responses observed by the
built-in middleware. They are not a hard cap on physical retries, auxiliary
requests or in-flight spending. Custom middleware state is outside this typed
protocol. A crash between provider execution and a successful checkpoint can
lose unpersisted observations. Tool batches can be re-executed after a crash;
tools still need idempotency appropriate to their side effects. Restored pending
tools are checked against the current toolkit, permissions and input validators.
Unfinished allowed/asking calls return to pending on explicit resume, so a saved
approval cannot bypass current policy; recorded results are retained.
Concurrent recoverable replies on one agent are rejected until actual core
completion, even if cancellation already closed the outward event channel.
Recovery middleware may invoke its reply handler at most once; invocations after
the middleware output is sealed are rejected without executing another core.
This API applies to UnifiedAgent replies; the loop bridge does not provide
reply recovery.
