# Load testing

The experimental `bench` package provides two load-generation modes. Use
`Runner.Run` with `Scenario` for a fixed number of concurrent workers. Each worker
waits for its callback before starting another iteration, so its arrival rate
decreases when work slows down.

Use `Runner.RunOpenLoop` with `OpenLoopScenario` to offer work on a fixed schedule.
Arrival offsets are relative to the start of observation; equal offsets represent
a burst. A slow callback does not postpone later arrivals. This is the first
measurement building block for [RFC #11](https://github.com/agentscope-ai/agentscope-go/issues/11),
not an application scheduler or a complete agent-quality benchmark.

## Configuration

`ArrivalOffsets` must be nonempty, nonnegative, and nondecreasing. `MaxInFlight`,
`Timeout`, and `DrainTimeout` must be positive. The runner copies the configuration
and schedule before starting work; do not mutate them concurrently with that copy.

- `MaxInFlight` bounds admitted callback wrappers, including inspection of a
  returned error and publication of the result. A full limit rejects an arrival
  immediately. There is no generator queue. These rejections describe the load
  generator's limit, not a response from an inference server.
- `Timeout` starts at the scheduled arrival, so scheduler delay consumes it.
  Overdue arrivals are retained as timed out rather than rescheduled.
- `DrainTimeout` sets the observation deadline after the last scheduled arrival.
  An earlier caller deadline also bounds observation. Observation can end early
  once every arrival has been offered and every admitted wrapper has finished.

The callback receives a context with the applicable deadline. Return promptly on
cancellation, and keep returned errors' `Error`, `Is`, and `Unwrap` methods prompt.
Callbacks and error inspection run outside the result-bookkeeping lock. A callback
that ignores cancellation keeps its slot until its wrapper finishes; timeout does
not permit another callback to replace it while it is still running.

Go cannot forcibly stop arbitrary callbacks. On cancellation or drain expiry the
runner returns an immutable outcome snapshot, cancels outstanding contexts, and
may leave at most `MaxInFlight` wrappers from that invocation finishing in the
background. The caller must arrange their termination before starting repeated
runs. This limit does not establish that remote inference stopped when its client
context was canceled.

## Reading the report

`Results` contains one record per planned arrival, in schedule order, with
one-based iteration numbers. `Offered` counts records whose scheduled arrivals
were reached by observation, including local rejection and timeout. Arrivals
interrupted before their scheduled time remain `not_offered`. An already-canceled
caller starts no observation: all records are `not_offered` and `Offered` is zero.

| Status | Meaning |
|---|---|
| `succeeded` | Wrapper completion was recorded before its arrival deadline with no error or cancellation |
| `failed` | Wrapper completed with an ordinary callback error before the deadline |
| `rejected` | Arrival reached the generator while its in-flight limit was full |
| `timed_out` | Arrival expired before authorization, completion reached its arrival deadline, the callback returned a deadline error, or observation expired before a due arrival could start |
| `canceled` | Completion reported cancellation before its deadline, or caller interruption prevented a due arrival from starting |
| `unfinished` | Wrapper had been authorized but no completion was recorded before observation ended |
| `not_offered` | Observation did not reach this planned arrival |

An arrival deadline takes precedence over a returned cancellation or ordinary
error. Completion at the exact arrival deadline is timed out. At the observation
deadline, neither a new start authorization nor a completion is accepted; an
authorized wrapper still lacking a recorded result is unfinished. Caller
cancellation returns the partial report and its context error. Reaching only the
drain deadline returns the report with a nil error; inspect the outcomes.

`StartedAt` records synchronized start authorization, not physical function entry.
A goroutine paused after authorization may enter the callback later with an
already-canceled context. `FinishedAt` records accepted wrapper completion,
including error inspection and bookkeeping. `Latency` is the interval from
`ScheduledAt` to `FinishedAt`, including scheduler delay. A wrapper whose callback
returned but whose error inspection has stalled remains unfinished.

Records without accepted completion have zero `FinishedAt` and `Latency`.
Unstarted records also have zero `StartedAt`. Do not insert these zero values into
a completion-latency distribution or omit their outcomes from the report. Late
completion cannot change a returned report. `EndTime` is the observation endpoint,
not the time when every noncooperative callback or remote operation stopped.

## Reproducible measurements

The [runnable example](../pkg/agentscope/bench/open_loop_example_test.go) uses an
offline callback. Run it with:

```bash
go test ./pkg/agentscope/bench -run '^ExampleRunner_RunOpenLoop$' -count=1 -v
```

For agent measurements, create isolated mutable agent/session state for each
independent task. Reuse only clients and tools whose concurrency contracts permit
it. Keep model and server versions, quotas/hardware, prompts, task inputs, tool
configuration, arrival schedule, and time limits with the report.

A nil callback error is not evidence of task quality. This driver deliberately
does not produce a task-goodput scalar or merge token usage. Join its iteration
records with application scoring and accounting before comparing harness
policies. Report all planned and offered outcomes, dispatch delay, completion
latencies, and unfinished work; separate generator rejection from backend errors.
The task-quality integration below adds a versioned corpus and scoring join;
complete inference-resource accounting remains subsequent work.

## Joining task quality with scheduled arrivals

`evalkit.Runner.RunLoad` connects the open-loop driver to a versioned task corpus
and a separate, bounded scoring phase. It does not change `RunOpenLoop` semantics.
The [quality-load example](../examples/quality_load/) includes a JSON manifest and
runs offline by default:

```bash
go run ./examples/quality_load > quality-report.json
```

The offline model returns fixed answers. Its results validate harness behavior;
they are not evidence of model quality or increased inference capacity. The
example's optional `-live` mode requires explicit service settings and a source
revision. Supply the actual model/server/tokenizer revisions, configured context
window, hardware or quotas, retry/cache policy and experiment group in manifest
metadata before using a live result for comparisons. The two demonstration tasks
and substring scorers are not a representative quality benchmark.

`WorkloadManifest.Version` is 1. Pin `TaskSetVersion` and `SourceRevision`, assign a
unique `RunID`, and declare `Scenario` and a finite `QualityThreshold` in `(0,1]`.
Task sampling temperatures and cost budgets must also be finite. TaskSpec keeps
its existing JSON representation with Go field names
(for example `Budget.MaxIters`, `Budget.MaxInTokens` and `Sampling.Seed`); YAML
field names are unchanged.
Each arrival names a task, a positive repeat and an offset. Task IDs and
(task ID, repeat) pairs must be unique; offsets are nonnegative and nondecreasing.
The report joins run ID, scenario and **one-based iteration**, preserving task ID
and repeat. `TaskSpec.Repeat` is used by `RunSuite`; `RunLoad` uses the explicit
arrival schedule instead.

Pass execution and scoring contexts separately:

```go
report, err := runner.RunLoad(executionCtx, scoringCtx, manifest, config)
```

Both contexts must be non-nil. `LoadConfig` and its limits must be non-nil/positive:

| Setting | Bound |
|---|---|
| `MaxInFlight` | Actual execution callback wrappers, including model construction and error inspection |
| `TaskTimeout` | Per-arrival deadline measured from scheduled arrival |
| `DrainTimeout` | Execution observation allowance after the last arrival |
| `MaxPendingScores` | Maximum number of task definitions and planned arrivals; bounds retained snapshots/workspaces |
| `MaxScorers` | Actual scorer workers, including error inspection and workspace cleanup |
| `ScoreTimeout` | Each scorer's context deadline |
| `ScorePhaseTimeout` | Whole scoring observation phase |

The runner's `TaskTimeout` additionally bounds each agent execution (default five
minutes). Effective limits are recorded in `LoadReport.Limits`. The factory must
provide a fresh model or a model safe for the declared concurrency. A custom
`Scorer` must support `MaxScorers` concurrent calls; otherwise leave it nil to
construct each task's declared scorer. Factories may return `ErrAdmissionRejected`
to identify application admission refusal. Generator rejection remains the
arrival's `rejected` status; structured provider failures appear separately in
`TaskResult.ErrorType`.

A task may be scored only when its execution snapshot is successful **and** the
open-loop driver accepts its callback as `succeeded`. Model errors, partial
responses, canceled/missing/unknown terminal events, exhausted iterations and
pending tool interactions cannot pass a budget scorer. The stock evaluation
runner does not implement a human approval or external-result UI.

Scoring receives the completed output without rerunning the task. Its context is
independent of the callback contexts, which the driver normally cancels on exit.
That cleanup cancellation cannot retroactively change accepted execution. A
scorer can inspect `TaskOutcome.Workspace` only during `Score`; it must not retain
or access the path after returning. The runner waits for the agent core and tool
collectors before transferring or removing the workspace. Tools must finish
workspace access before returning or closing their stream; detached processes
are a host responsibility. This waiting also means `RunTask` can return late if a
model or tool ignores cancellation.

A timed-out scorer keeps its worker slot while still running. Uncooperative
execution/scoring may outlive the observation interval, retaining at most
`MaxInFlight`/`MaxScorers` workers from that invocation. Their late results are
discarded and their workspaces are cleaned up after they return. Terminate these
workers before repeating a run. `ScorePhaseTimeout` bounds scoring observation;
filesystem cleanup after observation may add return latency.

`Results` retains all planned arrivals and separates `not_executed`,
`execution_failed`, `execution_unfinished`, `unscored`, `score_unfinished`,
`score_error` and `scored`. A scored task can still fail its quality threshold.
`CallbackEnteredAt` records observed callback entry; `Arrival.StartedAt` continues
to mean generator authorization. `CompletionSamples` and `Latencies` include only
accepted completions, excluding unfinished zero latencies. Execution and scoring
observation times are separate. Reports are detached from late task/score writes.

`Goodput` is quality-passing tasks accepted before their scheduled deadline per
observed execution second; it is absent for a zero-length interval. Always read
it together with planned/offered/authorized/entered counts, failures and unfinished
work. Current token/cost fields retain evalkit's existing accounting limitations:
unknown usage and hidden attempts are not a complete ledger. For the explicit managed Executor/Router path,
[RunManagedLoad](managed-routing-evaluation.md) instead preserves managed attempt
accounting and compares fixed, local-rule and classifier routing. Shared
governance across other execution paths remains proposed in the
[managed inference design](design/managed-inference.md).
