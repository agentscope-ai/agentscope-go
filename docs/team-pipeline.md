# TeamPipeline (Experimental)

`pipeline.TeamPipeline` gives a leader one `TeamAssign(member, prompt)` tool.
Registered members run in their own conversation contexts and can receive
follow-up assignments within that run. The host consumes participant events
through one `TeamRun` and answers confirmation or external-execution requests.
Only a member's final model round's text becomes the leader's tool result.

This is the serial, text-result, run-local stage agreed in
[#73](https://github.com/agentscope-ai/agentscope-go/issues/73#issuecomment-6464739698)
and tracked in [#84](https://github.com/agentscope-ai/agentscope-go/issues/84).
The existing inbox-style `team` package, `Spawn`, and `AgentManager` keep their
own contracts. See the completely offline [example](../examples/team_pipeline/):

```bash
go run ./examples/team_pipeline
```

It reads a real temporary fixture after an explicit host confirmation and then
asks the same member a follow-up. Models are scripted; it needs no credentials,
external service or human input. Its host approves only its own fixture tool.
Applications must make their own permission decisions.

## Construction and ownership

```go
type TeamAgentFactory func(context.Context) (*agent.UnifiedAgent, error)
type TeamLeaderFactory func(context.Context, tool.Tool) (*agent.UnifiedAgent, error)

type TeamMember struct {
    Name, Description string
    Factory TeamAgentFactory
}

func NewTeamPipeline(leader TeamLeaderFactory, members ...TeamMember) (*TeamPipeline, error)
func NewTeamLeaderToolkit(assign tool.Tool, others ...tool.Tool) (*tool.Toolkit, error)
```

Each `Start` calls the factories to create a fresh leader and members. Each
member retains its own history across assignments within that run. A later run
starts fresh; the library does not clear or reuse caller-supplied conversation
state. Member names must be unique, match the created agents, and differ from
the leader. Returned instances must be distinct.

The leader factory receives the reserved `TeamAssign` tool explicitly. Install
that exact tool in its toolkit. `NewTeamLeaderToolkit` rejects nil tools,
duplicate names and an existing tool called `TeamAssign`, instead of overwriting
it. Factories constructing their own toolkit must enforce the same contract;
the pipeline cannot inspect the agent's private toolkit or detect a factory
that ignores the provided tool.

Every participant must configure `agent.WithPermissionContext` so its engine
and input channels exist. Choose permissions explicitly: `TeamAssign` does not
grant itself permission, and members' own tools retain their permission checks.
Factories must construct idle agents exclusively owned by this run, without an
active `Conversation`. Do not invoke them independently or return an agent
shared with another pipeline/run. The pipeline detects duplicate instances
inside one run and rejects overlapping runs on the same pipeline; it cannot
prove that external code obeys exclusivity or that factories return fresh state.
Models, tools and backends remain caller-owned. Separate conversation context
does not isolate files, processes or shared application objects.

## Run and host input

```go
run, err := team.Start(ctx, "Delegate this task and check the result.")
if err != nil { return err }
defer run.Cancel()
for ev := range run.Events() {
    // Forward events to your UI. For a pending request, submit the host's
    // decision using that participant's original ReplyID and call IDs.
}
return run.Wait()
```

`Events()` preserves participant reply IDs. A member's `ReplyEnd` ends that
participant invocation; it is not the team's completion signal. `Done()` and
`Wait()` establish run completion after accepted cores exit. Drain events or
cancel the run: abandoning a normal run applies backpressure and can block Wait.

`SubmitUserConfirm(requestID string, *event.UserConfirmResultEvent)` and
`SubmitExternalResult(requestID string, *event.ExternalExecutionResultEvent)` return an error.
Use the pending request's `GetEventID()` as `requestID`. Initial tool approval
and a later workspace-directory grant can share a reply ID and call ID while
being different requests; an old approval cannot authorize the new request.
Submit one complete pending request, including its original participant reply
ID, every requested tool-call ID and unchanged tool names. Confirmation may
edit tool input as supported by the agent; it cannot replace the tool identity.
External results must specify a valid `ToolResultState`. Ordinary leader/member
external tools and confirmation requests pass to the host. Only the leader's
reserved TeamAssign handoff is consumed by the coordinator.

Unknown, stale, duplicate and mixed-owner responses return `ErrTeamInput`.
Canceled/finished runs return `ErrTeamClosed`. A nil or unsupported payload is
invalid input. A successful submit means accepted once into a bounded local
queue; cancellation can still stop it before dispatch. Never write directly to
the underlying agents' submission APIs while a team owns them.

Submissions are copied before acceptance, preserving concrete `permission.Rule`
and standard content-block types. Do not mutate input during submission; after
it returns, caller mutation does not change the accepted copy. Exported structs,
strings, numbers, booleans, pointers, arrays/slices and string-keyed maps are
supported. Cycles/depth over 64, more than 100,000 copied value nodes, opaque
struct fields, functions and channels are rejected rather than aliased. Events
are observations; do not mutate ordinary forwarded event payloads.

## Ordering, results and errors

All assignments, including different members from the same model response,
execute serially in original call order. A confirmation/external wait blocks
later assignments. Existing external/unsafe-tool barriers and tool concurrency
options retain their behavior; this stage does not add cross-member concurrency.

A member succeeds only after a matching `ReplyCompleted` and nonempty final
model-round text. Earlier-round narrative, thinking and tool results are not
returned to the leader. Text blocks are joined with newlines. Buffered and
streamed model output use their existing event ordering; compression-only usage
events do not reset the answer. Middleware-generated reply rounds must preserve
the agent event contract.

Mixed text/media returns text only. Media-only or empty final output becomes an
explicit error tool result. Data/media transfer is deferred. Member error,
missing/partial terminal output or `exceed_max_iters` becomes an error result;
member interruption becomes an interrupted result. The leader can inspect those
results and continue. Parent cancellation stops further admission. A leader
failure or parent cancellation returns an error from `Wait`; known participant
usage events are forwarded without an exact team-wide cost claim.

`Cancel()` is idempotent. Each participant invocation owns a fresh managed core
scope, preserving outer application ownership. Cancellation joins active cores
before releasing the pipeline for another run. A tool ignoring context keeps
that ownership until it actually returns; Wait has no timeout that permits
overlap. This does not track arbitrary detached tool-owned goroutines/processes,
roll back side effects, or provide durable team recovery.

## Python reference and deliberate differences

The reference is [Python TeamPipeline at `2929776`](https://github.com/agentscope-ai/agentscope/blob/2929776bb66661c10cd7cc766478fcab515243ec/src/agentscope/pipeline/_team_pipeline.py)
and its later [failure-round cleanup fix #2982](https://github.com/agentscope-ai/agentscope/pull/2982).
Go retains its singleton external-tool barrier, uses fresh participant factories
per run rather than retaining a leader and clearing only member context/summary,
and returns final text instead of Python's Text/Data block subset. Python maps
member iteration exhaustion to success; this stage deliberately reports an
error result. Parallel members, nested teams, cross-run history, remote-member
protocols and durable pause/resume require separate designs.
