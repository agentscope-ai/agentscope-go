# Architecture and implementation guide

Use this file to find the code responsible for a behavior and the contracts a
change must preserve. Contribution workflow, community communication, validation
and mandatory evaluator review live in [AGENTS.md](AGENTS.md). API stability and
known hardening limits live in [STABILITY.md](STABILITY.md).

The module is `github.com/agentscope-ai/agentscope-go/v2`. Paths below are relative
to the repository root. Read the implementation and its adjacent tests before
changing an area; this map describes responsibilities, not a guarantee that every
feature is wired into every execution path.

## Source map

| Area | Start here | Responsibility |
|---|---|---|
| Global configuration | [config.go](pkg/agentscope/config.go) | Process configuration and `agentscope.Log()` |
| Messages and events | [message/](pkg/agentscope/message/), [event/](pkg/agentscope/event/) | Typed content blocks and agent lifecycle events |
| Agent interface | [agent.go](pkg/agentscope/agent/agent.go) | `Agent` and embeddable `AgentBase` |
| Agent execution | [unified_agent.go](pkg/agentscope/agent/unified_agent.go) | `UnifiedAgent`, model/tool rounds, middleware and interactive approval |
| Context and recovery | [compress.go](pkg/agentscope/agent/compress.go), [checkpoint.go](pkg/agentscope/agent/checkpoint.go) | Context compression, summaries and checkpoint loading |
| Loop integration | [loop_bridge.go](pkg/agentscope/agent/loop_bridge.go), [loop/](pkg/agentscope/loop/), [runtime/](pkg/agentscope/runtime/) | Agent-to-loop adapters and session execution |
| Model interface and providers | [model.go](pkg/agentscope/model/model.go), [model/](pkg/agentscope/model/) | Chat calls, responses, usage, provider configuration and model cards |
| Managed task routing | [router.go](pkg/agentscope/managedtask/router.go), [router_classification.go](pkg/agentscope/managedtask/router_classification.go) | Task-owned qualified pools, guarded classification and shared execution limits; [guide](docs/managed-routing.md) |
| Fixed managed text tasks | [managedtask/](pkg/agentscope/managedtask/), [openai_text.go](pkg/agentscope/model/openai_text.go) | Opt-in fixed deployment, host disclosure policy, bounded admission/retries and frozen attempt snapshots; [guide](docs/managed-task.md) |
| Probabilistic classification | [classifier/](pkg/agentscope/classifier/) | Typed decisions, validation, attempt accounting and offline fake; [guide](docs/classifier.md) |
| Jev adapter | [classifier/jev/](pkg/agentscope/classifier/jev/) | Single-attempt Jev HTTP conversion, bounded payloads and failure accounting; [guide](docs/jev.md) |
| Bailian adapter | [classifier/bailian/](pkg/agentscope/classifier/bailian/), [shared codec](pkg/agentscope/classifier/internal/systemone/) | Experimental System One decisions, provider limits and guarded single-attempt dispatch; [guide](docs/bailian.md) |
| OpenAI Decisions adapter | [classifier/openai/](pkg/agentscope/classifier/openai/) | Single-attempt text Decisions conversion, complete distributions and guarded dispatch; [guide](docs/openai-decisions.md) |
| Message formatting | [formatter/](pkg/agentscope/formatter/) | Provider-specific message and multimodal formats |
| Middleware | [middleware.go](pkg/agentscope/middleware/middleware.go), [middleware/](pkg/agentscope/middleware/) | Lifecycle hooks, budgets, tracing, guardrails and memory integration |
| Tools and permissions | [tool.go](pkg/agentscope/tool/tool.go), [orchestrator.go](pkg/agentscope/tool/orchestrator.go), [permission/](pkg/agentscope/permission/) | Tool contracts, execution and permission decisions |
| Execution backends | [tool/backend.go](pkg/agentscope/tool/backend.go), [workspace/](pkg/agentscope/workspace/) | Local or remote filesystem/command execution and workspace lifecycle |
| State and sessions | [storage/](pkg/agentscope/storage/), [session/](pkg/agentscope/session/) | Storage interfaces, implementations and session state |
| Service and UI | [app/](pkg/agentscope/app/), [service/](pkg/agentscope/service/), [webui/](pkg/agentscope/webui/), [console/](pkg/agentscope/console/) | Application wiring, HTTP handlers and interaction frontends |
| External integrations | [mcp/](pkg/agentscope/mcp/), [a2a/](pkg/agentscope/a2a/), [channel/](pkg/agentscope/channel/) | Tool servers, remote agents and messaging channels |
| Retrieval and memory | [rag/](pkg/agentscope/rag/), [embedding/](pkg/agentscope/embedding/), [memory/](pkg/agentscope/memory/) | Retrieval, embeddings and memory stores |
| Composition and scheduling | [pipeline/](pkg/agentscope/pipeline/), [team/](pkg/agentscope/team/), [schedule/](pkg/agentscope/schedule/) | Agent coordination and scheduled execution |
| Discovery and skills | [credential/](pkg/agentscope/credential/), [hub/](pkg/agentscope/hub/), [skill/](pkg/agentscope/skill/) | Provider configuration, integration discovery and skill loading |
| Edge and device support | [device/](pkg/agentscope/device/), [messagebus/](pkg/agentscope/messagebus/), [connectivity.go](pkg/agentscope/model/connectivity.go) | Device tools, pub/sub and cloud/local routing |
| Managed routing evaluation | [managed_load.go](pkg/agentscope/replay/evalkit/managed_load.go), [managed_summary.go](pkg/agentscope/replay/evalkit/managed_summary.go) | Direct Executor/Router load, independent scoring and whole-ledger cost per qualified completion; [guide](docs/managed-routing-evaluation.md) |
| Observability and evaluation | [tracing/](pkg/agentscope/tracing/), [audit/](pkg/agentscope/audit/), [replay/](pkg/agentscope/replay/) | Traces, audit records, deterministic replay and evaluation |
| Shared internal helpers | [internal/](pkg/agentscope/internal/) | HTTP transport, HTTP server hardening and atomic file replacement |
| Runnable examples | [examples/](examples/) | Usage examples; inspect each example's configuration and service requirements |

## Execution paths and contracts

### Agents, events and middleware

Bounded goal execution lives in `pipeline/goal.go`, with UnifiedAgent
adapters in `pipeline/goal_agent.go` and report generation in
`agent/structured_usage.go`. The adapters join their local run scope before
snapshotting context; output corrections never re-execute work. Keep report
usage separate from work events, and preserve middleware error provenance so
guardrail rejections cannot become compatibility retries.
See [GoalPipeline](docs/goal-pipeline.md) for ownership and Stage 1 limits.

`pipeline/team*.go` implements serial TeamPipeline delegation. Factories install
the reserved external TeamAssign explicitly; the coordinator validates pending
participant input, copies submissions without losing typed permission rules,
and joins an invocation-local run scope before returning a member result. Final
text is selected by model-round events, never by flattening the whole reply.
The existing external barrier and required agent/tool interfaces are unchanged.
See [TeamPipeline](docs/team-pipeline.md) for factory, input and cleanup limits.

`UnifiedAgent` is the primary agent implementation. `ReActAgent` is the deprecated
JSON tool-calling implementation; avoid extending it when the feature belongs in
the current agent. `UserAgent` and `A2AAgent` serve interactive-input and remote
agent use cases.

`UnifiedAgent.ReplyStream` uses `ChatStream` for ordinary rounds and forced
finalization when model streaming is enabled (the default) and no middleware is
installed. `agent/model_stream.go` converts text/thinking deltas to events, checks
final consistency and rejects incomplete streams before tool execution. Streams
that fail before publishing any delta are replayed (bounded, transient errors
only); streams that fail after publishing, and completed-but-errored responses,
are terminal. Tool calls with broken streamed arguments are answered with a
parse-error result instead of failing the reply; nameless calls still fail.
Provider context-window rejections trigger bounded in-reply recovery (tool-result
truncation, then forced compaction) instead of failing the reply. Legacy
middleware chains and `WithModelStreaming(false)` use buffered `callModel`/`Chat`
so response filters run before publication. `Reply` drains the same event path and
returns terminal reply errors. `UnifiedAgentRunner.LoopOptions` connects the
agent to `loop.Loop`; its model adapter still uses buffered `callModel`.
When changing retries, cancellation, permissions or accounting, trace the
specific entry point and its adapters rather than assuming they share all hooks.

`agent/conversation.go` adds `Conversation`, an experimental persistent turn
kernel (codex session/queue parity): `Submit` queues input, injecting it at
the certified StateReason boundary (where `checkNextAction` has certified no
tool call awaits its result) so a steered message never splits a
tool_call/tool_result pair;
`Interrupt` cancels the turn context — killing in-flight tool processes — and
retains pending input for the next turn, appending a model-visible marker;
`Compact` queues compression to the same boundary. Conversation turns emit
terminal events on the conversation context so an interrupted turn's
`ReplyEnd` survives cancellation, and steered input is checkpointed before
the next model call. While a conversation is active, direct
`Reply`/`ReplyStream`/`ResumeReplyStream` are rejected.

`Conversation.RunUserShellCommand` runs a user-typed shell command outside
the model loop (codex `!` parity): platform shell with process-group
isolation, merged output streamed as `user_shell_output` events and a
`user_shell_end` carrying exit code and duration. It never enters model
context and bypasses the permission engine by design (the user approved by
typing it) — never route model-generated commands through it. `Close`
cancels in-flight commands and waits for their goroutines before closing
the event stream.

`Middleware` provides reply, reasoning, model-call, acting, system-prompt,
compression and permission hooks, plus tool registration. `BaseMiddleware`
provides defaults. System-prompt transformation is a pipeline; the other hooks
have their own handler contracts. In particular:

- `OnReply` may consume a non-interrupted `ReplyEndEvent` to request another
  round. Interrupted ends must propagate. Forward `CustomEvent` values whose
  names start with `agentscope.`; these carry internal round-boundary signals.
- `OnCheckPermission` and its chain builder exist, but the current unified-agent
  and loop bridge paths do not automatically invoke that middleware hook.
  Follow the actual permission-engine calls when changing enforcement.
- The loop bridge cannot carry interactive approval or external-tool events;
  use `UnifiedAgent.ReplyStream` for that workflow. Do not silently approve a
  tool because an execution path cannot request confirmation.

Preserve event ordering, terminal events and cancellation-aware sends. Check
checkpoint schema compatibility and pending-tool state when changing recovery.
Read [checkpoint tests](pkg/agentscope/agent/checkpoint_test.go) and
[loop bridge tests](pkg/agentscope/agent/loop_bridge_test.go) for those boundaries.

`UnifiedAgent.ForkSession` branches a session by persisting a checkpoint-fidelity
snapshot under a new session ID (context, summary, plan and permission grants;
reply ID and recovery counters cleared). It is a between-replies operation and
requires a `StateSaver`; run the branch via `LoadCheckpoint` + `WithState`.

Model streams carry an idle guard (`ModelConfig.StreamIdleTimeout`, 5-minute
default): a silent stream is a transport failure — replayed while unpublished,
fatal once deltas escaped. Every reply ends with a `turn_stats` custom event
aggregating model calls, tool calls, iterations and token usage; on the
middleware swallow path it follows the delivered ReplyEnd, never preceding a
round that gets swallowed.

The `app` chat handlers register a per-run cancel handle in `ChatRunRegistry`.
Session cancel also preserves the App's registered background-task cancellation.
An internal context scope joins accepted Agent cores (including children sharing
that context) before releasing the session slot; `ReplyStream` closure alone is
not execution completion. The scope seals against late middleware core launches.
Ordinary SDK calls have no App scope. See [App cancellation](docs/deployment.md#canceling-an-app-chat).

External tools use [external_tools.go](pkg/agentscope/agent/external_tools.go)
for input validation, host handoff and successful-result validation. Restored
submitted calls pass through the current toolkit and permissions before handoff;
missing or no-longer-external tools must be rejected before asking permission.
External metadata flows through recorded results, `ToolResultEndEvent` and
`Msg.AppendEvent`. Preserve complete external block output in state and the order
of supported text/data events; event data-size limits still apply.

`console.Launch` returns the caller's context error at prompts, confirmations and
active replies. SIGINT during a reply cancels only that reply. The caller owns
input readers; Launch cannot unblock an arbitrary reader without its owner's help.

### Messages, formatting and model calls

`message.Msg` carries typed blocks, including text, thinking, tool calls/results,
data and hints. Preserve provider metadata when transforming blocks. Tool results
can contain structured or multimodal content; use their accessors or checked type
switches instead of assuming `Output` is a string. Inspect the relevant formatter
for the supported media types and wire representation. Tool-result wire rendering
uses all text blocks and bounded unsupported-media placeholders. The shared token
estimator includes every tool text block; it remains an estimate. The legacy
GetOutputText accessor still returns only the first text block.

`model.ChatModel` defines `Chat`, `ChatStream` and `CountTokens`. Provider adapters
live in `model/`; OpenAI Chat Completions and Responses have separate
implementations. Shared types or options do not prove that every provider
serializes a field or interprets it identically. Trace request construction and
response parsing in both call paths.

The OpenAI-compatible formatter expands a merged agent message into ordered
assistant/tool messages, keeping all calls in a round before their results.
Its output count can exceed the input count. Multi-agent formatting assigns the
sender before expansion is combined and merges only plain text messages without
tool calls or reasoning; structured media keeps its message boundary.

Anthropic requests use its public formatter. Gemini builds requests separately
in `model/gemini.go`: formatter support for media inside hints does not establish
adapter support. Both paths filter empty text before joining or emitting it;
nonempty whitespace remains content. Multi-agent formatting must associate each
output with its original sender before filtering messages.

For provider work:

- Reproduce protocol bugs at the outbound HTTP boundary with the real adapter.
  An agent test whose model mock ignores history cannot establish correct
  tool-call/result ordering. Cover both `Chat` and `ChatStream` where they share
  the affected conversion, plus the agent path that produced the report.
- Give the independent evaluator the failing request or fixture, source call
  path, regression result and complete proposed public explanation. Have it
  challenge the root cause, fix and review conclusion under the gates in
  [AGENTS.md](AGENTS.md#mandatory-evaluator-review); passing tests alone do not
  establish those conclusions.
- Check the exact endpoint, authentication requirements, JSON field names,
  omission rules and option precedence. Use captured HTTP requests in tests;
  checking a Go options struct alone cannot establish the wire format.
- Keep provider-reported `ChatUsage` separate from `CountTokens` estimates.
  Preserve input/output and cache token fields, including in final stream
  responses, wrappers and budgets.
- On successful completion, `ChatStream` emits deltas followed by a final
  assembled response with `IsLast=true`. Consumers must inspect
  `ChatResponse.Error` on every response and handle channel closure and
  `ctx.Err()`: cancellation can close the channel without a final response.
  Do not concatenate the assembled final content onto already accumulated
  deltas. Producers close their output channel and make sends cancellation-aware.
- Test setup failures, partial streams, terminal errors and cancellation.
  `FallbackChatModel` falls back on stream setup errors; it does not restart an
  already-started stream after a terminal error.
- Inspect transport behavior before adding retries. The JSON helper in
  [internal/httpx](pkg/agentscope/internal/httpx/) retries eligible failures with
  backoff and `Retry-After` handling; SSE and provider-specific request paths
  need separate inspection. Do not multiply attempts or duplicate transport logs
  without an explicit reason and tests.

HTTP client defaults and override precedence live in
[model/http.go](pkg/agentscope/model/http.go); individual constructors can also
supply their own defaults. A non-nil custom client is used unchanged. In the
shared client builder, a positive `ClientOptions.Timeout` overrides the default;
zero does not disable it. The shared SSE helper removes the client's total
request timeout and relies on the caller's context. Do not assume this describes
every provider-specific transport. Tests for longer calls must cover cancellation
as well as successful completion.

`SecretStr` and `ResolveAPIKey` support credential handling in provider configs.
Keep keys out of logs, errors and test fixtures committed to the repository.
`GenerateStructuredOutput` uses tool calling; its fallback and retry behavior
must be considered when accounting for model calls and usage.

The explicit `managedtask.Executor` path accepts one projected user text and a
concrete `model.OpenAITextClient`. It requires host qualification for production,
a trustworthy bound over final JSON plus output reservation, and dispatch-time
authorization. Its worker owns task/admission capacity through local cleanup;
returned terminal snapshots cannot be updated by late work. It does not govern
ordinary Chat/Agent paths or depend on the proposed shared-inference package.

The optional `managedtask.Router` registers fixed deployments and qualified pools.
Trusted metadata precedes local rules and guarded Choice classification; explicit
defaults/fallbacks retain shared task limits, admission and attempt accounting.
Jev/Fake implement `classifier.DispatchClassifier` for dispatch-time policy and
accounting. See [routing contracts](docs/managed-routing.md). Routes never mutate
a shared Agent; ordinary Agent/Chat paths remain outside this boundary.

Experimental `tierrouter.ChatRouter` is a separate opt-in `ChatModel` wrapper
for the actual buffered `UnifiedAgent.Reply` path with plain-text fake
deployments. It owns routing and fallback under the configured single-attempt
Agent setup, checks epoch/TTL before dispatch and before accepting responses,
and keeps completed responses only for process-local replay. It does not alter
`managedtask`, cover streaming/tools/multimodal/helpers, or recover responses
across processes. See [three-tier routing](docs/tier-routing.md).

Experimental `embodied` provides versioned observations, structured plans and
simulated action admission for issue #67 milestone 2A. `Gateway` composes
`taskenvelope` rather than duplicating its lifecycle, persists complete action
bindings and revalidates current state on replay/recovery. `SimController`
performs final local admission and owns an independent durable command/effect
log. The host schedules plan steps; the package does not fetch media, control
hardware or provide real-time/exactly-once physical guarantees. See
[embodied contracts](docs/embodied-contracts.md).

### Context and model capabilities

[agent/compress.go](pkg/agentscope/agent/compress.go) owns compression thresholds,
summary generation and retention of recent context. A positive
`ContextConfig.ContextSize` is an explicit override. Otherwise
`model.ResolveContextSize` takes the minimum over the members of a
`ConnectivityAwareModel` or `FallbackChatModel`; for other models it checks
`ContextSizer`, then `ModelNamer` plus a model card, then the inner model of a
`ChatModelUnwrapper` with the same fallback, then the caller's fallback. These
interfaces are optional; do not assume that a provider or wrapper implements them.

Embedded cards in [model/models/](pkg/agentscope/model/models/) describe model
metadata. They do not configure a server or establish its runtime context limit.
For local servers and fallback chains, check the configured window of every
model that may receive the request. Delegating metadata to the initially selected
model alone cannot protect a smaller model selected after a failed call.

Context changes must preserve valid tool-call/result relationships and account
for system prompts, summaries, tool schemas and multimodal content. Check
compression failure handling and recorded usage, not only the successful summary
path. Model capability data is not automatically enforced by every tool path;
verify the wiring before documenting such a guarantee.

Experimental `WithContentBlockCompaction` opts into the completed-exchange
splitter in [compress_blocks.go](pkg/agentscope/agent/compress_blocks.go) at those
same compression points. It preserves call/result intervals and owned message
fragments; defaults and checkpoint schemas are unchanged. See the
[retention policy and fixture limits](docs/context-compaction.md).

`UnifiedAgent.Compact` exposes the same pipeline for manual triggers (a host
UI's `/compact`), skipping the trigger threshold via an explicit force flag —
not a zero ratio, which `withDefaults` would normalize away. It is contracted
for use between replies; see [manual compaction](docs/tools.md#manual-compaction).

`ContextConfig.CompactStrategy` selects the post-compression layout:
`reserve_tail` (default) keeps the recent messages fitting `ReserveRatio`;
`fresh_window` folds the history into the summary, keeping at most the
trailing open tool pair (codex /compact parity). `SummaryMaxTokens` bounds the generated summary (default: window/32
clamped to [1024, 8192]); over-budget summaries are middle-elided so the
overview head and next-steps tail survive.

`ContextConfig.PrecompressRatio` (opt-in, must be below `TriggerRatio`)
moves summarization latency off the reply's critical path: crossing the lower
ratio starts a background generation from a context snapshot
([precompress.go](pkg/agentscope/agent/precompress.go)), and the hard trigger
applies a still-valid result without a blocking model call. Validity is
structural — same base summary plus the snapshot's boundary message identity
— so checkpoint rollbacks and interleaved compactions safely discard it; the
reserved suffix keeps the LIVE message versions so in-place truncation from
overflow recovery stays in effect. Precompression is off under
`WithContentBlockCompaction` (the block-level splitter's granularity differs)
and never retries or truncates on failure — the synchronous path owns that.

`WithPlanTool` registers `update_plan`: the model maintains a step plan
(at most one `in_progress`) stored in `AgentState.Plan` — checkpointed,
immune to compression, and re-injected into the model input on every call
(the compression split accounting counts the injected message). The tool is
read-only for permission purposes and deliberately NOT concurrency-safe:
updates are ordered state transitions, and batching would scramble
last-write-wins against event order. Each successful update emits a
`plan_updated` custom event. `WithContextRemainingTool` registers
`get_context_remaining`, reporting the usage-grounded remaining budget as
`{"tokens_left": N|null}`. Both tools follow the compress_context wiring
pattern: the tool validates, a callback into the agent mutates state.

### Tools, permissions and workspace boundaries

Tools implement `tool.Tool`, commonly by embedding `BaseTool`. The toolkit and
`tool.Orchestrator` handle registration, execution and permission decisions.
Adding a tool requires examining schema validation, error behavior, cancellation
and the relevant permission checks; registration alone does not provide them.
Do not weaken existing safety checks to make a new path work.

Experimental `agent.WithToolConcurrencyLimit` selects the bounded dispatcher in
[bounded_tools.go](pkg/agentscope/agent/bounded_tools.go) for both fresh and restored
tool steps. It admits through a serial coordinator, keeps hooks serial and joins
actual workers before ordered recording. Never-admitted calls become interrupted
on cancellation, including later sequential barriers. Checkpoint safe points and
the loop bridge are unchanged; see [the contract](docs/tools.md#bounded-tool-dispatch-experimental).

`tool.InputValidator` is optional semantic validation after schema checks;
`tool.ExternalResultValidator` validates successful external results against their
input. Keep the required `Tool` interface unchanged when adding such hooks.
[AskUser](pkg/agentscope/tool/ask_user.go) uses both hooks and is opt-in. Its host
must configure a permission context, consume `ReplyStream` and submit actual user
answers; the [offline example](examples/ask_user/main.go) deliberately simulates
those answers. See [the host contract](docs/tools.md#askuser) for limits and errors.

`tool.ShellManager` (with the opt-in `Shell`/`ShellStdin` tools) provides
persistent shell sessions: one long-running POSIX shell process per named
session, serialized commands, exit codes via a same-line sentinel (a separate
sentinel line would be swallowed by stdin-reading commands), head/tail-bounded
output, and process-group kills on cancellation or `Kill`/`Close`. Sessions
outlive turns by design; the host owns the manager's lifecycle. `Shell`
delegates the `Bash` permission pipeline under its own tool name, so learned
rules stay scoped per tool.

The opt-in `web_search` tool (`tool.NewWebSearchTool`) searches the web
through a host-injected `SearchFunc`, keeping provider credentials and
ranking outside the library (no new dependency). It is read-only and
concurrency-safe; results are ranked title/URL/snippet hits, with `limit`
defaulting to 5 and capped at 10.

MCP clients support server-initiated elicitation (codex
`Op::ResolveElicitation` parity): with an `ElicitationHandler` set on
`mcp.StdioConfig` or `streamablehttp.Config`, the client declares the
elicitation capability at initialize and resolves `elicitation/create`
requests mid tool call through the transport-neutral `mcp.ElicitationHandler`
(`ElicitRequest`/`ElicitResponse`, actions accept/decline/cancel). Stdio
answers unknown server requests with -32601 and runs the handler on the wire
read loop (other calls block meanwhile); the bare-POST `HttpClient` has no
inbound channel and does not support elicitation.

Bash/Shell rule matching is conjunctive across subcommands: a compound command
is allowed only when EVERY subcommand matches an allow rule
(`tool.MatchAllSubcommands`), while a deny rule fires when ANY subcommand
matches (`tool.MatchAnySubcommand`). The engine dispatches through the
`permission.CompoundRuleMatcher` interface; unparseable commands match no rule
and degrade to Ask. Learned "always allow" rules persist through
`permission.RuleStore` (`config.NewSettingsRuleStore` writes
`.agentscope/settings.local.json`, which `LoadProjectConfig` reads back next
session); `Engine.AddRulePersistent` refuses to apply a rule its store could
not durably record.

Project instructions layer codex-style: `LoadProjectConfig` concatenates
CLAUDE.md/AGENTS.md/README.md (first match per directory) from the `.git`
root down to the working directory, deeper files last, capped at 32 KiB. An
instruction file is content, not a project-root boundary.

File and command tools can use a `tool.Backend` from the context.
`workspace.NewToolBackend` adapts a workspace for use with `tool.WithBackend`.
When that backend is present, inspect its path and operation semantics instead
of assuming local OS behavior. Use `path` for workspace-relative paths with
forward slashes and `filepath` for actual host filesystem paths.

Local file-tool path confinement is opt-in through `tool.WithWorkspaceRoot`;
it does not provide process or network isolation. Session grants widen the
jail: when a jailed file tool rejects a path, `tool.NewErrorResponse` tags the
response with a typed `WorkspaceEscapeInfo` and the agent prompts the user to
grant the path (an existing path grants itself; a new path grants its parent;
`grant:dir:` suggested rules), records it in
`permission.Context.WorkingDirectories` (`Engine.AddWorkingDirectory`), and
re-executes once with `tool.WithWorkspaceGrants`. Every working directory in
scope widens the jail on each execution; grants checkpoint with the agent
state and merge back on resume, but are never written to the rule store.
Concurrent tool batches cannot prompt (nil event channel) — the escape error
stands and the model may retry the call alone. Permission modes, an
orchestrator sandbox policy and a workspace execution backend have different
responsibilities. A configured policy does not by itself enforce all filesystem,
network or resource restrictions. Verify each claimed boundary in the relevant
backend; consult [known hardening limits](STABILITY.md#open-hardening-work).

Shell safety uses the Unix parser and Windows-specific logic. Add platform guards
for tests that require a Unix shell; do not skip portable behavior merely because
Windows takes a different path. Safety changes need adversarial cases and the
fuzz checks in `AGENTS.md`. Preserve audit records for tool execution and policy
decisions when adding or changing execution paths.

### Persistence and internal services

Storage, session state, replay files and tool writes have distinct durability
contracts. [internal/fsutil](pkg/agentscope/internal/fsutil/) provides atomic
replacement, but not every writer uses it. Check the actual writer before
claiming atomic persistence. Review copying and ownership of nested maps, slices
and pointers when state crosses goroutines or is restored from a checkpoint.

[internal/httpsec](pkg/agentscope/internal/httpsec/) contains shared server
hardening helpers. Inspect authentication, body limits, timeouts and shutdown at
the actual handler/server boundary. Reuse existing helpers where appropriate;
the existence of a helper is not proof that an endpoint uses it.

### Retrieval and task-quality evaluation

[Typed Qdrant filters](pkg/agentscope/rag/metadata_filter.go) address literal
metadata keys; query constraints are ANDed with copied host conditions before
embedding and topK selection. [Retrieval contracts](docs/retrieval.md) document
reserved fields, numeric precision and cache semantics.

[evalkit load execution](pkg/agentscope/replay/evalkit/load.go) joins scheduled
arrivals with versioned task-quality results. Execution snapshots and workspaces
have a single owner. Wait for per-turn core completion before transferring them;
the public ReplyStream forwarder may close earlier on cancellation. Scoring has
its own contexts and worker limits. Never inspect an old callback context during
later scoring to revise accepted execution, or let late workers write a returned
report. See [managed inference contracts](docs/design/managed-inference.md) for
the implemented/proposed path matrix.

## Go conventions

- Use `context.Context` as the first argument for blocking operations. Preserve
  parent cancellation through goroutines, retries and outbound requests.
- Return errors for operational failures. Preserve documented constructor
  contracts, including the programmer-error panics in `message.NewMsg` and
  `agent.NewUnifiedAgent`; do not add new panic-based APIs casually.
- Prefer small interfaces, existing embeddable defaults and functional options.
  Preserve source compatibility and document meaningful behavior changes.
- Wrap errors with context while preserving `errors.Is`/`errors.As` matching.
  Use the structured errors and sentinels in
  [errors/](pkg/agentscope/errors/); `AgentError.Is` matches by code.
  `model.IsRetryableError` accepts a typed `Retryable=true` and otherwise falls
  back to error-text matching; `false` alone does not prevent a retry. Use
  `AgentMessage` where an error supplies a model-facing explanation.
- Log through `agentscope.Log()`. Choose levels for the caller's operational
  needs and avoid logging the same retry or failure at several layers.
- Make resource ownership clear: who closes a channel, response body or file;
  who cancels a worker; and whether returned state is shared or copied.
- Keep tests deterministic where possible. Inject transports, clocks or fixtures
  at existing seams instead of requiring real API keys, hardware or long waits.

## Test navigation

| Change | Relevant tests or helpers |
|---|---|
| Agent rounds, recovery or compression | Adjacent `agent/*_test.go`, [agenttest/](pkg/agentscope/agenttest/) |
| Lifecycle events | [event/streamcheck/](pkg/agentscope/event/streamcheck/) and agent stream tests |
| Provider requests, usage and streaming | `model/*_test.go`, [providercontract/](pkg/agentscope/providercontract/) |
| Deterministic replay | [replay/](pkg/agentscope/replay/), [agent/golden_test.go](pkg/agentscope/agent/golden_test.go) |
| Task-level evaluation | [replay/evalkit/](pkg/agentscope/replay/evalkit/) |
| Load generation and scheduled-arrival outcomes | [bench/](pkg/agentscope/bench/), [load-testing guide](docs/benchmarks.md) |
| Tool safety or backend routing | Adjacent `tool/*_test.go`, `workspace/*_test.go` and parser fuzz targets |

`providercontract` is test support and imports `testing`; do not add it to
production dependency paths. Inspect its registered harnesses before claiming a
provider is covered. Golden tapes can be regenerated with
`go test ./pkg/agentscope/agent -golden-update` after an intentional behavior
change; review the resulting diff, rather than accepting a new golden file as
proof of correctness.

Use the commands and coverage policy in `AGENTS.md` for final validation. Do not
copy package counts, coverage snapshots or release inventories into this map;
link to the relevant source or measure them when needed.
