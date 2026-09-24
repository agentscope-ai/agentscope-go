# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

Version sections below correspond to git tags from `v2.0.4` onward. `[v2.0.3]`
predates the tagging convention and has **no** tag, so it cannot be verified with
`git log` — treat it as historical narrative. Per-version details for tagged
releases can be verified with `git log <prev-tag>..<tag> --oneline`.

## [Unreleased]

### Added

- Opt-in managed inference with immutable deployment bindings, adapter capability
  and host policy metadata, shared FIFO admission, one physical retry owner and
  bounded attempt/operation snapshots. Direct streams hold permits through local
  cleanup; summaries and structured repairs retain operation attribution.
- Bounded embedding workers and per-batch shared admission for supported text
  and DashScope multimodal adapters, with tenant/deployment cache namespaces.
- Explicit UnifiedAgent reply recovery with owned, versioned built-in budget
  counters, remaining iterations and observable checkpoint failures. Legacy
  checkpoint writes remain schema 1; opt-in recovery writes use schema 2.
- Physical-attempt projections in the cost ledger and task-quality/load reports,
  including separate scoring attribution and explicit incomplete accounting.
- Provider-qualified model-card lookup and twelve metadata cards from Python
  AgentScope #2727. Includes a runnable managed-inference example and guide.

- Typed Qdrant metadata equality/range filters, applied before topK through
  `QueryVector` and `QueryWithFilter`, with copied host-required conditions.
- Experimental `evalkit.Runner.RunLoad`: versioned task/arrival manifests,
  independent bounded scoring, retained workspaces, immutable joined outcomes
  and quality goodput. Includes offline quality/load and local Qdrant examples.

- Experimental `bench.Runner.RunOpenLoop` for finite arrival schedules, bounded
  callback concurrency, and per-arrival outcomes including rejection, timeout,
  cancellation, unfinished work, and arrivals not offered before interruption.
  Reports retain scheduled-arrival latency and remain unchanged after return.
  This supplies the first load-generation building block for RFC #11.

### Fixed

- Synchronous agent replies propagate cancellation and select only the current
  reply; retries and fallback stop after cancellation in both execution entries.
- Qdrant ingestion no longer panics by passing `[]float32` vectors as payload
  metadata. It validates vectors, stores them in the native vector field and
  returns unsupported metadata errors before submitting a batch.
- Evaluation rejects failed, partial, canceled or incomplete replies before
  scoring, preserves multi-turn usage, and waits for tool completion before
  workspace cleanup.
- Tool-result requests retain all text and bounded media placeholders; token
  estimates include later tool text. Responses retains native tool images.
- Toolkit registration copies caller slices. Grep sorts by path and keeps its
  per-file limit independent of pagination. Text chunking drops blank windows.
- Embedding file caches skip individually oversized writes without evicting valid
  entries, handle size-limit overflow, and atomically replace accepted JSON files.
  Skipped same-key writes intentionally preserve the old value.

- CI fuzz smoke uses execution-count budgets to avoid spurious Go 1.25 deadline
  failures, with a separate 10-minute timeout for the combined fuzz step.
- OpenAI-compatible chat formatting preserves assistant tool calls, every tool
  result and subsequent replies in merged agent history (#7). This fixes requests
  that contained a tool result without its preceding assistant `tool_calls`.
- Multi-agent formatting keeps sender attribution after message expansion and
  preserves message boundaries carrying tool calls, reasoning or media.
- `model.WithMaxTokens` now reaches Ollama, DashScope, DeepSeek, Moonshot and
  xAI as `max_tokens` (#8). The shared OpenAI-compatible request struct had no
  JSON tag on the field, so those five adapters sent a supplied limit as
  `"MaxTokens":<value>`, a key no server reads, and every request from the
  family (OpenAI included) carried a stray `"MaxTokens":null` when no limit was
  set. The `providercontract` wall now decodes the captured request and checks
  the max-tokens value at the provider's wire path on both `Chat` and
  `ChatStream` for providers with `MaxTokensKey` configured (Anthropic
  deliberately skips it: its adapter does not apply the per-call option), and
  registers Ollama and xAI, which were missing from the wall.

## [v2.0.11] - 2026-09-19

### Added

- Opt-in `tool.AskUserTool()` with typed questions and answers, semantic input
  validation and successful external-result validation. Hosts collect responses
  through `UnifiedAgent.ReplyStream` and `SubmitExternalResult`; a permission
  context is required. The offline `examples/ask_user` uses a simulated model
  and answer. The stock console and loop runner do not supply an AskUser UI.
- Optional `tool.InputValidator` and `tool.ExternalResultValidator` interfaces,
  without changing the required `Tool` interface. AskUser rejects mismatched,
  duplicate or missing answers; invalid successful results become error results.

### Fixed

- External tool metadata now survives recorded agent state, terminal events and
  message reconstruction. Multi-block external output retains all text and data
  in state, including on checkpoint resume; supported events keep their order.
- Restored submitted calls now check the current active external tool, input and
  permissions before handoff. A missing, inactive or no-longer-external tool
  produces an error result rather than prompting for permission or running locally.
- Anthropic/Gemini formatters omit empty text and hints while preserving supported
  hint media, nonempty whitespace and sender attribution after filtering. Gemini
  request construction also filters empty ordinary/system text before joining it.
  This does not add general multimodal hints to the Gemini model adapter.
- `console.Launch` returns caller cancellation/deadline errors at the input prompt,
  during confirmation and while consuming a reply, without waiting for the event
  producer to close. SIGINT during a reply still interrupts only that reply.

### Changed — repository and module path move

- **BREAKING (import path):** the repository and module moved from
  `github.com/alanfokco/agentscope-go/v2` to
  `github.com/agentscope-ai/agentscope-go/v2`. `v2.0.11` is the first tag declaring
  the new module path; `v2.0.4`–`v2.0.10` declare the former path.
- Change imports from `github.com/alanfokco/agentscope-go/v2/pkg/agentscope/...`
  to `github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/...`, then run:

  ```bash
  go get github.com/agentscope-ai/agentscope-go/v2@v2.0.11
  go mod tidy
  ```

  The path rename itself is mechanical. This release also changes behavior as
  listed above; see [STABILITY.md](STABILITY.md#v2011-behavior-and-compatibility).
  Avoid mixing old-path and new-path packages: their Go types are distinct.
- Deleted old-path tags `v2.1.0` and `v2.1.1` remain available from the Go proxy
  and can outrank `v2.0.x` for `@latest`. The existing retractions cannot correct
  that from the new module path. Consumers staying on the old path should pin
  a real tag such as `v2.0.10`; consumers migrating should use the new path above.

### Changed — documentation and contribution checks

- Reorganize the English and Spanish READMEs, installation and examples guides,
  contributor instructions and architecture map. Document the AskUser host
  contract, validation and current implementation limits.
- Add a library coverage gate, PR checklist and reproducible local coverage
  commands. Raise the initial 65.0% floor to 66.5% in CI, Makefile and contributor
  instructions; examples remain outside the library coverage measurement.
  Expand package-local stream reconstruction tests and print package coverage
  in CI, retaining coverage profiles for diagnosing platform differences.
- Correct release history: entries already present in the `v2.0.10` tag are
  grouped under that version below instead of remaining under `[Unreleased]`.

Upstream references, selected ports and deferred capabilities are recorded in
[the v2.0.11 design](docs/design/upstream-sync-v2.0.11.md).

## [v2.0.10] - 2026-09-09

### Added — upstream sync batch (Python 8/14–9/7 window)

Nineteen upstream PRs ported. The rest were triaged as not applicable or
deferred; `STABILITY.md` records the reason for each under "Deliberately not
ported".

- **Real cron scheduling** (`app/cron.go`, #2442): five-field cron with steps,
  ranges, comma lists, month and day names, and the `@` aliases is parsed and
  validated before a schedule is persisted. `POST /api/schedule` rejects an
  invalid or never-firing expression with HTTP 400 through the typed
  `*CronValidationError`. Previously every unrecognized expression, including
  standard cron such as `0 9 * * *`, was folded into an hourly interval and
  recorded as active. API note: the endpoint used to answer 201 for any string,
  so expressions it accepted and the new parser rejects now answer 400. That
  covers six-field and other Quartz forms (`0 0 9 * * *`, `?`, `L`, `W`, `#`),
  `@every_*` values outside `5m`/`10m`/`30m`/`1h`/`12h`, `@reboot`, and any
  string that is not five fields
- **`schedule.TaskRemover`**: optional `Scheduler` capability for dropping a
  task entry that has already fired. `InMemoryScheduler` implements it. A cron
  chain re-arms by scheduling a fresh one-shot task per fire, so without a
  removal path a per-minute schedule accumulated one dead map entry and one
  `context.CancelFunc` per fire for the life of the process
- **`workspace.ExecPathResolver`**: optional capability returning the path
  spelling a backend's `Execute` resolves, so `ToolBackend.StatFile` measures
  the same file `ReadFile` read. `DockerWorkspace`, `DaytonaWorkspace` and
  `AppleContainerWorkspace` implement it and return an absolute in-sandbox path.
  LocalWorkspace, K8s, E2B, OpenSandbox and bubblewrap do not implement it: their
  `Execute` uses the caller-relative path, and bubblewrap binds its host root to
  `/`
- **`tool.BackendStatter`**: optional `Backend` capability (`StatFile`) letting
  the read cache judge freshness against the backend's own filesystem instead of
  the host's (#2092). `workspace.ToolBackend` implements it with POSIX `stat`,
  trying the GNU `-c %Y` and BSD/busybox `-f %m` forms, behind a path
  containment check
- **Read returns images** (`tool`, #2114): `.png .jpg .jpeg .gif .webp .bmp
  .tiff .tif .ico` come back as base64 `DataBlock`s on both the host and backend
  paths, preceded by a text placeholder such as `[shot.png: image/png, 12345
  bytes]` so consumers that only read text still get a meaningful result. New
  `tool.MaxInlineImageBytes` (256 KB): above it the image is reported as text
  rather than inlined, because the token estimator decodes base64 back to raw
  bytes and divides by four, making an inlined image cost roughly fileSize/4
  tokens. PDF page rendering is not ported; it needs a rasterizer dependency
- **Agent-driven compression** (`tool.NewCompressContextTool`,
  `agent.WithAgentDrivenCompression`, #2143): registers a `compress_context`
  tool the model can call itself
- **`ContextConfig.MaxImageNum`** (#2362): the oldest context images beyond the
  limit are replaced with text reminders before token counting. URL-backed
  images keep a pointer
- **`ContextConfig.AgentDrivenTriggerRatio`**: the threshold at which the
  model-invoked `compress_context` may compress. Defaults to `TriggerRatio/2`
  and is clamped to at most `TriggerRatio`. At the automatic threshold the agent
  would always compress first and the model could never trigger the tool
- **`rag.LLMReranker`** (`NewLLMReranker`, #1975): a `Reranker` over any
  `ChatModel` using structured-output judging, with a per-(query, document)
  score cache. Options are `WithLLMRerankerDocChars` (truncates by rune, so a
  multi-byte character is never split), `WithLLMRerankerCacheMax`,
  `WithLLMRerankerPrompt`, and `WithLLMRerankerPromptRunes`, which bounds the
  whole judge prompt by shrinking the per-document budget rather than dropping
  candidates (a dropped candidate scores 0 and sinks in the ranking)
- **`rag.Document.Score`** (#2486): retrieval relevance, normalized so higher
  means more relevant across Elasticsearch, Qdrant, MongoDB and Milvus. Milvus
  L2 distance is negated, so callers that negated it themselves must stop.
  `RerankedIndex` overwrites the field with the rerank score
- **`ChunkConfig.Unit` and `Validate()`** (`rag/parser`, #2083 core): explicit
  `ChunkUnitChars` / `ChunkUnitApproxTokens` so an API boundary can reject a bad
  config instead of relying on a silent fallback
- **`model.GenerateStructuredOutputWithUsage`** (#2433): accumulates usage
  across structured-output strategy attempts. Compression usage is recorded and
  emitted as a model-call-end event so budgets and cost tracking include it
- **`permission.Context.TargetShell`, `Engine.CheckPermissionInContext`,
  `tool.BackendPermissionContext`** (#2366 residual): shell-specific permission
  checks follow the execution backend, so host-detected PowerShell patterns no
  longer misfire on POSIX containers
- **`tool_result_data_delta` emission**: an image tool result is now also emitted
  as an event between the text delta and `tool_result_end`, so a consumer
  rebuilding a message from the stream alone (`Msg.AppendEvent`: channel
  gateways, console renderer, replay tapes) matches the agent's own context. The
  event type and merge path already existed; nothing produced them
- **`SchedulerManager.Update`**: mutates a record under its entry lock and
  returns a copy. `Get` and `List` now return copies, so patching their result
  has no effect
- **Deployment scheduling documentation** (`docs/deployment.md`): the HTTP
  schedule API, the accepted cron syntax, timezone behavior (expressions are
  evaluated in the process's `time.Local` and the API has no per-schedule
  timezone field, so pin `TZ` in the container), non-hour-offset and DST
  behavior, `DELETE` versus `PATCH` for stopping a schedule, and what a custom
  `Scheduler` implementation needs to know

### Changed — upstream sync batch

- `tool.NewCompressContextTool` takes a `tool.CompressFunc`
  (`func(ctx) (tool.CompressionResult, error)`). This is new in this release: the
  tool, `CompressFunc` and `CompressionResult` did not ship in any tagged
  version. The tool reports whether anything was actually summarized, and says
  so when the context was below the threshold, instead of claiming success on a
  no-op. It is `ConcurrencySafe: false` because it rewrites the shared message
  history, and returns `permission.BehaviorAllow` so an ASK-mode engine does not
  stall every model-initiated compression
- **BREAKING (behavior)** `cron_expr` is now parsed as cron. The previous parser
  recognized only `@hourly`, `@daily`, `@every_5m`, `@every_10m` and
  `@every_30m`; everything else, including `@every_1h`, `@every_12h`, `@weekly`,
  `@yearly` and any five-field expression, became an hourly interval. Recognized
  values became a `schedule.Interval` with no `RunAt`, and the in-memory
  scheduler runs an Interval task immediately, so every cron schedule also fired
  once at creation. Both halves changed. `@every_5m` created at 10:03 now fires
  at 10:05 rather than at 10:03 and then 10:08, `@every_12h` means 00:00 and
  12:00 rather than hourly, `0 9 * * *` means daily at 09:00 rather than hourly,
  and nothing fires at creation any more. Persisted schedules keep their stored
  expression and fire on the new grid; `docs/deployment.md` has the upgrade table
- **BREAKING (behavior)** `CreateScheduleRequest.RunOnce` was accepted by the API
  and read by nothing, so a schedule created with it re-armed like any other cron.
  It now fires once at the next matching slot and does not re-arm. A client already
  sending `run_once: true` therefore sees a repeating schedule become one-shot with
  no change to the request, the response, or the status code. The expression is
  still validated, since a malformed one is a client error either way. After that
  single fire the record reports `completed` and the spent task is dropped through
  `schedule.TaskRemover`; nothing marked a one-shot as finished before, so a listing
  could not tell a schedule that fires tomorrow from one that fired yesterday and
  never fires again. This also corrects the pre-existing no-`cron_expr` path, not
  only `run_once`
- **BREAKING (behavior)** `message.ToolResultBlock.Output` holds a
  `[]message.ContentBlock` when a tool returned non-text blocks, and a plain
  `string` otherwise. The field was already typed `any` and `GetOutputText()`
  already existed, so this compiles unchanged, but a bare `.(string)` assertion
  now panics on an image tool result. Read the field with `GetOutputText()` or a
  comma-ok assertion
- **BREAKING (behavior)** `SchedulerManager.Get` and `List` return copies rather
  than live pointers, because the scheduler callback rewrites `TaskID` and
  `Status` from another goroutine while HTTP handlers marshal them. Code that
  mutated the returned record now writes to a throwaway copy and sees no effect,
  with no compile error and no panic. `PATCH /api/schedule/{id}` goes through
  `Update` and answers 409 when a status transition is refused, instead of 200
  with a body that disagrees with the store
- **BREAKING (behavior)** `ReadCache.GetCache` and `GetCacheWithMtime` return
  copies. The signatures are unchanged, so nothing fails to compile, but
  mutating the returned `*ReadCacheEntry` used to change the cache and now does
  not. `removeAt` shifts elements inside the shared backing array, and the new
  `Remove` (called by Write and Edit) made the previously returned internal
  pointer race under concurrent tool batches
- **BREAKING (behavior)** schedule status is one-way. Pausing, completing,
  failing or blanking the status stops the re-arm chain, and `Update` refuses to
  set `active` again from any non-active status, including an empty one, so two
  PATCH calls cannot reopen the path. Only a record that is active has an armed
  task behind it. Resuming means creating a new schedule. Sibling fields in the
  same patch are still applied
- **Tracing finish reasons** (#2450) are marshaled with `encoding/json`, because
  a provider `StopReason` containing a quote produced an invalid attribute value.
  The three failure shapes are now distinguishable: a canceled context reports
  `interrupted`, a transport or handler failure reports `error`, and a partial
  reply reported through `ChatResponse.Error` reports `incomplete`. All three
  previously reported `interrupted`. Dashboard note: an alert filtering on
  `interrupted` will see fewer hits and should match all three
- **OpenAI Responses streaming** (#2426) emits blocks in the same order as the
  non-streaming path: reasoning, then text, then tool calls. Replay follows block
  order, and emitting text first replayed a reasoning item with nothing after it,
  which is what #2426 exists to prevent. Tool calls keep the order the API
  produced them in rather than map order. An empty replayable reasoning item is
  skipped rather than sent as `{}`, and a turn whose blocks the API cannot carry
  is emitted as an explicit placeholder instead of being dropped
- **Schema-guided argument coercion** (`jsonx`, #2496) runs on every tool call,
  not only when JSON parsing fails. Internal note: `jsonx.CoerceToSchema`
  returns the coerced map and is copy-on-write, so it never rewrites the caller's
  map, nested maps or slices. `internal/` carries no compatibility guarantee (see
  `STABILITY.md`), so this is not a consumer-facing break
- **Backend reads consult the cache.** A backend that does not implement
  `BackendStatter` caches nothing, because a host `os.Stat` of a
  workspace-relative path is meaningless or matches an unrelated host file. A
  stat round-trip costs at most one exec per read: one on a miss to obtain the
  mtime the entry is cached with, one on a hit to validate it
- **Compression splitting** never sweeps an unfinished tool call into the
  summarized portion and never leaves a call on the opposite side from its own
  result (`pullSplitBackForToolPairs`, iterated to a fixpoint after
  `adjustSplitForToolPairs`). Both repairs move the split backward, because
  moving it forward can drag an in-flight call into the summary
- **`limitContextImages` is copy-on-write.** `state.Context` shares its `*Msg`
  pointers and nested tool-result block lists with snapshots handed out earlier
  under the lock and read afterwards
- **`RepetitionBreakerMiddleware`** (#1816) gained an independent error-streak
  dimension. Error-state responses no longer count as successes
- **xAI usage** (#2461) adds `completion_tokens_details.reasoning_tokens` for xAI
  only. The shared OpenAI-family parse path is untouched, so other providers
  cannot double-bill
- **Gemini schemas** (#2437): nullable type arrays such as `["string","null"]`
  are sanitized and multi-type arrays become `anyOf`. A combination that cannot
  be sanitized is left for the API to reject rather than dropped
- **`approxTokenChars` documentation corrected** (documentation only; chunking
  behavior is unchanged): four characters per token under-counts CJK, where one
  token is roughly one to one-and-a-half characters, so a chunk sized in
  `approx_tokens` comes out larger in real tokens than configured. The previous
  comment claimed the opposite
- **`.gitignore`** covers root-level example executables, since
  `go build ./examples/<name>` writes a binary of about 20 MB into the current
  directory, and the internal planning and review documents

### Fixed — upstream sync batch

Two kinds of fix appear below. The first group corrects behavior that shipped in
`v2.0.9` or earlier. The second group corrects defects this batch introduced and
adversarial review caught before the commit; those never shipped, and are recorded
because they explain the shape of the new code.

**Shipped in `v2.0.9` or earlier**

- **Anthropic mid-stream `error` events were skipped.** They arrive inside an
  otherwise successful HTTP 200 stream, so the only visible symptom was the
  misleading "stream ended without message_stop (truncated)". They are now parsed
  into `ChatResponse.Error`, and the agent reply loop reads that field, logging a
  warning and emitting a `model_partial_response` event. Nothing outside tracing
  read it before, so a truncated reply was indistinguishable from a complete one
- **OpenAI Responses streams that ended cleanly without `response.completed` were
  silent**: `Error` was only set on a scanner failure. Same class as #2350
- **`exceed_max_iters` was emitted on a successfully finished last iteration.**
  It is now emitted only when the reply ran out of budget
- **Agentic-memory default instructions** no longer carry the malformed
  `</search>` tag (#2513)
- **`WakeupDispatcher.Wakeup`** sends under the registry lock, closing a
  send-on-closed-channel race with `Unregister` (#2476 class)

**Introduced by this batch, fixed before the commit**

- **Cron would have been unusable in every timezone whose UTC offset is not a whole hour.**
  `next()` advanced with `time.Truncate`, which aligns to absolute UTC
  boundaries, so in Asia/Kolkata (+5:30), Asia/Tehran (+3:30), Asia/Yangon
  (+6:30), Australia/Darwin (+9:30), America/St_Johns (-3:30) and Asia/Kathmandu
  (+5:45) an hour boundary landed on local HH:30 and no expression could match.
  Schedule creation would have returned HTTP 400 for all of them. It now rebuilds from local
  calendar fields with `time.Date`. The test suite and CI both ran on whole-hour
  offsets and every test used `time.Local`, so nothing caught it
- **`next()` could have spun forever on a DST fall-back day.** `time.Date` resolves an
  ambiguous local hour to its first occurrence, so rebuilding hour+1 at 01:00 EST
  returned 01:00 EST again. Every step now carries an absolute-time fallback
- **`next()` could have returned a time in the past** on a fall-back day, because
  rebuilding from local fields can move backward by the offset span. It now steps
  until strictly after, so `Create` cannot hand the scheduler an already-due
  `RunAt`
- **A valid leap-day expression would have been rejected.** `0 0 29 2 *` fires at most once
  every four years and the first cut of the scan window was one year, so it was refused as never
  firing. The window is now eight years, which also covers the gap around a
  century year that is not a leap year: from 2097 the next 29 February is 2104
- **Canceling a schedule could have left it running.** A `Cancel` landing while a
  re-arm was in flight canceled only the task it had already seen, so the freshly
  armed one survived and ran one more chat after the user canceled. Re-arm and
  cancel are now serialized on a per-record lock with a re-arm sequence number,
  and the callback cancels a task armed during the race. The record is also
  published before `Schedule`, so a scheduler that runs the callback
  synchronously can no longer make the first fire see no record and degrade a
  cron to a one-shot
- **`PATCH /api/schedule/{id}` would have had no effect.** Making `Get` return
  copies without updating the handler left it mutating a local copy and answering
  200 with a body the store never saw, which also removed the ability to pause a chain
- **`forcedFinalSummary` would have stored tool calls that never execute.**
  `tool_choice: none` is a request rather than a guarantee, and Ollama, vLLM and
  many OpenAI-compatible gateways ignore it. The stored pending calls were picked
  up by the next reply and run as ghost tool calls (#2443)
- **A typed slice would have been corrupted by argument coercion.** `ValidateInput` accepts
  `[]string`, `[]float64` and `[]int` as arrays and Go callers pass those
  directly, but the coercion branch only type-asserted `[]any`, so anything else
  reached the lone-value wrapper and a list of N items became a one-item list
  containing the list
- **String-to-number coercion would have accepted non-finite values.** `strconv.ParseFloat`
  parses `"NaN"`, `"Inf"` and `"1e999"`, which cannot be marshaled back to JSON.
  The float64 branch already refused them; the string branch did not
- **`ToolBackend.StatFile` would have had no path containment check.** It interpolated the
  path straight into a shell command, which would have exposed the mtime and
  existence of any reachable file had it been wired ahead of `ReadFile`

### Not ported (reasons in `STABILITY.md`)

GoalPipeline (#2428), team enhancements (#2386, #2379), the full A2AAgent
protocol (#2142, pending an SDK decision), the workspace prewarm pool (#1755,
needs isolation-policy and storage/lifecycle prerequisites), MCP SSE transport
(#2311), and the channel and app Phase E items. Mid-stream failover is also
still unimplemented: `FallbackChatModel` fails over on stream setup errors only,
so a truncated stream is reported but not retried.

### Changed — evaluator-followup hardening (Phase 3 cleanup)
- **`skill.ErrInvalidInput` sentinel**: `Store.Add` validation failures
  (missing name / instructions) are typed; the add route maps them to 400
  via `errors.Is` instead of string matching; escaping `agent_id` now has
  HTTP-level 400 coverage on all skill routes
- **`skill.Store` YAML rendering escapes all C0 controls** (`\u00XX`),
  not just `\n`/`\r`/`\t` — a description with raw controls can no
  longer produce unparseable frontmatter
- **`memory.FileStore` atomic load**: an unreadable stream (e.g. an
  over-long line) fails the whole load and commits nothing instead of
  leaving partial data; multi-instance sharing caveat documented on the
  type
- **`workspace.ErrPathEscape` sentinel**: `LocalWorkspace` and
  `BubblewrapWorkspace` containment failures are typed; artifact-route
  status mapping uses `errors.Is`; `BubblewrapWorkspace` containment also
  became separator-aware (same sibling-prefix class as the Local fix)
- **`app.WorkspaceInfoResponse` gains `workspace_id`** (additive; the
  historical `session_id` field still carries the workspace ID)
- **`WorkspaceManager` does workspace creation outside the manager lock**
  (concurrent creators of the same ID adopt one instance); covered by a
  20-goroutine `-race` test
- **`hub` client-name sanitization collapses invalid-character runs**
  (exact Python `re.sub` parity: `a..b` → `a-b`)


### Added — workspace sharing + artifacts (Phase 3)
- **Workspace sharing** (Python #1951 semantics): `WorkspaceManager` now
  tracks session↔workspace bindings with refcounts — `GetOrCreate` binds a
  session to a private workspace named after itself, `Share` rebinds it
  onto a named workspace other sessions can join (workspace IDs validated
  against escaping), `BoundWorkspaceID` / `GetByID` / `RefCount` expose
  the binding state, and a workspace is released from memory when its
  last session unbinds (files persist on disk; the next reference
  recreates it over the same directory). The `workspace.Workspace`
  interface is unchanged — sharing lives entirely in the manager layer
- **Artifact endpoints** (Python #2187): `POST /api/workspace/share`
  binds a session onto a shared workspace; `GET /api/workspace/{id}/list_dir`
  and `GET /api/workspace/{id}/read_file` give read-only artifact access
  to live workspaces (workspace jail enforced; size checked through the
  directory listing BEFORE content is read — 10 MiB cap, `413` above;
  `nosniff` on responses)
- **`LocalWorkspace` path containment hardened**: the jail check is now
  separator-aware — the previous bare prefix match admitted sibling
  directories (`/tmp/ws123/...` passed for base `/tmp/ws1`), a hole the
  new HTTP-exposed artifact endpoints would have surfaced — and
  symlink-aware: contained paths are resolved through their links (and
  the nearest existing ancestor for not-yet-created targets) and
  re-checked, so a link planted inside the workspace cannot serve or
  receive content outside it; both covered by regression tests

### Added — agentic memory (Phase 3)
- **`memory.FileStore`**: the file-based long-term memory store completing
  the InMemory / Mem0 / Vector / File store set — memories persist as JSON
  Lines (`memories.jsonl`) under a directory (workspace-friendly);
  crash-tolerant loading (a torn final line is skipped), atomic rewrite on
  delete, append-friendly adds, and the same keyword search semantics as
  `InMemoryStore`
- **`memory.AgenticMemoryMiddleware`**: file-backed long-term memory where
  the LLM decides when and what to save (port of Python's
  AgenticMemoryMiddleware, #2263) — keeps a workspace-local Markdown memory
  store (`<workdir>/Memory/MEMORY.md`, directory configurable) and injects
  the Auto-Memory instructions (faithful port of the Python text,
  `{memory_dir}` substituted) plus a token-budgeted `MEMORY.md` snapshot
  (default 4000 tokens; `<<<TRUNCATED>>>` with a Read-offset reminder on
  overflow, empty-store placeholder otherwise) into the system prompt; the
  agent maintains the store with its regular file tools. Deliberate delta:
  the asynchronous LLM-driven relevance retrieval is not ported yet — the
  selection text is kept as `DefaultAgenticRetrievalInstructions` for the
  future port
- **`app.WorkspaceAgentFactory`**: optional workspace-aware agent factory
  (`AppConfig.WorkspaceAgentFactory`, requires `WorkspaceDir`) that hands
  the session's workspace to the factory when creating session agents —
  parity with Python's workspace-aware agent middleware factories, enabling
  per-session filesystem-backed middleware such as agentic memory

### Added — workspace skill isolation (Phase 3)
- **`skill.Store`**: per-agent skill partitions under a workspace
  (`skills/<agent_id>/<skill-dir>/SKILL.md`, Python #2283 semantics) —
  first access equips a partition from the `skills/.seed` template
  exactly once (the partition's existence is the marker, so a deleted
  seed skill stays deleted), idempotent migration of the pre-partition
  layout into the seed, content-based `Add` and directory-copy `AddDir`,
  `Remove` by skill name, `PurgeAgent`; agent IDs are validated against
  partition escaping (leading dots, separators), and only
  `<partition>/<dir>/SKILL.md` counts as a skill
- **`skill.SkillManager` agent partitions**: `RegisterForAgent` /
  `GetForAgent` / `ListForAgent` / `PurgeAgent` /
  `LoadAgentFromStore` / `FormatInstructionsForAgent` alongside the
  unchanged global registry; `skill.FilterByName` for session skill
  selection
- **Workspace skill routes implemented** (previously stubs):
  `GET/POST/DELETE /api/workspace/skill` now read and write the
  session's workspace partition (`session_id` + optional `agent_id`
  query parameters); `SessionRecord`/`SessionResponse` carry
  `active_skills` (settable via `SetActiveSkills` and the session PATCH
  route) so agent factories can assemble per-session toolkits

### Added — hub built-in sources (Phase 3)
- **`hub.GitHubMCPRegistry`**: GitHub's public MCP registry
  (`api.mcp.github.com`, `GET /v0/servers`) as a `hub.Hub` source —
  cursor pagination, client-side query filter (the registry has no
  search parameter), cards mapped from remote or package entries with
  sanitized model-safe names. Stdio commands follow the registry's
  `runtime_hint` (`npx`/`uvx`/`uv`/`docker`) with per-runtime arguments
  and `name@version` pinning for npx (Python `_RUNTIMES` parity);
  remote entries carry their auth headers, with `{TOKEN}` placeholders
  rewritten to `${TOKEN}` and registered as required install inputs.
  `Install` atomically writes the MCP client config as
  `<client-name>.json` (`{"<client-name>": <config>}`) preserving the
  install-input surface: `${KEY}` placeholders plus per-input specs
  (description / is_required / is_secret) embedded under `inputs`
  (Python #2230 semantics). Deliberate shape divergence from
  `MCPHub.Install`, which persists the upstream registry's raw body as
  `<cardID>.json` — each hub writes its own upstream's native install
  shape. Optional token only raises rate limits
- **`hub.ClawHub`**: the ClawHub skill registry (`clawhub.ai`) as a
  `hub.Hub` source — catalog (`/api/v1/skills`, cursor-paginated) or
  search (`/api/v1/search`, single page), owner-scoped card IDs
  (`owner/slug` whenever the record names an owner, because slugs are
  not unique — Python #2214). `Install` validates the slug against a
  safe charset, downloads and unpacks the ZIP archive into
  `<targetDir>/<slug>/` with zip-slip protection plus zip-bomb
  defense (64 MiB per-file and 512 MiB aggregate extraction caps,
  truncation detected rather than silently applied), and removes a
  freshly created destination directory when unpacking fails

### Added — reply lifecycle semantics (Phase 3)
- **`ReplyEndEvent` parity**: `FinishedReason` (completed / interrupted /
  exceed_max_iters / error) plus structured `Error` (`types.ReplyErrorInfo`,
  classified authentication/permission/rate_limit/invalid_request/upstream/
  connection/internal/setup/unknown); `NewReplyEndEventWithReason` /
  `NewReplyEndEventWithError` constructors; model-call failures now end the
  reply with reason `error` instead of a bare end (classified into
  rate_limit / connection / invalid_request / upstream / unknown);
  iteration exhaustion ends with reason `exceed_max_iters`
- **Swallowable `ReplyEndEvent`** (port of Python #2322): an `OnReply`
  middleware that receives a completed reply's `ReplyEndEvent` without
  forwarding it forces another reasoning-acting round (the iteration
  counter restarts, unblocking a swallowed exceed-max-iters end);
  interrupted ends cannot be swallowed; a busy-loop guard ends the reply
  when end events keep getting swallowed without any reasoning/acting in
  between. Middleware must forward `CustomEvent` values named
  `agentscope.*` (internal round-boundary sentinels)
- Console renderer prints error/interrupted ends; `Launch` no longer
  double-notices interruptions; DingTalk channel surfaces reply errors

### Added — channels (Phase 2, DingTalk first)
- **`channel` package**: `Channel` interface + normalised `Event` /
  `ConfirmationEvent` inbound types, `Capability`, connection `Status`,
  and `SplitText` line-aware message splitting; `Gateway` orchestrates
  inbound events — per-chat session agents, one reply at a time with
  drop-with-notice for concurrent messages, reply event streams tee'd to
  the channel's `SendResponse`, and tool-call confirmation round-trips:
  text-mode answers (y/n/a plus common Chinese equivalents) apply to all
  parked calls, while native confirmation UIs deliver
  `ConfirmationEvent`s answered per call; optional `Notifier` for
  housekeeping notices
- **`channel/dingtalk` package**: DingTalk enterprise robot channel over
  the official Stream SDK (long-lived inbound connection, no public
  endpoint); replies and confirm prompts sent as Markdown through the
  per-message session webhook (expiry-checked); group chats answered
  only when @-mentioned by default (`ReplyWithoutAt` to change); v1
  scope: text/Markdown + text-mode confirmations — AI-card streaming,
  media, and user search not ported yet (`examples/dingtalk_channel`)

### Added — console (Phase 1)
- **`console` package** (port of Python agentscope's `console` module):
  `console.Renderer` turns an agent event stream into line-based terminal
  output (three verbosity levels, tool-result truncation, HITL notices,
  token usage, auto-detected ANSI color honoring `NO_COLOR`) and exposes
  the accumulated reply via `LastMsg`; `console.Launch` is an interactive
  chat loop bound to one agent — stdin prompt, streamed rendering,
  tool-call confirmation (`y`/`N`/`a`, where `a` accepts suggested
  permission rules), Ctrl+C interrupts the current reply at the stream or
  the confirmation prompt and exits at the input prompt, `exit`/`quit`/
  Ctrl+D to leave. Interruption is context cancellation; confirmations
  are submitted out-of-band via `SubmitUserConfirm` while the reply
  stream stays open (`examples/console`)

### Added — harness engineering batch
- **Flight recorder** (`replay/`): `NewRecorder` gains `WithRingLimit`,
  `WithRecordSizeLimit` (oversized inputs stored as summaries),
  `WithDumpOnError` (atomic `flight-<reply_id>-<unixnano>.jsonl` on failed
  replies, tape + event tail) and `WithRedactor`; `Entry` gains `reply_id` and
  `usage` fields (additive JSON)
- **Run correlation**: reply IDs flow from the agent through MiddleContext
  into recorders and audit entries (`audit.Entry.reply_id`), and into
  tracing spans via the new optional `tracing.LateAttributer` extension
- **`event/streamcheck`**: single implementation of event-stream invariants
  (reply/block/tool-call/tool-result pairing, no orphan deltas); `agenttest`
  delegates to it, and `middleware.NewStreamValidator` offers opt-in runtime
  validation for development
- **Provider contract wall** (`providercontract/`, test-only): usage
  accounting, streaming lifecycle (exactly one IsLast), truncation-error
  surfacing, ctx-cancel stops, error taxonomy (429 retryable / 401 not),
  and thinking wire formats — harnesses for openai, anthropic, dashscope,
  gemini, deepseek, moonshot
- **Golden replay seeds** (`agent/testdata/golden/`): multi-tool batch,
  HITL park/resume, external tool, compression summary; randomized
  ids/timestamps normalized; regenerate with `-golden-update`
- **Repetition breaker** (`middleware.NewRepetitionBreaker`): detects
  identical successful tool-call spins (name+input hash; failed calls reset the
  success streak — superseded within this same unreleased cycle by the
  independent error-streak dimension in the "upstream sync batch" section
  above), injects a change-strategy reminder at threshold, and past the
  threshold the typed `ErrToolRepetition` replaces the tool result (the
  over-threshold call itself still executes — side effects cannot be
  un-run — but its actual result is discarded and the model sees the
  error). Streaks are keyed per reply, so concurrent replies on one agent
  do not reset each other. Allowlist exempts read-only/idempotent tools.
  Spins whose inputs vary by timestamps/random values are out of scope
- **Reply watchdog** (`middleware.NewReplyWatchdog`): wall-clock and idle
  timeouts cancel stalled replies
- **Evaluation kit** (`replay/evalkit/`): YAML task suites with fixtures
  and budgets, a runner with pinned sampling (default temperature 0),
  scorers (contains / json_field / text_contains / trajectory / budget /
  LLM judge with result caching), multi-turn tasks, Markdown suite reports,
  and A/B `Compare` reports (flips + token deltas + verdict)
- **Cost governance**: `model.ResolvePrice` pricing overlay (`SetPrice`
  overrides win; kept separate from upstream card sync), `CostLedger` +
  `CostTrackingMiddleware` cross-session aggregation (session-safe query
  API; low-cardinality labels only for metrics), and
  `ReplyCostBudgetMiddleware` (80% soft warning hint + hard stop with
  `ErrBudgetExceeded`)
- **Run logs**: `middleware.NewRunJSONL` (with redactor hook) writes the
  full event stream + middleware-routed model-call records as JSONL
  (compression summary calls bypass middleware and are not recorded); `replay.ParseRunLog` /
  `DiffRunLogs` align two runs by LCS over event types;
  `examples/replayview` terminal viewer
- **Fault injection** (`agenttest/faults/`): deterministic model-error /
  tool-failure / latency injection for resilience chaos testing
- **Crash recovery**: `agent.WithStateSaver` auto-checkpoints at tool-batch
  boundaries and park points; `AgentState` gains `schema_version`;
  `agent.LoadCheckpoint` loads resumable state (rejects newer schemas);
  resumed replies re-emit `RequireUserConfirmEvent` /
  `RequireExternalExecutionEvent` for pending calls and wait again.
  Contract: a crash mid-batch resumes by re-executing the whole batch
  (side effects are not exactly-once across crashes)
- **Bench v2**: `Battery` + `Baseline` save/load + `CheckBaseline`
  regression detection (p95 latency with fractional-ms precision + success
  rate, 10% slack)
- **`model.WithSeed`**: sampling seed pass-through for the OpenAI-family
  providers (evaluation determinism)

### Changed
- Resumed conversations with pending ASKING calls are detected even when a
  fresh user input follows the restored assistant message; a confirmed call
  is executed inline through the normal acting path so resumed HITL work
  actually runs. Batch confirmations AND batched external execution
  results are stashed per call ID, so a single event answering several
  pending calls is never lost; the state checkpoint is refreshed right
  after resumed calls execute, so a crash just after resume cannot
  re-execute an already-executed tool. Behavior change: a
  `SubmitExternalResult` event matching no pending call no longer ends
  the wait — it is stashed, and the affected reply blocks until a
  matching result arrives or its context ends (previously it
  fast-errored with a tool-result error)

### Fixed
- Reprompted confirmations force the ALLOWED call state regardless of the
  state echoed back by the confirmer (previously an echoed asking-state
  block could loop the re-prompt forever)

## [v2.0.9] - 2026-08-26

### Added
- **Structured output strategy ladder** (`model/`): `GenerateStructuredOutput`
  now walks `forced → auto → no_think → none`; provider request-shape
  rejections and missing structured results advance the ladder, other errors
  stop it; failures wrap the typed `errors.ErrStructuredOutput` (port of
  upstream fix #2140)
- **`model.WithThinkingDisabled()`** call option + `ThinkingDisabler`
  provider interface: DashScope sends `enable_thinking=false`;
  DeepSeek/Moonshot/Anthropic send `thinking:{"type":"disabled"}`
  (upstream #2140)
- **`gen_ai.input.messages` chat-span attribute** (`middleware/tracing`):
  bounded `role: text` serialization of what the model saw (port of upstream
  fix #2391)
- **`ToolResultDataDeltaEvent.Validate()`** (`event/`): enforces exactly one
  of Data/URL (port of upstream fix #2370)

### Changed
- **Gemini usage accounting**: tool-use prompt tokens count as input, thought
  tokens as output, cached-content tokens feed cache accounting (port of
  upstream fix #2406)
- **Audio input formats**: explicit `wav|mp3|mpeg→mp3` map for `input_audio`;
  unsupported audio subtypes produce a clear `Format` error instead of being
  passed through to the API (port of upstream fix #2301). Note: the exported
  `FormatDataBlockForOpenAI`/`FormatDataBlockForDashScope` helpers keep their
  signatures and return nil for invalid audio (dropping the block); the
  `Format` paths surface the error
- **Compression trigger ratio** `0.9` is now accepted; only values above it
  fall back to the 0.8 default (port of upstream fix #2396)

### Fixed
- **OpenAI Responses streams close deterministically**: `processStream`
  honors ctx on every send; scan errors and streams ending without
  `response.completed` surface as a final `IsLast` response instead of
  ending silently (port of upstream fix #2349)
- **`ReadCache` refreshes recency on hit**: repeatedly-read files no longer
  evict first under FIFO (port of upstream fix #1811)
- **Compression falls back to truncation** when summary generation fails —
  the context is truncated to the reserve set, the previous summary is kept
  (a truncation notice substitutes an empty one) and the dropped content is
  offloaded when an offloader is configured, instead of staying wedged above
  the threshold (port of upstream fix #2140)
- **Chat SSE handler releases the session registry via defer**: a panic
  mid-handler can no longer wedge the session slot permanently

## [v2.0.8] - 2026-08-18

### Added
- **Model cards synced with the upstream AgentScope Python v2.0.6 refresh**:
  24 cards added (claude-fable-5/opus-5/sonnet-5, qwen-flash and
  qwen3.5/3.6/3.7-flash, qwen3.8-max, gemini flash-lite family,
  gemini-3.5/3.6-flash, kimi-k2.7-code(-highspeed), gpt-5.6 luna/sol/terra,
  grok-4.20 family, grok-4.5, grok-build-0.1); 48 existing cards refreshed to
  upstream values; Go-exclusive embedding cards preserved
- **`ModelCard` lenient `deprecated_at` parsing** (`UnmarshalYAML`): accepts
  RFC3339, naive ISO, space-separated, and plain-date timestamps, matching the
  upstream Python card format (previously sunset cards were silently dropped)
- **`Toolkit.HasGroup` / `IsGroupActive` / `GroupNames`** for group inspection
- **`StdioClient.Reconnect`** (`mcp/`): reconnect a closed or failed MCP
  stdio client; `Close` is now idempotent and calls on a closed client return
  a clear error (port of upstream fix #2308). `Close`/`Reconnect` stay
  responsive even while a `CallTool` is hung on an unresponsive server — the
  subprocess is killed outside the wire mutex, which unblocks the in-flight
  read
- **`platform.Command`**: build an `exec.Cmd` through the platform-detected
  shell (bash/zsh/sh on Unix; pwsh, powershell.exe, or cmd.exe on Windows)
- **`FormatDataBlockForDashScope`** (`formatter/`): DashScope audio variant —
  base64 audio wrapped in a `data:;base64,` URL (port of upstream fix #2315).
  The mpeg→mp3 format mapping lives in the shared data-block formatter and
  applies to the OpenAI path as well (its `input_audio` API only accepts
  `wav|mp3` labels)
- Regression test locking in Moonshot/OpenAI-compatible trailing usage-only
  chunk handling (upstream #2314; verified not affected)

### Changed
- **DashScope requests now use `DashScopeFormatter`**: base64 audio sent as
  data URLs (upstream fix #2315), video DataBlocks emitted as `video_url`
  parts (previously dropped), and `reasoning_content` preserved in request
  history
- **Shell execution is platform-aware**: `LocalWorkspace.Execute`,
  `workspace.LocalBackend.ExecCommand`, `tool.LocalBackend.ExecShell`, and the
  legacy shell tool run commands through `platform.Command` instead of
  hardcoded `sh -c` — Windows workspaces execute via PowerShell/cmd
  (port of upstream feature #2132). On Unix, execution now follows the
  detected shell (bash/zsh/sh): a non-POSIX `$SHELL` (fish, tcsh, xonsh, …)
  is skipped in favor of the bash→zsh→sh chain so `-c` commands keep POSIX
  semantics

### Fixed
- **ResetTools validates all group names before changing any state**: unknown
  or `basic` group names now return an error listing the invalid names and
  available groups instead of partially resetting activations; non-string
  array elements (e.g. `activate: [1]`) are rejected instead of being
  silently dropped (port of upstream fix #2302)
- **`CollectStream` preserves ERROR state**: a trailing INTERRUPTED/DENIED
  final chunk no longer overwrites an earlier ERROR (port of upstream fix
  #2178)
- **External tool results no longer emit a duplicate `ToolResultStart`**: a
  SUBMITTED external call already emits its start event at submission, so a
  wait that ends without a matching result (canceled, timed out, or unmatched
  submission) now emits only the delta/end (upstream #2167 class)

## [v2.0.7] - 2026-08-11

### Added
- **K8s workspace hardening** (`workspace/k8s.go`): `PodSecurityContext`
  (RunAsNonRoot, RunAsUser, RunAsGroup, FSGroup), `ResourceRequirements`
  (CPU/Memory limits and requests), `ServiceAccountName`, Labels + Annotations
  on created Pods, `PodTTLSeconds` (default 3600, anti-leak cleanup via
  `activeDeadlineSeconds`), `ImagePullPolicy` (default `IfNotPresent`),
  `DisableServiceAccount` (`automountServiceAccountToken: false`), and
  `SecretToken` (`model.SecretStr`) alongside the plain `Token` field
- **Read-only Kubernetes cluster tools** (`workspace/k8s_tools.go`):
  `NewKubectlGetTool` (15 resource types; `secrets` BLOCKED) and
  `NewKubectlLogTool` (tail/since/container) — 30s timeout, no cluster mutation
- `examples/k8s_workspace` demonstrating the usage pattern

### Changed
- Extracted `buildPodManifest()` for testability

### Fixed
- Duplicate timeout in `runKubectl` (now respects the parent context deadline)
- Replaced GNU `find -printf` with a POSIX-compatible alternative

## [v2.0.6] - 2026-08-10

### Added
- **Web UI Studio** (`webui/`): embedded single-page web interface for agent
  interaction; zero-dependency SPA served via `go:embed` with streaming chat,
  thinking-block display, tool-call visualization, human-in-the-loop
  confirmation, session management, and model browser; mount with
  `service.HandlerWithWebUI` or use `webui.Handler` directly
- **Reranker interface + RerankedIndex** (`rag/`): `Reranker` interface and
  `RerankedIndex` wrapper that re-scores retrieval results for improved
  precision
- **Eval harness** (`replay/`): `Scorer` interface, `EvalTape`, and
  `AssertTape` for replay-based agent evaluation with pluggable scoring
  functions
- **CostTrackerMiddleware**: hard USD spend cap via `WithMaxCostUSD`; currency-conversion
  display support (`WithExchangeRate`, e.g. CNY)
- **GuardrailMiddleware** (`middleware/`): output content safety filtering
  with three actions — `Block` (rejects with `ErrGuardrailBlocked`), `Redact`
  (replaces with placeholder), `Warn` (allows with metadata flags); built-in
  rules: `KeywordBlockRule`, `KeywordRedactRule`, `MaxLengthRule`,
  `CustomRule`
- **RedisFullStorage** (`storage/`): 28-method `FullStorage` implementation
  backed by Redis with TTL, prefix isolation, and atomic operations
- **SecretStr**: `UnmarshalJSON` support + dual-field (`APIKey`/`SecretAPIKey`)
  across all 22 API-key configs (model, TTS, embedding, workspace, hub, and storage adapters) for safe key handling
- **`errors.AgentError.Is()` method**: sentinel matching via `errors.Is`;
  `AgentMessage()` for LLM-facing error descriptions
- Spanish README (`README.es-ES.md`) — first community PR from a human
  contributor (@webbrain-one, #3)
- Werewolves multi-agent game demo (`examples/werewolves`) plus 3 more
  examples; 35 new tests from the adversarial-review pass

### Added — Edge & Embedded Intelligence
- **ConnectivityAwareModel** (`model/connectivity.go`): wraps local + cloud
  ChatModel with internal circuit breaker; routes to cloud when online, falls
  back to local (Ollama) when offline, auto-recovers via single-probe half-open
- **PubSub interface** (`messagebus/pubsub.go`): minimal pub/sub contract for
  IoT protocols with QoS (0/1/2), retain, configurable buffer size
- **MQTT adapter** (`messagebus/mqtt/`): Eclipse Paho-based PubSub
  implementation with auto-reconnect, topic prefix, credentials; build tag
  `mqtt` to avoid bloating non-MQTT builds
- **Device framework** (`device/`): `Connector` interface + 4 pure-Go hardware
  drivers (Serial via termios, GPIO via chardev, CAN via SocketCAN, I2C via
  i2c-dev) — all `//go:build linux`, zero CGO
- **DeviceTool**: wraps any Connector as `tool.Tool`; sensors auto-allowed,
  actuators require bypass-immune ASK; integrated watchdog kick on success
- **SensorTool**: read-only sensor tool with JSON output and auto-allow
  permissions
- **SensorMiddleware**: injects live sensor readings into system prompt
  (`[SENSOR DATA]...[/SENSOR DATA]`) with configurable max-token budget to
  prevent context bloat
- **Watchdog**: timer-based safety; if `Kick()` not called within timeout,
  triggers safe-state callback (motor off, valve close, etc.)
- **CI cross-arch job**: build verification for linux/arm64, arm, mips64le,
  riscv64 with binary size check (< 18MB)
- 4 edge examples: `edge_offline`, `edge_sensor`, `edge_serial_robot`,
  `edge_fleet`
- 4 docs: edge-deployment.md, device-tools.md, offline-operation.md,
  multi-device.md

### Added — Security & Audit
- **Audit logging** (`audit/`): new package with structured `Logger` interface
  and 4 implementations (InMemory, File/JSON-Lines, Multi fan-out, Nop); 10
  action types; context propagation via `WithLogger`/`GetLogger`
- **Sandbox execution events**: 3 new event types — `tool_exec_start`,
  `tool_exec_end`, `tool_policy_denied` — providing visibility into the
  orchestrator execution layer
- **Sandbox Policy enforcement**: `sandbox.Policy` now actually controls tool
  execution; `OrchestratorConfig.Policy` blocks tool calls that violate
  filesystem/network/process policies before execution
- **Process-group isolation** (`proc_unix.go`/`proc_windows.go`): child
  processes killed as a group on timeout via `Setpgid` + `SIGKILL` to process
  group, preventing orphans and fork-bombs
- **Interpreter attack detection** (`CheckInterpreterAttack`): detects
  dangerous API calls hidden inside interpreter inline-code flags
  (`python -c`, `node -e`, `perl -e`, `ruby -e`, `lua -e`, `php -r`); checks
  for `os.system`, `subprocess`, `child_process`, etc.
- **Write hardening**: 10 MB size cap (`MaxWriteBytes`), atomic writes via
  `fsutil.WriteFileAtomic`, executable-extension detection triggers
  bypass-immune ASK for .sh/.py/.exe/.dll etc.
- **Expanded dangerous paths**: added 20+ credential files (.kube/config,
  .aws/credentials, .docker/config.json, SSH private keys, .gnupg/*) and 4
  directories (.kube, .aws, .docker, .gnupg) to the safety layer
- **awk removed from read-only allowlist**: `awk` can execute arbitrary
  commands via `system()` and write files

### Changed
- **`exception` package removed**: all types consolidated into `errors/`; no
  more split hierarchy

### Fixed
- Data race in `a2a/grpc` `Server.Close` vs the Listen accept loop
- Watchdog timer race in device tests
- `TextBlock` JSON serialization in the werewolves example
- `/static/` prefix path resolution for CSS/JS assets in webui

## [v2.0.5] - 2026-08-04

### Added
- **Ported 25 features from AgentScope Python**, including:
  - **5 workspace backends**: Kubernetes (`workspace/k8s.go`), OpenSandbox,
    Daytona, Apple Container, Bubblewrap
  - **Gemini TTS** (`tts/gemini.go`): text-to-speech via the Gemini
    generateContent API with audio response modality; default model
    `gemini-2.5-flash-preview-tts`
  - **Document parsers** (`rag/parser/`): Parser interface with 5
    implementations — `TextParser`, `PDFParser`, `WordParser`, `ExcelParser`,
    `PPTParser` — all producing `rag.Document` slices with configurable text
    chunking and overlap
  - **Multi-tenant RBAC** (`access/`): four permission levels
    (`none`/`read`/`write`/`admin`) across four resource kinds
    (`credential`/`agent`/`knowledge_base`/`session`); principals can be
    users, groups, or organizations; pluggable `Store` interface
  - **Component Hub** (`hub/`): `Hub` interface for browsing, searching, and
    installing MCP tools and skills from remote registries; `MCPHub`,
    `SkillHub` adapters; multi-hub `Registry` for unified search
  - **More retrieval and storage backends**: Elasticsearch, Milvus, and
    MongoDB indexes (`rag/`); SQL storage backend (`storage/sql.go`);
    cursor-based pagination for `list_messages`
  - **Model cards**: Kimi K3 (256K context, 64K output, vision + video +
    thinking), qwen3.7-plus (131K context, thinking), GLM-5.2 (131K context)
  - **`OnCheckPermission` middleware hook**: wraps the permission check before
    tool execution; enables custom authorization, audit logging, or dynamic
    permission policies (middleware hook count 6 → 7)
- **Six Go-native capabilities**:
  - **Deterministic Replay** (`replay/`): record/replay middleware capturing
    model-call request/response pairs into JSON tapes; replay without calling
    the LLM for offline CI/CD testing
  - **Hot-reload Config** (`hotreload/`): polling-based file watcher with
    generic `Reloader[T]` and atomic pointer swap; JSON/YAML/TOML parsers
  - **WASM Sandbox** (`wasm/`): execute WebAssembly modules with strict
    resource limits (memory, time, instruction count/fuel); auto-discovers
    `wasmtime`, `wasmer`, or `wasm3` CLI runtimes
  - **gRPC-style TCP A2A transport** (`a2a/grpc/`): bidirectional
    agent-to-agent communication over newline-delimited JSON on TCP; `Server`
    + `Client` with streaming support
  - **Agent Load Testing** (`bench/`): scenario-based benchmarking with
    configurable concurrency, duration, and ramp-up; throughput, latency
    percentiles (p50/p95/p99), and error breakdowns
  - **Generic Pool with backpressure**: bounded queue (`ErrPoolFull`) and
    atomic `PoolStats`, building on the fan-out `AgentPool` introduced in
    v2.0.4

## [v2.0.4] - 2026-07-13

### Added
- **v3 production infrastructure layer**: `protocol`, `loop`, `runtime`,
  `metrics`, `tracing`, `sandbox`, `platform` packages; universal agent loop
  with state machine and event streaming
- **Coding agent infrastructure**: fan-out `AgentPool`, `agenttest` mock-model
  harness, project/settings config, MCP stdio server, CostTracker + metrics
  middleware, prompt composer/providers, resilience primitives (circuit
  breaker, rate limiter, model wrapper)
- **6 new built-in tools (15 total)**: MultiEdit, ApplyPatch, WebFetch, Spawn,
  LSP, Notebook
- **Tool backend routing**: read/write/edit tools route through a configured
  workspace backend (`tool.WithBackend`), closing sandbox isolation for file
  tools
- **AgentManager** for subagent lifecycle; **SkillManager**; Windows safety
  checks; `InMemoryProvider`
- **Anthropic prompt caching** (opt-in) + prompt-cache token accounting in
  loop events and budget
- **JSON-Schema tool input validation**
- **Prometheus metrics provider** + optional `/metrics` endpoint
- **OpenAI TTS** (`tts/openai.go`): OpenAI Audio Speech API
  (`/v1/audio/speech`) with streaming support
- **Streaming error propagation**: `ChatResponse` carries `Error` and
  `StopReason`
- **Production hardening (first pass)**: WebFetch SSRF guard (blocks
  loopback/private/link-local incl. cloud metadata), MCP subprocess env
  isolation, per-tool timeout + result cap, opt-in workspace-root jail for
  file tools, token and duration budgets on the loop path, atomic file writes,
  HTTP server hardening (`ReadHeaderTimeout`/`IdleTimeout`/body caps) +
  `/healthz` + `/readyz` + graceful shutdown, credential redaction in URLs and
  logs, OTEL span attributes and per-label metrics, fuzz targets + coverage
  step in CI
- Gemini schema sanitizer (const→enum, removes `$ref`/`$schema`/
  `additionalProperties`), JSON repair utility for truncated tool-call inputs,
  Ollama formatter `tool_name` field, Gemini synthetic tool-call ID generation
- STABILITY.md documenting versioning and stability tiers

### Changed
- **BREAKING**: module path migrated to `/v2` — import
  `github.com/alanfokco/agentscope-go/v2/pkg/agentscope/...` (the repository has
  since moved to `github.com/agentscope-ai/agentscope-go`; see `[v2.0.11]` →
  "Changed — repository and module path move")
- Deprecated `ReActAgent`; examples migrated to `UnifiedAgent`

### Fixed
- OpenAI-compatible API: use `max_completion_tokens` instead of the deprecated
  `max_tokens` wire field
- Anthropic formatter: drop empty text/thinking blocks (fixes 400 responses)
- Gemini formatter: drop empty text/thinking blocks (fixes 400 responses)
- Event-forwarder goroutine leaks and torn reads in managers/schedulers
- Kept HITL/external tools off the concurrent batch path
- 10 code-review fixes (middleware bypass, HITL backfill, safety regex, races,
  panics)
- Credential leakage via URLs and logs

### Security
- Closed the bash read-only classification bypass (write redirects are no
  longer auto-allowed) plus 8 further hardening fixes
- Removed `curl`/`wget` from the read-only bash allowlist

## [v2.0.3] - 2026-06-25

### Added
- 9 model provider adapters (OpenAI, Anthropic, DashScope, DeepSeek, Gemini,
  Ollama, Moonshot, xAI, OpenAI Responses API)
- 54 bundled model cards with context sizes, capabilities, and status
- UnifiedAgent (v2) with native API-level tool calling and streaming
- ReActAgent (v1) with text-based tool calling protocol
- 9 built-in tools: Bash, Read, Write, Edit, Glob, Grep, ResetTools,
  TaskCreate/Get/List/Update, Schedule (Create/Delete/List/View)
- Bash safety analysis with AST-level injection detection
- 6-hook middleware system (OnReply, OnReasoning, OnModelCall, OnActing,
  OnSystemPrompt, OnCompressContext)
- Per-tool middleware support
- Built-in middleware: TracingMiddleware, TTSMiddleware,
  ReplyBudgetControlMiddleware, LongTermMemoryMiddleware
- Permission engine with 5 modes (Default, AcceptEdits, Explore, Bypass,
  DontAsk)
- MCP client (Stdio + HTTP JSON-RPC) with MCPTool adapter. There has never been
  an SSE/streamable-HTTP transport; an earlier revision of this line claimed one
- A2A (Agent-to-Agent) HTTP protocol
- Agent Teams with Leader/Worker coordination tools
- Pipeline with Then/If combinators + MsgHub for multi-agent routing
- Embedding models (OpenAI, DashScope, Gemini, Ollama) with batch processing
  and caching
- RAG with InMemoryIndex and Qdrant vector store
- TTS (DashScope standard + CosyVoice realtime)
- Audio caption streaming (PCM to WAV) for OpenAI/DashScope omni models
- Context compression with structured summaries and tool result truncation
- Workspace sandboxing (Local, Docker, E2B)
- HTTP Agent Service with REST + SSE streaming + AG-UI protocol
- InMemory + File + Redis storage backends
- InMemory + Redis message bus with registry operations
- Scheduled task execution (one-shot and recurring)
- Configurable ID factory (GenerateID/SetIDFactory)
- ClientOptions for all model providers (Timeout, DefaultHeaders, Transport)
- mem0 REST API memory store adapter
- SubagentHitlProjector with durable storage and 5 event types
- 25 examples covering all major features
