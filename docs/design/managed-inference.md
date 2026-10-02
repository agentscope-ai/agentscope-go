# Managed inference: delivery contracts

Status: staged implementation of [RFC #11](https://github.com/agentscope-ai/agentscope-go/issues/11).
The execution, retrieval and quality-measurement foundations described below are
implemented in this change. Shared inference governance and routing are proposed
subsequent changes. Availability on `main` does not imply a published release.

## Objective and sequence

Improve the number of tasks a shared inference deployment can complete within
explicit quality and latency requirements. Measure resident sessions separately
from completed tasks, and measure fixed-deployment policies separately from
experiments that change models or hardware. A throughput gain has not been
established by the local protocol fixtures.

The implementation sequence is three contributions:

1. **Execution and retrieval foundations:** reliable cancellation and terminal
   classification, complete tool text, safe cache writes, filtered retrieval and
   an arrival-to-quality report. These features are independently usable.
2. **Managed inference and resource governance (proposed):** explicit deployment
   capabilities, a shared model-operation path, physical-attempt accounting,
   bounded admission, embedding request concurrency and restored budget state.
   This stage must work with one fixed model.
3. **Routing and comparative evaluation (proposed):** rule-based and bounded
   classifier routing, persisted/revalidated decisions and repeatable quality,
   cost and load comparisons.

Keep the required `Agent`, `ChatModel` and `Tool` interfaces source-compatible.
New governance and routing policies will be opt-in. Each contribution includes
its executable examples, validation and behavior documentation.

## Current execution paths

| Path | Current behavior | Consequence for subsequent work |
|---|---|---|
| `UnifiedAgent.Reply` | Drains lifecycle events, checks cancellation, then selects the current reply ID from state | Cancellation returns nil message and a context error; recorded partial state may remain |
| `UnifiedAgent.ReplyStream` | Streams provider thinking/text without middleware; legacy middleware remains buffered | Admission must distinguish streamed and buffered paths |
| UnifiedAgent model rounds | Middleware wraps buffered `callModel`; without middleware the stream path retries setup failures only | A logical model round can contain several physical requests before streaming starts |
| Loop bridge | Calls `callModel` directly | Do not assume all model middleware runs through this entry |
| Compression and structured output | Have helper-specific call paths | Account for helper work explicitly in a managed executor |
| Direct `ChatStream` | Returns provider deltas and a final assembled response | Admission must cover the stream lifetime, not just setup |
| `evalkit.Runner.RunTask` / `RunLoad` | Validate terminal outcome and wait for core/tool collectors before workspace transfer | Uncooperative execution may delay cleanup; load reports freeze separately from worker termination |

`Reply` checks cancellation after draining and again under the state lock before
returning a message. It does not atomically arbitrate cancellation with the Go
function return. By contrast, load results are snapshots accepted during their
execution observation interval; later cleanup cancellation does not revise them.

## Foundation behavior

- `callModel` retains the ordinary retry policy, but waits through a context-aware
  timer and checks cancellation before starting another attempt or fallback.
  This does not yet control hidden retries in arbitrary wrappers/transports.
- OpenAI-compatible and Gemini requests retain all tool-result text and bounded
  media placeholders. Responses preserves supported native tool images and emits
  placeholders for unsupported media. The shared token estimator counts every
  tool text block while retaining its existing media estimates; it remains an
  estimate. `ToolResultBlock.GetOutputText` still returns the first text block.
- Toolkit registration copies slice containers, preserving tool object identity.
  This does not make arbitrary tools safe to share between agents.
- Grep requests path ordering from ripgrep and keeps its per-file match limit
  independent of the requested page. The Go fallback walks paths in order.
  Sorting may cost additional work; no performance improvement or snapshot
  consistency under directory mutation is claimed.
- Embedding cache writes skip individually oversized entries before replacement
  or eviction and use atomic file replacement for accepted entries. Chunking
  drops entirely blank windows without trimming retained text.
- Qdrant metadata filters are typed, validated and applied before topK. Host
  conditions are ANDed with query conditions. Ingestion keeps native vectors out
  of payload metadata and returns conversion errors instead of panicking. See [retrieval](../retrieval.md)
  for key, precision, persistence and authorization boundaries.

Tool-output state/metadata reconstruction and existing truncation remain separate
from wire rendering. External results preserve their recorded block sequence and
metadata; ordinary tool aggregation can join text and append media. This change
does not claim to preserve arbitrary original interleaving across every tool or
to replace existing truncation with artifact storage.

## Quality measurement contract

`evalkit.Runner.RunLoad` joins a versioned task/arrival manifest to
`bench.Runner.RunOpenLoop`. The identity is run ID + scenario + one-based iteration,
with task ID and repeat retained. Every planned arrival remains visible, including
not-offered, rejected and unfinished work. Generator authorization and actual
observed callback entry are distinct timestamps.

A successful execution candidate is scored only if the driver also accepted its
callback as successful before the scheduled deadline. Partial responses, missing
or failed terminal events, cancellation, exhausted iterations and pending tool
interactions cannot be treated as task success. Multi-turn failures retain prior
observed work and stop subsequent turns.

Execution and scoring use distinct bounded contexts and observation intervals.
The scorer consumes the existing output and an exclusively retained workspace;
it does not rerun the task. Actual execution/scorer workers retain ownership
through completion, error inspection and cleanup. Timeouts do not release slots
while user code is still running. Late results cannot mutate a published report;
future supplemental accounting must use a separately versioned report.

The [load guide](../benchmarks.md#joining-task-quality-with-scheduled-arrivals)
defines configuration, statuses and the goodput denominator. Completed-latency
percentiles exclude unfinished zero values and include their sample count. This
stage does not establish a complete token/cost ledger: existing evalkit fields
cannot distinguish every unknown usage value or hidden physical attempt.

## Proposed managed execution contracts

A deployment descriptor should distinguish provider, API family, model identity,
actual endpoint/deployment, configured context window, adapter capabilities and
host policy. Model cards are metadata, not evidence of actual server limits.
Keep legacy model-card lookup behavior; qualified lookup belongs to the new path.

Bind the actual target before token sizing and compression. `ModelCallInput.ModelName`
is observability metadata, not a target selector. Final validation must include
prompt hooks, tool schemas, response format and media transformations. A token
estimate cannot provide a strict context guarantee without a trustworthy bound.

Record both logical operations and physical attempts at the actual send boundary.
Define one retry owner, revalidate each fallback target and expose unsupported
wrappers. Attribute reasoning, summary, repair and later routing work without
counting an attempt and its enclosing aggregate twice. Missing usage/prices and
failed attempts must remain visible; they cannot be represented as free work.

Bound active requests and queued demand per deployment across agents. Hold a
permit through a managed stream's local lifetime, release it on every terminal
path, and keep retry backoff/tool waiting/cache hits outside active inference
occupancy. Embedding admission must cover **each actual batch/attempt request**,
not merely the outer `Embed` invocation. Local cancellation does not prove remote
compute has stopped. Initial single-process admission will not be distributed
quota enforcement.

Restore reply budget/middleware state explicitly, with a versioned schema,
ownership rules and observable persistence errors. Existing checkpoint logging
alone does not establish durable acceptance. Restore and revalidate target IDs
and policy versions instead of serializing model objects or credentials. Never
mutate a shared agent model to implement routing; immutable configuration does
not make arbitrary model clients concurrency-safe.

## Upstream references and deliberate differences

Upstream AgentScope was inspected through
[`083cbd19`](https://github.com/agentscope-ai/agentscope/tree/083cbd1975c7b5ed055c69b0d19c150727e7f606).

| Reference | Go direction |
|---|---|
| [Cancellation #2650](https://github.com/agentscope-ai/agentscope/pull/2650) | Preserve recognizable context errors and stop retry/fallback starts |
| [Cache limits #2790](https://github.com/agentscope-ai/agentscope/pull/2790) | Check encoded size before writing; unlike Python's write/delete path, preserve an older value even for an oversized same-key update |
| [Tool group copies #2721](https://github.com/agentscope-ai/agentscope/pull/2721) | Copy slice containers, retain tool objects |
| [Blank chunks #2687](https://github.com/agentscope-ai/agentscope/pull/2687) | Apply to both Go text-chunk units; retain nonblank text verbatim |
| [Grep ordering #2726](https://github.com/agentscope-ai/agentscope/pull/2726) | Use explicit path sorting and stable per-file limits |
| [Tool media #2751](https://github.com/agentscope-ai/agentscope/pull/2751) | Cover actual Go adapter requests and corresponding token estimates |
| [Tool metadata #2754](https://github.com/agentscope-ai/agentscope/pull/2754) | Preserve existing Go state/event metadata contracts; do not add Python-specific timestamps mechanically |
| [Qdrant floats #2783](https://github.com/agentscope-ai/agentscope/pull/2783) | Typed conditions on Go's top-level payload, finite float equality as a closed range |
| [Routing #2758](https://github.com/agentscope-ai/agentscope/pull/2758) | Proposed after managed execution/accounting; no Jev dependency in these foundations |

Shared cache namespaces/versioned keys must precede cross-tenant cache sharing or
same-miss coalescing. Generic classifier APIs, Jev integration, Excel/multimodal
documents and complete realtime protocols remain conditional separate work.
