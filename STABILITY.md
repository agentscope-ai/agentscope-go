# API Stability & Production Hardening

This document states the stability guarantees of `agentscope-go` and tracks the
production-hardening status of the library.

## Versioning

The module path is `github.com/agentscope-ai/agentscope-go/v2`. The latest release
tag is `v2.0.11`, the first tag declaring the community module path. Earlier tags
use `github.com/alanfokco/agentscope-go/v2`; upgrading requires the
[import-prefix migration](CHANGELOG.md#changed--repository-and-module-path-move).
Consumers import as:

```go
import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
```

## Stability tiers

- **Stable** (source-compatible within a major version): `message`, `model`,
  `agent` (UnifiedAgent), `tool`, `permission`, `formatter`, `errors`.
- **Experimental** (may change): `inference`, `runtime`, `loop`, `app`, `service`, `realtime`,
  `tune`, `replay/evalkit`, `event/streamcheck`, `agenttest/faults`,
  `providercontract` (test-only), `console`, `channel`, `channel/dingtalk`,
  `hub` (built-in sources), `skill` (`Store` partitions), `middleware/memory`
  (`FileStore` + `AgenticMemoryMiddleware`).
  These are the newer v3 infrastructure layers, harness tooling, the
  console/channel frontends, and the Phase 3 hub/skill/memory additions.
- **Internal** (`internal/...`): no compatibility guarantee; do not import.

Packages not listed above are unclassified. Treat them as Experimental until
they are graded. That list includes `workspace`, `schedule`, `rag`,
`rag/parser`, `middleware`, `storage`, `mcp`, `embedding`,
`audit`, `device`, `metrics`, `replay`, `bench`, `wasm`, `hotreload`, `a2a`,
`messagebus`, `pipeline`, `session`, `prompt`, `team`, `webui`, `access`,
`resilience` and `tracing`. The 2026-09 sync batch added exported API to five of
them: `workspace.ExecPathResolver`, `schedule.TaskRemover`, `rag.Document.Score`
and `rag.NewLLMReranker`, `rag/parser` `Unit` plus `Validate`, and `middleware`
`WithRepetitionErrorThreshold` plus `WithRepetitionErrorHint`.

Stable means source-compatible for documented usage. It does not mean
value-compatible. Two Stable-tier contracts changed behavior in the 2026-09 sync
batch without changing a signature, so neither breaks compilation; both are
recorded as `BREAKING (behavior)` in `CHANGELOG.md`.
`message.ToolResultBlock.Output` may now hold a `[]message.ContentBlock`, so read
it with `GetOutputText()` or a comma-ok assertion, since a bare `.(string)`
panics on an image tool result. `tool.ReadCache.Get*` return copies, so mutating
the returned entry no longer affects the cache.

## v2.0.11 behavior and compatibility

- `tool.AskUserTool()` is an opt-in external tool. Its typed question/answer
  values and the optional `InputValidator` and `ExternalResultValidator`
  interfaces add APIs without changing the required `Tool` interface.
  Use `UnifiedAgent.ReplyStream` with a permission context and a host that handles
  external execution and permission confirmation. `Reply`, the loop bridge and
  the stock console do not supply an AskUser UI. `ModeDontAsk` denies AskUser;
  explicit permission rules remain effective in other modes.
- Restored submitted tool calls now check the current toolkit, permissions and
  input before handoff. Calls whose tool is missing, inactive or no longer
  external become error results. Invalid successful answers become error
  results; `SubmitExternalResult` still returns no error. Hosts should inspect
  the terminal event and must not mutate submitted results afterward.
- External result metadata survives agent state and event reconstruction.
  Multi-block external outputs retain all blocks in recorded state; supported
  text/data events preserve their order, subject to the existing event data cap.
  A normal single-text result retains its string representation; resumed
  results retain their submitted representation. `GetOutputText()` returns only
  the first text block of a block list; inspect `Output` to read every block.
- Anthropic/Gemini formatters omit empty text and hints while retaining supported
  hint media and sender names after filtering. Whitespace is preserved. Gemini's
  model adapter separately omits empty ordinary/system text; it does not gain
  general multimodal hint support.
- `console.Launch` returns `context.Canceled` or `context.DeadlineExceeded` when
  its caller context ends at a prompt, confirmation or active reply. SIGINT
  during a reply still interrupts only that reply. A caller-owned reader blocked
  on I/O may outlive Launch; its owner must close it to release the read.

The [upstream comparison](docs/design/upstream-sync-v2.0.11.md) records the selected
ports and deferred work, including realtime/TUI, SOP and model-context wiring.

## Unreleased execution and retrieval behavior

- Synchronous `Reply` checks caller cancellation after draining events and again
  before returning state, and only selects the current reply ID. Cancellation
  returns a nil message and a recognizable context error; earlier replies are
  never returned as the new response. Partial recorded state is retained.
- Agent retry/fallback waits honor cancellation in both UnifiedAgent and the
  loop bridge. This does not control hidden retry policies in custom wrappers.
- Tool-result wire rendering retains all text and bounded unsupported-media
  placeholders, with native image support preserved in Responses. This changes
  requests previously missing later blocks; `GetOutputText` retains its legacy
  first-text behavior. Token estimates include all tool-result text.
- Toolkit registration owns the slice container; callers cannot replace a
  registered tool by changing their input slice. Tool objects are still shared.
- Grep path ordering and per-file limits are stable across pages for an unchanged
  directory. Blank chunk windows are omitted; nonblank text is not trimmed.
- Embedding file caches skip oversized entries before eviction and atomically
  replace accepted JSON entries. A skipped same-key write returns nil and keeps
  the older value. Use content-addressed keys and treat writes as best effort.
- Qdrant adds typed metadata filtering without changing required Index methods.
  The experimental `evalkit.Runner.RunLoad` joins versioned tasks with scheduled arrivals
  and separate scoring. Existing TaskSpec JSON and YAML field names remain unchanged.
  Execution failures cannot pass scoring, and workspace cleanup waits for actual
  core/tool completion; uncooperative code can delay `RunTask` cancellation.

See [retrieval](docs/retrieval.md), [quality/load testing](docs/benchmarks.md) and
[the delivery contracts](docs/design/managed-inference.md) for API and lifecycle
limits. Managed admission is available through explicit constructors; model
routing remains proposed.

## Unreleased managed inference behavior

- `model.NewManagedChatModel` and `embedding.NewManagedEmbeddingModel` bind
  supported built-in adapters to a host-owned deployment and shared admission
  pool. Required model interfaces and unmanaged defaults stay unchanged.
  Managed calls require trusted tenant identity, own physical retries and reject
  hidden fallback wrappers, custom transports and redirects. See the
  [adapter support table](docs/managed-inference.md#supported-adapters).
- Deployment context windows participate in optional `ContextSizer` resolution;
  explicit agent context overrides still win. Provider-qualified model-card
  lookup returns owned metadata and does not validate live serving limits.
- Positive embedding `MaxConcurrency` bounds batch workers. Managed cache keys
  include tenant and deployment identity; unmanaged cache keys retain their
  format. Use distinct stable deployment IDs for distinct serving configurations.
- Physical attempt snapshots expose missing usage/prices and retention loss.
  `CostLedger` can project this source without counting logical records twice;
  `RunLoad` can attach matching attempts and scoring attribution to its report.
- `WithReplyRecovery` opts UnifiedAgent into typed built-in reply budgets,
  explicit `ResumeReplyStream`, fail-stop persistence and exclusive reply state
  until actual core completion. Middleware may invoke its reply handler at most
  once; calls after its output closes are rejected. Pending tools retain their
  unfinished iteration on resume and pass current validation.
- Ordinary checkpoints keep schema 1; recovery checkpoints use schema 2 and
  budget snapshot version 1. Older loaders reject schema 2. New loaders accept
  older conversation state, but explicit recovery requires its typed snapshot.
  Use a pre-recovery checkpoint or fresh conversation when rolling back; do not
  remove counters to bypass resume validation. `SaveCheckpoint` returns errors;
  legacy `Checkpoint` still logs them. Recovery does not provide exactly-once
  tool execution or a reservation against concurrent physical spending.

## Error handling

Errors are structured `*errors.AgentError` (category + code + retryable +
optional `RetryAfter`) and are `errors.Is` / `errors.As` compatible. Match
sentinels such as `errors.ErrModelRateLimited`, `errors.ErrToolDenied`,
`errors.ErrLoopMaxIters`, `errors.ErrBudgetExceeded`. `model.IsRetryableError`
honors the typed `Retryable` flag; `errors.RetryAfterOf` extracts a throttle delay.

## Construction contract

Constructors validate inputs and **panic on programmer error** (mirroring
`message.NewMsg`): e.g. `agent.NewUnifiedAgent` panics on a nil model or empty
name. These indicate misuse, not runtime conditions — fix the call site.

## Production hardening status

Landed:

- **Security:** bash read-only classification rejects write redirects (was an
  auto-allow bypass); curl/wget removed from the read-only allowlist; opt-in
  workspace-root jail for file tools; WebFetch SSRF guard (blocks
  loopback/private/link-local incl. cloud metadata); MCP subprocesses run with a
  minimal env instead of inheriting parent secrets; credentials kept out of URLs
  and redacted in logs.
- **Reliability:** retries honor 429/`Retry-After` with ctx-aware full-jitter
  backoff; ordered `FallbackChatModel` chain; single-probe half-open circuit
  breaker; per-tool timeout + result cap.
- **Concurrency:** SSE/loop/turn/session event sends are cancellation-aware
  (goroutine-leak fixes); manager/scheduler/task snapshots avoid torn reads;
  `permission.Engine` and `pipeline.MsgHub` are mutex-guarded.
- **Streaming:** long streams are no longer truncated by the client's whole-request
  timeout; `ChatResponse` carries `Error` and `StopReason`.
- **Durability:** `storage.FileStorage`, runtime file-session saves, and selected
  write paths use `fsutil.WriteFileAtomic` (temp + file fsync + rename). This is
  not universal: replay FileStore, local backend writes, and
  local Edit/MultiEdit/ApplyPatch still use `os.WriteFile`.
- **Budgets:** token and duration budgets are enforced on the loop path.
- **Ops:** HTTP servers set `ReadHeaderTimeout`/`IdleTimeout`/`MaxHeaderBytes` and
  cap request bodies; `/healthz` + `/readyz`; graceful `Shutdown`; reference
  Dockerfile.
- **Observability:** OTEL span attributes recorded; label-aware in-memory metrics;
  sandbox execution events (`tool_exec_start`/`tool_exec_end`/`tool_policy_denied`).
- **Audit:** structured `audit.Logger` interface with InMemory/File/Multi/Nop
  implementations; the orchestrator records every tool execution, permission
  denial, and sandbox policy decision.
- **Process isolation:** on Unix, child processes run in a dedicated process group
  (`Setpgid`); timeout kills that group, reducing orphaned children. This is not a
  process-count limit, and Windows does not use this process-group mechanism.
- **Interpreter attack detection:** `CheckInterpreterAttack` detects dangerous
  API calls hidden inside `python -c`, `node -e`, `perl -e` and similar. Eight
  interpreter binary names cover six languages (`python`, `python2` and `python3`
  count once). There are 27 dangerous-API patterns; Lua and PHP match only the
  three language-agnostic ones.
- **Sandbox policy checks:** the orchestrator applies selected name-based checks
  for built-in tools. These checks are not a complete isolation boundary:
  custom tools, alternate call-name casing, shell/network behavior, and resource
  limits need separate enforcement. Policy configuration alone does not route
  execution through `Sandbox.Execute`; use and validate a workspace backend.
- **Write hardening:** the local Write tool has a 10 MB input-size cap, atomic
  replacement via `fsutil.WriteFileAtomic`, and executable-extension
  bypass-immune ASK (.sh/.py/.exe etc.). Other file tools and backend paths
  have different persistence behavior.
- **Testing:** fuzzers for the safety parsers and JSON decoder; CI fuzz smoke +
  coverage.

- **Edge & Embedded Intelligence:** ConnectivityAwareModel (cloud/local routing
  via circuit breaker), PubSub interface + MQTT adapter (build tag: mqtt),
  Device framework (Serial/GPIO/CAN/I2C pure-Go drivers + DeviceTool + Watchdog +
  SensorMiddleware), cross-arch CI (arm64/arm/mips64le/riscv64), binary ~6MB.

### Upstream sync batch (2026-09, Python 8/14–9/7 window)

Per-PR mapping is in the commit bodies.

- **Scheduler correctness (#2442):** `POST /api/schedule` validates cron
  expressions before persistence (standard five-field cron with steps/ranges/
  lists/names plus legacy aliases); invalid or never-firing expressions are
  rejected with HTTP 400. The old parser silently folded anything unknown
  into an hourly interval.
  - Next-fire arithmetic is rebuilt from local calendar fields with
    `time.Date`, never `time.Truncate`. `Truncate` aligns to absolute
    (UTC-based) boundaries, so in zones with a non-hour UTC offset
    (Asia/Kolkata +5:30, Asia/Tehran +3:30, Australia/Darwin +9:30, ...) it
    lands on local `HH:30` and **no** expression can ever match.
  - The scan window is 8 years, so a valid leap-day expression
    (`0 0 29 2 *`) is accepted, including from a year whose next 29 February is
    beyond the usual four (2100 is not a leap year, so from 2097 the next one is
    2104). `0 0 30 2 *` is still rejected as impossible. It remains a window,
    not a proof of impossibility.
  - A DST fall-back makes an ambiguous local hour resolve back onto itself
    (`time.Date` always picks the first occurrence), so every step carries a
    monotonic fallback; `next()` cannot spin.
  - **BEHAVIOR CHANGE:** the legacy `@every_*` aliases are now real cron
    expressions on the minute grid, so their fire times shift (e.g.
    `@every_5m` created at 10:03 fires at 10:05, and `@every_12h` means
    00:00/12:00). Schedules persisted before this change keep their stored
    expression but fire on the new grid.
  - Re-arm and cancel are serialized on a per-record lock with a re-arm
    sequence number: a `Cancel` that lands while a re-arm is in flight also
    cancels the freshly armed task, `Get`/`List` return copies (the callback
    rewrites `TaskID`/`Status` from another goroutine while HTTP handlers
    marshal them), the record is published before `Schedule` so a scheduler
    that fires synchronously cannot degrade a cron to a one-shot, and a fired
    task is dropped through the optional `schedule.TaskRemover` so a
    per-minute chain does not accumulate one dead entry per fire.
  - Because `Get`/`List` return copies, `PATCH /api/schedule/{id}` goes through
    `SchedulerManager.Update`, which mutates under the entry lock. Patching the
    returned copy used to answer 200 with a body the store never saw, and
    silently lost the ability to pause a chain via `{"status":"paused"}` (the
    re-arm callback reads that field).
  - Status is one-way. Pausing, completing, failing or blanking the status stops
    the re-arm chain, and `Update` refuses to set `active` again from any
    non-active status, including an empty one, so two PATCH calls cannot reopen
    the path. Only a record that is currently active has an armed task behind it.
    The route answers 409 with the reason rather than 200 with a body that
    disagrees with the store. Resuming means creating a new schedule.
  - `CreateScheduleRequest.RunOnce` is honored: the expression is still
    validated, but the schedule fires once at the next matching slot and does
    not re-arm; after that fire the record reports `completed` and its task
    entry is dropped. It was previously accepted by the API and read by nothing.
  - The status field is not validated against a vocabulary, so
    `PATCH {"status":"canceled"}` marks the record canceled without canceling the
    armed task, and the next fire still runs one chat before the chain stops. Use
    `DELETE /api/schedule/{id}`, which calls `Cancel`, to stop a schedule.
    Restricting the field to `{active, paused}` is tracked as follow-up work.
- **Concurrency (#2476):** `WakeupDispatcher.Wakeup` sends under the registry
  lock, closing a send-on-closed-channel race with `Unregister`.
- **Provider accounting (#2461):** xAI output usage adds
  `completion_tokens_details.reasoning_tokens` (xAI excludes them from
  `completion_tokens`; other OpenAI-family providers include them and are
  untouched to avoid double-billing).
- **Stream truncation (#2350):** Anthropic streams ending without
  `message_stop` surface `ChatResponse.Error` instead of ending silently, and
  Anthropic mid-stream `error` events (`overloaded_error`, rate limits, an
  aborted generation) are parsed into that field rather than skipped — they
  arrive inside an otherwise successful HTTP 200 stream. OpenAI Responses
  streams that end without `response.completed` are reported the same way.
  The agent reply loop now consumes `ChatResponse.Error`: it logs a warning
  and emits a `model_partial_response` custom event, while still keeping the
  partial content (a truncated reply is usually more useful than none).
  Mid-stream **failover** is still not implemented: `FallbackChatModel` fails
  over on stream setup errors only, so a truncated stream is reported, not
  retried.
- **Responses reasoning replay (#2426):** OpenAI Responses reasoning items
  (incl. `encrypted_content`) are preserved on `ThinkingBlock.Extra` and
  replayed as native input items; multi-tool-call and multi-tool-result
  turns now convert completely (previously only the first survived).
  The STREAMING path (the agent's default) emits blocks in the same order as
  the non-streaming one — reasoning, then text, then tool calls — because
  replay follows block order and text-first would replay a reasoning item
  with nothing after it. Tool calls keep the order the API produced them in
  (a map range would randomize it). An empty replayable reasoning item is
  skipped rather than sent as `{}`, and a turn whose blocks the API cannot
  carry is emitted as an explicit placeholder instead of vanishing.
- **Gemini schemas (#2437):** nullable type arrays (`["string","null"]`) are
  sanitized; multi-type arrays become `anyOf`; a multi-type array beside an
  existing `anyOf` is left for the API to reject rather than silently
  dropping constraints.
- **Tracing (#2450):** chat spans record `gen_ai.response.finish_reasons`
  from the real response, marshaled as JSON (a `StopReason` containing a quote
  used to produce an invalid attribute value). The three failure shapes stay
  distinguishable: a canceled context reports `interrupted`, a transport or
  handler failure reports `error`, and a partial reply reported via
  `ChatResponse.Error` (#2350) reports `incomplete`.
- **Context images (#2362):** `ContextConfig.MaxImageNum` replaces the oldest
  images with text reminders (URL-backed images keep a pointer) before token
  counting. The rewrite is copy-on-write: `a.state.Context` shares its `*Msg`
  pointers (and nested tool-result block lists) with snapshots handed out
  earlier under the lock and read afterwards, so mutating them in place raced
  with concurrent history reads, checkpoint saves and serialization.
- **Prompt text (#2513):** default agentic-memory instructions no longer
  carry the malformed `</search>` tag.
- **Permission target shell (#2366 residual):** `permission.Context.TargetShell`
  pins the shell assumed by checks; the orchestrator and agent derive
  `"posix"` per call when a non-local workspace backend executes commands,
  so host-based PowerShell pattern checks no longer misfire on containers.
- **Read cache backends (#2092):** optional `tool.BackendStatter` lets the
  cache validate freshness against the backend's own filesystem
  (`workspace.ToolBackend` implements it via POSIX stat, trying the GNU
  `stat -c %Y` and BSD/busybox `stat -f %m` forms, with a lexical jail check
  before the path reaches a shell); backend writes and edits invalidate cached
  copies. Read→Edit now works in workspaces.
  - The path spelled into that shell command must name the SAME file `ReadFile`
    read, and the correct spelling is backend-specific, so it is delegated
    through the new optional `workspace.ExecPathResolver`. Docker, Daytona and
    AppleContainer resolve to an absolute in-sandbox path (`docker exec` has no
    `-w`, so a relative path would resolve against the image WORKDIR); K8s, E2B,
    OpenSandbox and bubblewrap pass the caller-relative path through, and for
    bubblewrap an absolute `BasePath`-joined path would be actively wrong
    because its `BasePath` is a HOST path that the sandbox sees as `/`.
    Backends that do not implement the interface get the relative form, which is
    the historical behavior. A wrong spelling is not just a wasted exec: it
    supplies a freshness key for the wrong file and the cache can then serve
    stale content as fresh.
  - The backend read path actually consults the cache. A stat round-trip costs
    at most one exec per read: on a miss it is paid once to obtain the mtime the
    entry is cached with, on a hit once to validate it. It is never paid purely
    to build a key nobody checks, which is what the first cut did.
  - A backend that does not implement `BackendStatter` caches nothing: a host
    `os.Stat` of a workspace-relative path is meaningless, or worse, matches
    an unrelated host file.
  - `GetCache`/`GetCacheWithMtime` return a copy. `removeAt` shifts elements
    inside the shared backing array, and `Remove` (called by Write/Edit) made
    the previously returned internal pointer race under concurrent tool
    batches.
- **Read images (#2114):** the Read tool returns image files as base64
  DataBlocks (host and backend paths). PDF page rendering is not ported
  (needs a rasterizer dependency).
  - Every image response also carries a leading text placeholder
    (`[shot.png: image/png, 12345 bytes]`). The agent's tool pipeline is
    string-based; without the placeholder a DataBlock-only response was
    flattened to an EMPTY tool result and the model concluded the file was
    empty.
  - The agent keeps the non-text blocks: `ToolResultBlock.Output` becomes a
    `[]message.ContentBlock` when a tool returns them, and stays a plain
    string otherwise, so existing consumers are unaffected.
  - Native rendering depends on the provider. The OpenAI **Responses** path
    converts image results into `input_image` parts (#2389). Chat
    Completions, Anthropic, Gemini and DashScope format tool results as text
    and therefore see the placeholder, not the pixels — there is no
    `model_input_types` capability probe yet (upstream has one), so Read
    cannot refuse images for a model that cannot see them.
  - Images above `tool.MaxInlineImageBytes` (256 KB) are reported as text
    instead of inlined. The token estimator decodes base64 back to raw bytes
    (`len*3/4`) and divides the total by four, so an inlined image costs roughly
    `fileSize/4` tokens: 256 KB is ~64k, and a 1 MB screenshot would be ~250k and
    trigger an immediate compression. Token counting now includes images carried inside
    a tool result.
  - The image is also emitted as a `tool_result_data_delta` event between the
    text delta and `tool_result_end`, so a consumer that rebuilds a message
    purely from the event stream (`Msg.AppendEvent`: channel gateways, the
    console renderer, replay tapes) ends up with the same content as the agent's
    own context. An event that would violate the #2370 source invariant is
    skipped with a debug log rather than emitted invalid, and the block stays on
    `ToolResultBlock.Output` either way.
  - Event payloads are capped at `agent.maxEventInlineDataBytes`, measured on
    the BASE64 form: 64 KB of base64 is roughly 48 KB of raw image, since
    base64 inflates by 4/3. An 800x600 capture of dense text or a photograph
    exceeds that easily, while a flat-colour UI capture may not. Above
    the cap the event is dropped with a debug log and the image still reaches
    the model through `ToolResultBlock.Output`. The cap exists because
    event volume is NOT bounded elsewhere: `middleware.NewRunJSONL` serializes
    every event verbatim, and the flight recorder's tail is capped by COUNT
    (`tailCap = 50`) with no byte accounting — `approxEntryBytes` covers
    messages/tools/response/error but not events, and `WithRecordSizeLimit`
    summarizes record messages only. Without the cap, one reply reading a
    handful of screenshots would write megabytes of run log and a
    multi-megabyte crash dump. The trade-off: stream-only consumers do not see
    images whose base64 exceeds 64 KB (they see the text placeholder), which
    they could not have rendered anyway. If a future consumer genuinely needs
    the pixels, move the bound to the recorder/run-log side rather than raising
    this constant.
- **Argument repair (#2496):** tool-call arguments get schema-guided type
  coercion on every call (`jsonx.RepairWithSchema` / `CoerceToSchema`):
  quoted numbers, stringified booleans, lone values for arrays, stringified
  objects. Keys are never dropped; uncoercible values go to validation as-is.
  `CoerceToSchema` is copy-on-write and RETURNS the coerced map — it never
  rewrites the caller's map, nested maps or slices in place. Strings that
  parse to a non-finite float (`"NaN"`, `"Inf"`, `"1e999"`) are left alone so
  validation rejects them loudly instead of a later `json.Marshal` failing.
- **Error streaks (#1816):** `RepetitionBreakerMiddleware` gained an
  independent error dimension — the same call failing `WithRepetitionErrorThreshold`
  times injects an error hint; the next identical failure aborts with
  `ErrToolRepetition`. Error-state responses (nil Go error) now feed the
  error streak instead of counting as successes.
- **Compression accounting (#2433):** compression model calls accumulate
  usage (`GenerateStructuredOutputWithUsage`) and the reply loop emits it as
  a model-call-end event so budgets and cost tracking include it.
- **Forced finalization (#2443):** exhausting the react budget triggers one
  tools-disabled summary call (hint + `tool_choice: none`) so replies end
  with text instead of silence; finished reason stays `exceed_max_iters`.
  `tool_choice: none` is a request, not a guarantee — local and proxy-backed
  providers (Ollama, vLLM, assorted OpenAI-compatible gateways) ignore it
  routinely — so any tool calls in that reply are dropped before the context
  save. Stored unexecuted calls would be picked up by the NEXT reply's
  `checkNextAction` and run as ghost tool calls.
  As a side effect the `exceed_max_iters` event is no longer emitted on the
  last loop iteration when that iteration finished normally.
  - The extra call is unconditional and there is no flag to disable it. Every
    reply that exhausts `ReactConfig.MaxIters` costs one more model call over the
    full context, which at that point is the longest it will be. It is emitted as
    `model_call_start` and `model_call_end`, so budget and cost middleware account
    for it, but a workload that routinely hits `MaxIters` has a higher per-reply
    cost. Raise `MaxIters` or shorten the context if that matters.
- **RAG scores (#2486):** `rag.Document.Score` carries retrieval relevance,
  normalized higher-is-better (Milvus L2 negated); ES/Qdrant/MongoDB populate
  it; `RerankedIndex` carries rerank scores onto documents.
- **LLM rerank (#1975):** `rag.NewLLMReranker` implements `Reranker` over any
  `ChatModel` via structured output, with per-(query,doc) score caching.
  Document excerpts are truncated by RUNE (`WithLLMRerankerDocChars`) so a
  multi-byte character is never split, and the whole judge prompt is bounded
  by `WithLLMRerankerPromptRunes` (default 24000) by shrinking the per-document
  budget — candidates are never dropped, because a missing candidate would be
  scored 0 and silently sink in the ranking. Document text is interpolated into
  the judge prompt as-is, so adversarial corpus content can still try to
  influence scores (inherited from the upstream design).
- **Chunker config (#2083 core):** `ChunkConfig` gained an explicit `Unit`
  (chars / approx_tokens) and `Validate()`; API layers can reject bad configs
  instead of silent fallback. App/KB pipeline and WebUI surfaces are not
  ported (Go has no index worker/KB routes).
- **Agent-driven compression (#2143):** `tool.NewCompressContextTool` +
  `agent.WithAgentDrivenCompression()` register a `compress_context` tool the
  model can call itself.
  - The seam is `tool.CompressFunc`, returning a `tool.CompressionResult`.
    The tool reports honestly whether anything was actually summarized: below
    the threshold the reply says so, instead of claiming success and teaching
    the model that details it can no longer see are still in context.
  - The tool compresses at `ContextConfig.AgentDrivenTriggerRatio` (default
    `TriggerRatio/2`, clamped to at most `TriggerRatio`). Without a lower bar
    the automatic path always fires first and the model can never trigger it.
  - The tool is `ConcurrencySafe: false` (it rewrites the shared message
    history) and returns `permission.BehaviorAllow` so an ASK-mode permission
    engine does not stall every model-initiated compression.
  - The compression split never sweeps an unfinished tool call into the
    summarized portion, and never leaves a call on the opposite side from its
    own result (`pullSplitBackForToolPairs`, iterated to a fixpoint after
    `adjustSplitForToolPairs`). Both repairs move the split BACKWARD: pushing it
    forward — which is how an orphan result was repaired before — can drag an
    in-flight call into the summary and expose further split pairs. The
    `reserve_ratio=0` emergency fallback (context already over the window)
    still compresses everything; that trade-off is documented at the call site.
- **Native multimodal tool outputs (#2389):** Responses-API tool results
  carrying images become native `input_image`/`input_text` content parts
  instead of flattened text.

Recently landed (since initial hardening):

- ~~Full tool-in-sandbox execution.~~ **Done:** `workspace.ToolBackend` adapter
  (Workspace → `tool.Backend`); Bash, read, write, and edit all route through a
  configured backend using workspace-relative paths. Wire with
  `tool.WithBackend(ctx, workspace.NewToolBackend(ws))` for real Docker/E2B
  isolation; the rich local path (streaming/cwd/read-cache) is the default.
- ~~Prometheus exporter + `/metrics` + trace-context propagation~~ **Done:**
  `metrics/prometheus` provider; trace integration through middleware/hooks. The current `loop.Hook` methods
  do not accept `context.Context`; caller-context propagation is API-specific.
- ~~JSON-Schema input validation~~ **Done:** `tool.ValidateInput` with fuzz target.
- ~~Module `/v2` decision~~ **Done:** module path is now `/v2`.

- **USD/CNY cost tracking:** `WithMaxCostUSD` rejects subsequent calls after
  already-accounted cost reaches the threshold (`ErrBudgetExceeded`). It does
  not reserve or estimate the next call's cost; individual or concurrent calls
  may overshoot. Missing usage/prices prevent complete accounting.
  `WithExchangeRate("CNY", 7.2)` converts the tracked total for display.
- ~~Record/replay eval harness~~ **Done:** `replay.Scorer` interface with 5
  built-in scorers (ExactMatch, Contains, JSONField, TextContains, Composite),
  `EvalTape()` runner producing `EvalReport`, and `AssertTape(t, ...)` go-test
  helper for regression testing agent behavior against recorded tapes.
- ~~Anthropic prompt-caching write path~~ **Done:** `AnthropicConfig.PromptCaching`
  + `applyPromptCaching()` + cache token tracking in both streaming and
  non-streaming paths. (Needs live-API integration test.)
- ~~`exception`→`errors` migration (Phase 1)~~ **Done:** Tool error types
  (`ToolNotFoundError`, `ToolJSONDecodeError`, etc.) moved to `errors/`;
  `errors.AgentError` gained `AgentMsg`/`AgentMessage()` for LLM-facing messages;
  `exception` aliases were a transitional step; the package has since been deleted.
- ~~`SecretStr.UnmarshalJSON`~~ **Done:** `SecretStr` can now be populated from
  JSON config files; value is stored internally, never re-exposed via Marshal.

- ~~`RedisFullStorage`~~ **Done:** full `FullStorage` implementation over Redis,
  using the existing `RedisClient` interface (no hard dependency on go-redis).
  All 28 methods implemented: credentials, agents, sessions, schedules, messages,
  teams. Message ordering preserved via per-session index key.
- ~~`SecretStr` dual-field (Phase 2)~~ **Done:** all 8 model provider configs
  now carry `SecretAPIKey SecretStr` alongside deprecated `APIKey string`.
  Constructors use `ResolveAPIKey()` to prefer the secret field. TTS/embedding/
  workspace configs were added in a later adoption pass listed below.
- ~~`exception` package removal (Phase 2)~~ **Done:** all 4 importing packages
  (`agent`, `tool`, `tool/orchestrator`, `tool/orchestrator_test`) migrated to
  `errors/`. Zero imports of `exception` remain outside the alias package itself.
  The temporary alias layer was removed in the final deletion step below.
- ~~OTLP setup helper~~ **Done:** shipped as `examples/tracing_otlp/` with a
  documented wiring pattern. No OTel SDK dependency added to the library.

- ~~`exception` package deletion (final)~~ **Done:** package removed entirely.
  All code uses `errors/` directly.
- ~~`SecretStr` adoption in TTS/embedding/workspace/hub configs~~ **Done:** all 14
  remaining config structs gained `SecretAPIKey model.SecretStr` with
  `ResolveAPIKey()` in constructors. Full coverage across the entire framework.
- ~~Output guardrails~~ **Done:** `GuardrailMiddleware` with 3 actions
  (Block/Redact/Warn), rule-based content filtering on model responses. Built-in
  rules: KeywordBlock, KeywordRedact, MaxLength, Custom. Hooks into `OnModelCall`.

- ~~Phase 3 porting batch~~ **Done:** built-in hub sources
  (`hub.GitHubMCPRegistry`, `hub.ClawHub`); per-agent workspace skill
  partitions (`skill.Store`, `/api/workspace/skill` routes implemented);
  agentic memory (`memory.FileStore` + `memory.AgenticMemoryMiddleware` +
  `app.WorkspaceAgentFactory` hook); session↔workspace sharing with
  refcounts + read-only artifact endpoints (`/api/workspace/share`,
  `/api/workspace/{id}/list_dir|read_file`). Path jail hardened:
  `LocalWorkspace` containment is separator-aware and symlink-aware;
  `BubblewrapWorkspace` containment is separator-aware.
  All code batches passed evaluator adversarial review (no HIGH findings).

## Deliberately not ported (2026-09 sync batch)

Upstream PRs triaged in the Python 8/14–9/7 window and intentionally skipped.
Each entry states the blocker, not a priority.

| Upstream | Item | Why it is not here |
|---|---|---|
| #2428 | GoalPipeline | No Go goal/objective layer to hang it on; it would be a new subsystem, not a port |
| #2386 / #2379 | team enhancements | `team/` is leader/worker only; the enhancements assume Python's team event model |
| #2142 | full A2AAgent protocol | Needs an SDK decision first: hand-rolled types in `a2a/` versus depending on an A2A Go SDK |
| #1755 | workspace prewarm pool | Prerequisites missing: isolation policy, and a workspace storage/lifecycle contract |
| #2311 | MCP SSE transport | `mcp.HttpClient` is request/response JSON-RPC only (`mcp/http.go`). An SSE or streamable-HTTP transport is a separate project |
| n/a | channel and app Phase E items | Frontend work, tracked separately |

Mid-stream failover is also absent: `FallbackChatModel` fails over on stream
setup errors only, so a truncated stream is reported (#2350) and not retried.

Nothing wires model capabilities into the tool layer, so `Read` cannot decline to
return an image to a model that cannot see it. The card data exists
(`ModelCard.InputTypes`, `ModelCard.SupportsImages()`), but the lookup path does
not: `model.ResolveContextSize` reaches a card through the optional `ModelNamer`
interface, which only `openaiResponseModel` and `FallbackChatModel` (by
delegation) implement. The other eight providers implement neither `ModelNamer`
nor `ContextSizer`. Wiring this means adding `ModelName()` to those eight first.

## Open hardening work

- **Unify the two price types.** `middleware.ModelPrice`
  (`InputPerMillion`/`OutputPerMillion`/`CacheReadPerM`/`CacheWritePerM`, used by
  `NewCostTrackerMiddleware`) and `model.Price`
  (`Input`/`Output`/`CacheRead`/`CacheWrite`, used by `NewCostTracking` and the
  `model.ResolvePrice` overlay) describe the same thing with different names, so
  the two cost middlewares cannot share one map and `docs/middleware.md` has to
  warn readers about it. Pick one and alias or migrate the other.

- Complete sandbox enforcement across custom tools, call-name aliases, shell
  execution, network allowlists, and resource limits.
- Isolate nested session-state objects and provide an explicit restore lifecycle.
- Extend atomic persistence to the remaining direct-write paths.
- Define cost reservation/accounting if a strict monetary ceiling is required.

See [execution and session hardening](docs/adversarial-hardening.md) for the
changes and limits established by PRs #4 and #5. Historical completed entries
above describe individual features, not a claim that these open items are solved.
