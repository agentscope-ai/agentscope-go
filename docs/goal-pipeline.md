# GoalPipeline

`pipeline.GoalPipeline` is an experimental, sequential executor-verifier
loop. Each attempt performs work, obtains an achievement report, and checks the
original goal. A failed verification feeds specific findings back to the
executor. The pipeline has no task queue, worker pool, or persistent goal store.

The [offline example](../examples/goal_pipeline/main.go) uses actual
`UnifiedAgent` instances with deterministic fixture models. The executor writes
a temporary pagination configuration; the verifier reads the artifact and
rejects its first cap. The next attempt corrects it. This demonstrates the
protocol, not a live model's ability to solve tasks:

```bash
go run ./examples/goal_pipeline
```

## Construction and results

Implement these role methods, or use the Agent adapters below:

```go
Execute(context.Context, string, string) (pipeline.ExecutionReport, error)
Verify(context.Context, string, pipeline.ExecutionReport) (pipeline.Verification, error)
```

The executor arguments are the original goal and previous feedback. The
verifier receives the original goal and latest report. Reports must be
nonblank; verdicts must be `pass`, `fail`, or `impossible`, with a nonblank
message. `impossible` is the verifier's judgment, not an objective proof.

`NewGoalPipeline(executor, verifier, GoalConfig{MaxIterations: 10})` returns a
pipeline and an error. MaxIterations must be positive and includes the first
attempt. Call `Run(ctx, goal)` and inspect both its result and error:

| Status | Go error | Meaning |
|---|---|---|
| passed | nil | The verifier accepted the goal |
| impossible | nil | The verifier judged it impossible |
| exhausted | nil | The last permitted attempt still failed verification |
| error | non-nil | Invalid participant output or an operational failure |
| canceled | context error | The caller canceled or its deadline expired |

A nil error does not mean the goal passed. Attempt history retains partial
reports and verification, including on error. Returned usage is copied;
later participant calls cannot mutate earlier results. Cancellation is
cooperative: custom participants and tools must honor their contexts.

## UnifiedAgent adapters

`NewGoalExecutor(a, GoalAgentConfig{MaxOutputRetries: 3})` retains the
executor's conversation between attempts. `NewGoalVerifier(factory, cfg)`
calls a `func(context.Context) (*agent.UnifiedAgent, error)` for each
verification. The factory must return a fresh, idle agent with the intended
tools, workspace and permissions. A verifier needs access to the same actual
artifacts as the executor; their conversations remain separate.

The adapter runs the work/check phase through ReplyStream. It drains the stream
and joins the library-managed cores using the existing internal run scope.
Only a matching, normally completed reply proceeds to report generation.
Confirmation or external-execution requests stop with ErrGoalInteraction;
the adapter cancels and joins work instead of approving or awaiting host input.
There is no public streaming/HITL-resume API or durable recovery in this stage.

After interrupted work, discard the agent or explicitly recover it before reuse;
the adapter does not clear pending calls or promise rollback of side effects.
Avoid detached background precompression in dedicated goal agents: its model
calls can outlive the work stream and are not a complete per-attempt cost record.

MaxOutputRetries counts additional report/verdict-generation attempts: zero
means one initial attempt. It does not count work attempts or bound all provider
requests, because structured-output compatibility strategies and provider
transports can make additional calls. Corrections only regenerate output;
they never rerun the work phase. Agent-adapter output is locally checked for
the fixed report/verdict fields, duplicate/unknown fields and size limits.

Overlapping calls on the same pipeline or adapter return ErrGoalBusy. Each
underlying agent is owned exclusively by its caller; do not share it with
another adapter, Conversation, or direct calls during a run. Models, workspaces
and other external resources remain caller-owned.

## Agent integration boundary

The additive experimental method
`UnifiedAgent.GenerateStructuredResponseWithUsage(ctx, schema)` returns
`(*agent.StructuredResponse, error)`, containing Output, Usage and
UsageComplete. It only generates a report from an idle, fully completed
conversation. It does not execute tools, append to history, acquire a session
lease, or change the old GenerateStructuredResponse method.

Its private snapshot includes system prompt, state-awareness/skills text,
summary, independent plan, and deep-copied message/block data, preserving
tool pairs and provider metadata. Each compatibility request gets its own
copy. Unsupported custom snapshot values return an error.

Each helper-visible model request passes through OnSystemPrompt and
OnModelCall, preserving provider compatibility capabilities. Model-call
options from compatibility strategies survive the adapter; middleware changes
to messages/tools/tool_choice remain effective. Middleware rejection is terminal,
and output is extracted after response filtering. The helper rejects malformed,
ambiguous, truncated or content-filtered candidates; it does not repair them
into a valid verdict. This stricter behavior is opt-in to the new method;
domain/schema validation remains the caller's responsibility.

The helper sums known usage once, including response-plus-error cases, and
returns it on failure. Unknown usage is not zero, and incomplete coverage sets
UsageComplete=false. Replay/cache middleware can supply usage without making
a real provider call. No exact provider bill is promised, particularly for
opaque internal retries. Existing cost middleware failure-accounting behavior
is unchanged.

For built-in token/USD reply budgets, each `Execute` or `Verify` call starts a
fresh scope with `middleware.WithReplyBudgetSnapshot(ctx, nil)`. Within that
call, work, report/verdict generation, output corrections and compatibility
requests share the counters. The executor and verifier have separate scopes,
and counters restart on their next call, including the next pipeline attempt.

This local scope shadows any reply-budget snapshot attached by the host;
Goal adapter usage does not charge those host counters. These per-call budgets
do not cap the whole `Run`: total consumption can grow with the number of
attempts. Built-in budgets observe successful model calls and may overshoot a
threshold; they are not a hard limit on provider charges. Hosts needing a
run-wide limit must enforce it separately.

Arbitrary custom middleware state is not automatically shared. Work usage comes
from existing model-call events, which cannot distinguish absent usage from
known zero or expose all internal retries. Consequently Agent-adapter totals
are conservatively marked incomplete. Report-helper usage is separate from
work usage and is never added twice.

## Python mapping

This implements the bounded executor-verifier pattern in
[Python GoalPipeline at ed357feb](https://github.com/agentscope-ai/agentscope/blob/ed357feb/src/agentscope/pipeline/_goal_pipeline.py).
Differences: text goals and non-interactive execution only; a fresh verifier
replaces all agent-local state instead of clearing only conversation/summary;
output-correction budgets reset per report/verdict; explicit Go outcomes and
partial results are returned. A verifier's pass depends on its evidence and
checks and is not a correctness guarantee.
