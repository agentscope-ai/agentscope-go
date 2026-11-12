# Main Thread: one session shared by a cloud and an edge agent

The experimental `thread` package lets a cloud agent and an edge (on-device)
agent serve one continuous user session. Each keeps its own context, model and
tools. They share an append-only log, the Main Thread, that holds only what
was published: the user's input, the text the user actually saw, which tools
ran (each call's name and arguments, cut at 200 characters, never outputs),
summaries one side wrote for the other's window, ownership changes, background
tasks and their progress, reports, artifacts and markers. A Coordinator is the
single writer of that log. The design was proposed in
[#117](https://github.com/agentscope-ai/agentscope-go/issues/117).

[`thread`](../pkg/agentscope/thread/) holds the log entries and stores, the
projection of entries into a participant's context, the Coordinator, the
usage ledger, the tool-output list, leases and the contracts a participant,
a router and a forker implement (`runtime.go`). Two packages build on it and
land separately: `thread/participant` adapts a `UnifiedAgent` (or a cloud
master with sub-agents) and provides the tools an agent uses to transfer,
assign, report and read; `thread/route` provides a router with rules and an
optional classifier. This page describes the Coordinator's side.

Every exported identifier in this package is EXPERIMENTAL and may change
without notice. The repository tests are offline, with scripted
participants, so nothing here establishes task quality, cloud cost, latency
or energy benefits.

## Concepts

**Participants.** A participant implements `thread.Participant`. Its
`thread.Descriptor` gives its name, zone (`edge` or `cloud`), context window,
the budget of a summary written for it, the longest brief it accepts, its
tool groups and a description.

**One owner at a time.** Exactly one participant owns the foreground. Only the
owner's foreground activation reaches the user. Every owner change increments
the epoch; reply and owner-change writes carry a guard `{owner, epoch}` that
the store checks, so output from a participant that lost the foreground can
never be written as a reply. It is kept as a report with a
`rejected_output` marker instead.

**No privacy classes.** Every participant may read every entry, and the
cloud imports the edge's entries like any others. Keeping some data on the
device is future work.

**Cursors.** Each participant reports the last entry it imported
(`ParticipantState.Cursor`). The cursor and the imported content change in
one state update; a `UnifiedAgent` keeps them in `AgentState.ThreadCursor`
(`agent.ApplyThreadUpdate`, `agent.WithThreadInput`).

The invariants the Coordinator keeps are listed in
[`thread/doc.go`](../pkg/agentscope/thread/doc.go).

## How a query flows

1. `Coordinator.Submit` appends the user input.
2. If an activation is running, the input is steering for the current owner and
   arrives at its next safe boundary (after a tool step, before the next model
   call). Routing only happens at a query boundary.
3. Otherwise `Config.Router` picks the owner. A change appends an owner change
   based on the input. Without a router the query stays with the owner when
   it may take it, else goes to the first available participant that may (a
   participant the query names comes first).
4. The Coordinator imports what the owner has not seen (see "Context and
   summaries"), then starts its activation. Its events stream to the user.
5. When the activation ends, the visible text and the tool digest are sealed
   into a reply entry. An interrupted run gets an `interrupted` marker. A
   failed one gets a `failed` marker with the error, `unknown_effects` when
   it had called tools, then one failover to another participant that may
   take the query, or `no_owner`. The entry that ends the run (the `failed`
   or `interrupted` marker, else the reply) lists the inputs it took besides
   its trigger (`Entry.Steered`): the steering and the older inputs its cycle
   carried as context. An input that found no owner waits parked: it does
   not count toward `Limits.MaxPendingInputs`, at most that many are kept
   (the oldest beyond that is retired and stays in the log as context), and
   a later cycle imports it as context.

### Failover

A user message never changes the route while an activation runs: it steers
the running owner. The failover is the one automatic handover. When the
owner fails, the Coordinator picks another participant that may take the
query and queues its activation; if that participant is running a
background task, the failover waits behind the task instead of interrupting
it. The new owner's input is one line:

```
[cloud failed: <reason> (#41). You now answer the user: continue their request #37 "<the request, at most 120 characters>" from where cloud left off. What cloud did is in the summary and the entries above; its latest progress is #40.]
```

The import that goes with it brings the `failed` marker and the failed
run's last progress, so the new owner reads the error and the task state
instead of being told them at length. The new owner also takes the inputs
that steered the failed run. Without a new owner the request ends with
`no_owner`, and its steering with it. A failed wake does not fail over (its
report was already answered); the inputs that steered it are queued again.

## Handing the foreground over

A participant hands the foreground over with `Host.Transfer` and a
structured brief: goal, done, remaining, constraints and references. The
Coordinator appends the caller's visible text and the owner change in one
guarded write and starts the target with the brief; the caller then ends
its reply after the tool step (a `UnifiedAgent` does so with
`agent.MarkHandedOff`, ending it with `types.ReplyHandedOff`). The
Coordinator never waits for a summary fork.
Unlike a sub-agent call, nothing returns to the caller: the other side
continues with the user.

## Background work

`Host.Assign` starts a task on another participant in the background; its
events are not shown to the user, except confirmation requests. Its final
answer becomes a `report` addressed to the assigner. A report with `notify`
reaches a running recipient at its next boundary, or wakes the idle owner
(at most `Limits.MaxWakesPerQuery` wakes per query; task depth is limited by
`Limits.MaxAssignDepth`). Nothing addressed to a participant is left unread:
after every activation the Coordinator checks for entries past the
participant's cursor. `thread.Leases` lets tools that touch shared resources
(for example a file path) take a lease, so two participants cannot use one
at the same time; with `Config.Leases` set, the Coordinator releases a
participant's leases after each of its activations.

**Following tasks from the host.** While the Coordinator runs, each `assign`
and `report` entry reaches `Events()` (with `Foreground: false`). `Close`
stops delivering events at an arbitrary point, so entries not yet delivered
when it runs, such as the report of a task it interrupted, may reach only
the Store. A task still queued when `Close` runs gets no report then; the
next start's recovery reports it. So a host that follows tasks reads the
log (`Store.Read`) when it starts and then follows the event stream,
skipping events whose `Entry.Seq` the read already covered. The report
that ends a task sets `Task.End` to how the task ended: the reason the
assignee's run finished with (`types.ReplyFinishedReason`; a run that could
not start, for example on an unavailable assignee, ends with `error` or
`interrupted`), or `thread.TaskUnfinished` on the report recovery writes for
a task that had not ended when the session stopped. Progress reports about a
task leave `End` empty; treat any other value, including one this version
does not define, as the end of the task. When the run failed or was
interrupted after making tool calls, an `unknown_effects` marker follows the
report. An unfinished task's effects are unknown too, without a marker.
These are records of what happened, not a task state machine: a host that
needs a ledger, for example one that settles budgets or refuses to repeat an
action whose effect is unknown, builds it from these entries.

**Relation to `taskenvelope`.** [`taskenvelope`](../pkg/agentscope/taskenvelope/)
is a durable ledger for delegating a subtask to an external executor that
stays the action authority. A task here hands part of the conversation's
work to the other agent of the same session, and the log is its record.
Both write the record before the work, never redispatch, and treat a result
as information that authorizes nothing. They differ where an agent run is
not an executor call: the run streams events and asks the user to confirm,
it cannot attest its effects, and the assigner (with the user) decides what
an uncertain end means instead of waiting for validated evidence. The two
compose: a device agent's tools can dispatch each physical action through
`taskenvelope`, and a host can map task ends onto envelope states for
accounting after the fact. See the
[design note in #117](https://github.com/agentscope-ai/agentscope-go/issues/117#issuecomment-6769407061).

**Known extension point: admission.** The Coordinator commits an `assign`
entry before any host code sees it, so a ledger built from entries accounts
after the fact and cannot refuse a task. Once `thread/participant` lands, a
`deny` or `ask` permission rule on its assign tool gates each assignment
before it is committed. A host that must check a budget before a task starts
needs a hook on `Assign`, which does not exist yet; it will be added when a
host needs it. There is also no per-task budget and no per-task cancel:
interrupting the foreground or a transfer does not stop a task, and
`Coordinator.Close` does.

**Progress.** A participant reports its plan as `progress` entries of its
run (an agent's `update_plan` calls, with `agent.WithPlanTool`). A projection
shows only the newest progress of a run that is still going; once the run
replied, transferred, reported, answered late or was interrupted, its
progress drops out. A failed run keeps its last progress visible, which is
what a failover or a recovery points at.

## Context and summaries

Before each activation the Coordinator decides what the participant imports.

* **Peer summary (for the edge).** Summaries are written by the cloud's
  `thread.Forker` from a fork of its last request, without touching its main
  context. At the end of a cloud reply, when what a handover would bring the
  edge (its newest peer summary plus the entries after it, plus what the
  reply added) reaches `SummaryPolicy.PeerRatio` of the edge's window
  (default 0.8), so the edge can always take over within its window,
  failover included; also when the cloud hands the foreground to the edge,
  or when the cloud's own notes need a covering summary. The summary is
  appended as a `summary` entry with its coverage: through the cloud's last
  sealed entry, excluding entries written by others after the cloud's
  cursor.
* **Own notes (for the cloud).** The forker also writes notes for the
  cloud's own window once its input reaches `SummaryPolicy.OwnInit` (default
  10,000 tokens), updated whenever the input grew `OwnEvery` (default 5,000).
  The Coordinator offers them to the cloud (`Forker.OfferOwnSummary`), which
  installs them at its own compaction line. Notes are offered only once a
  peer summary covers their snapshot: once they replace the history, its tool
  work could no longer reach the edge.
* **Clear rule (edge handover).** When the edge is about to run after another
  participant held the foreground since the edge last acted, and the newest
  peer summary is newer than the edge's last activity, the edge's context is
  replaced in one update that goes with the activation: the peer summary and
  the entries the summary does not cover, including the edge's own when the
  summary excludes them. A clear whose activation fails is not recorded, and
  the next handover clears again. An edge that kept the foreground (for
  example while the cloud ran a background task for it) is not cleared; it
  imports incrementally.
* **Incremental import.** Otherwise the participant imports the entries past
  its cursor. A cloud participant imports them in full; an edge import stays
  within a share of its window (`ImportPolicy.EdgeShare`).
* **Chunked import (edge fallback).** When an edge import does not fit (no
  newer peer summary exists, for example because the cloud is unavailable),
  the Coordinator imports in chunks and compacts between them with
  `Participant.Compact` (a `UnifiedAgent` uses `CompactStrict`, which leaves
  the state unchanged on any failure). After `ImportPolicy.MaxRounds` it moves
  on and tells the participant which entries to read.

The tool-output list (`thread.EventList`) is opt-in: a participant that
records its tool outputs through `Host.RecordEvent` keeps them outside the
log and every context, and its peer in the other zone reads them on demand
through the host (`EventOutline`, `EventSearch`, `EventPage`; every read is
capped). Nothing is pushed, and a participant is never offered records from
its own zone.

## Wiring

```go
store := thread.NewMemoryStore() // or thread.OpenFileStore(path)

coord, err := thread.NewCoordinator(ctx, thread.Config{
    Store: store, Participants: []thread.Participant{edge, cloud}, Router: router})
// handle err
defer coord.Close()

go func() { for ev := range coord.Events() { render(ev) } }() // must be drained
_, err = coord.Submit(ctx, thread.Query{Text: "…"})
err = coord.WaitIdle(ctx)
```

Answer confirmation requests from `Events()` with
`Coordinator.SubmitUserConfirm`. Show only events with `Foreground` set as
the reply: background tasks and late output also appear in the stream,
marked `Foreground: false`. A run that ends normally with no text and no
tool calls writes an empty reply entry; skip it when rendering. `Interrupt`
cancels the foreground activation (one still queued behind a background task
on its participant keeps the foreground until that task ends, and only then
writes its interrupted marker); a transfer, task, report or artifact its
tools had not committed yet is refused (`thread.ErrInterrupted`) and nothing
is written. A write that committed just before the interrupt reaches the
participant's context as a note before its next activation, so the agent and
the log agree. If an interrupt reaches the new owner of a Transfer while the
old owner's pre-transfer events still wait to be shown, the rest of them are
dropped from the event stream (the Transfer's reply entry still carries the
whole text), so nothing of an old owner follows a later owner's events.
While those events wait, the new owner does not start and new entry events
wait behind the Transfer's; they flow when the user reads, when the old
owner's stream ends or on `Interrupt`.

A participant must fail `Activate` when it cannot apply the activation's
Sync (or does not know whether it did); returning a channel means the Sync
was applied, and the Coordinator then records a clear's reset. The notes the
Coordinator writes into a participant's context name a `thread_read` tool
for entries an import cut or left out, so give the agent a tool by that name
that reads through `Host.Read`. The `thread/participant` package, which
a later change adds, will provide one.

## Accounting

`Coordinator.Ledger()` records the model calls participants report through
`Host.RecordUsage` and the Coordinator's own fork and classifier calls, by
participant, path (member) and kind: `main`, `fork_peer`, `fork_own`,
`classifier`, `compaction`. Unknown
usage stays unknown: a call whose usage the provider did not report is counted
as unknown, and totals give no cost when any call is unknown or unpriceable.

## Durability and recovery

`thread.FileStore` writes one JSON line per append batch and fsyncs it (and
the directory when it creates the file); a torn final line is dropped on open,
anything else malformed is an error. After an fsync failure the batch may or
may not be durable, so the store refuses further writes. Duplicate
writes with the same dedup key are idempotent. On restart the owner and epoch
come from the log, and participants' cursors from their own state.

Cycles run one at a time, so the stopped process was answering one turn:
one cycle and the inputs that steered it or waited for it. A routing owner
change is based on its input, but the router writes one only when the owner
changes, and a run writes nothing before it ends unless it reports
progress. So the turn holds every open input that no entry is based on,
wherever it stands in the log (an input sent while the previous reply was
still streaming, or right after an interrupt, ran after that reply or
marker), and the open requests with an entry after the newest finished
request (such as an input skipped by an earlier cycle that got its own
cycle afterwards). The owner resumes the turn's requests as one: the
newest of them that an entry is based on (an owner change, progress, a
marker), else the newest, with the others joined to it. An
input a failed or interrupted run listed (`Entry.Steered`) goes with that
run's request: it is joined when the request is resumed and ends when the
request ended without an answer.

The owner gets a `restarted` marker and an input that quotes the request
and the inputs joined to it, the host's `Config.RecoveryNote` (why the
process stopped, on one line), the failure before the restart if any, and
the latest progress. The input also warns that work done before the restart
may or may not have taken effect, so the owner checks the actual state
before redoing anything. A request a Transfer handed to the owner is resumed
there like any other, and the input quotes it. If the owner is gone, the
turn's routing never finished: its request is routed again behind a
`restarted` marker whose dedup key starts with `requeue:` and counts the
restart, the turn's other inputs that
nothing is based on are queued again as Submit would queue them, and one
that something is based on gets a `recovered` marker. An open request whose
entries all precede the newest finished one gets a `recovered` marker and is
not re-run. A run that ends normally always writes its reply, empty when it
had no text and no recorded tool calls, so recovery sees the request
answered. After two restarts of the same
request it gets a `recovered` marker on its newest restart and waits for
the next message, and the inputs that would have joined it get `recovered`
markers too. Every marker has a dedup key of its own, so repeated restarts
are each recorded.

A background task left without a report gets a report saying it did not
finish (`Task.End` is `TaskUnfinished`), which points at its last progress;
the task is never re-run. Recovery cannot tell a task that never started
from one that was running: the log does not record when a task's run
starts. A restored participant re-imports its own entries past its
checkpoint, its live progress included. A participant accepts one
Coordinator at a time: `Coordinator.Close` releases participants that
implement `thread.Releaser`, so a new Coordinator over the reopened log can
bind them again. A graceful `Close` seals a running reply as interrupted
instead of leaving it for recovery.

## Limits

* Repository tests are offline, with scripted participants.
* One Coordinator process per log; `FileStore` takes no file lock and keeps the
  whole log in memory.
* Some state grows with the session: settled tasks stay in the Coordinator's
  task table, the usage ledger keeps every call, and an agent's fan-out is
  bounded only by its iteration limit at each assign depth. Imports and
  routing read the log from the start, so each step costs time linear in
  its length.
* The Coordinator does not persist which peer summary a participant holds;
  after a restart the next clear decides from the log.
* Resuming a request after a restart cannot tell which of its tool calls took
  effect; the resumed owner is told to check the actual state.
* A settle failure after a normal end (a note or a checkpoint the participant
  could not save) is logged; the Coordinator reports end errors only for
  failed ends.
* The loop bridge (`UnifiedAgentRunner.LoopOptions`) cannot hand the
  foreground over or ask the user; use the Coordinator.
* No privacy boundary: the cloud reads the edge's entries, briefs, reports
  and recorded tool outputs. Do not give the edge data that must stay on the
  device. Untrusted tool output is not tracked either: nothing gates a write
  after a web fetch beyond the host's permission rules.
