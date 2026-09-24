# Managed inference: delivery contracts

Status: staged implementation of [RFC #11](https://github.com/agentscope-ai/agentscope-go/issues/11).
The execution, retrieval and quality-measurement foundations and opt-in managed
inference path are implemented. Routing remains a proposed subsequent change. Availability on `main` does not imply a published release.

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
2. **Managed inference and resource governance:** explicit deployment
   capabilities, a shared model-operation path, physical-attempt accounting,
   bounded admission, embedding request concurrency and restored budget state.
   This stage works with one fixed model; see the [managed inference guide](../managed-inference.md).
3. **Routing and comparative evaluation (proposed):** rule-based and bounded
   classifier routing, persisted/revalidated decisions and repeatable quality,
   cost and load comparisons.

Keep the required `Agent`, `ChatModel` and `Tool` interfaces source-compatible.
Managed inference is opt-in; subsequent routing policies will also be opt-in. Each contribution includes
its executable examples, validation and behavior documentation.

## Current execution paths

| Path | Current behavior | Consequence for subsequent work |
|---|---|---|
| `UnifiedAgent.Reply` | Drains lifecycle events, checks cancellation, then selects the current reply ID from state | Cancellation returns nil message and a context error; recorded partial state may remain |
| `UnifiedAgent.ReplyStream` | Emits agent lifecycle events while calling `Chat` | This is not provider token streaming |
| UnifiedAgent model rounds | Model middleware wraps `callModel`; retries/fallback stop on cancellation | A logical model round can contain several physical requests |
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
percentiles exclude unfinished zero values and include their sample count. The optional `LoadConfig.AttemptLedger` joins managed physical attempts, including
scoring calls made with the supplied context. Unknown usage, retention loss and
unmanaged task models remain explicit. Custom uninstrumented calls are outside
this observation; existing task-level cost fields retain their logical semantics.

## Managed execution contracts

The [managed inference guide](../managed-inference.md) documents registration,
adapter support, retry ownership, streaming cleanup, bounded embedding workers,
physical accounting and explicit reply recovery. The
[example](../../examples/managed_inference/) runs multiple agents and a direct
provider stream against a local protocol fixture.

Deployment identity, adapter capabilities and host policy are separate values.
Model cards remain descriptive metadata. Admission pools are separate from
actual targets, allowing chat and embedding deployments to share one bound.
Physical attempts are the canonical cost source; logical operations never add a
second copy of their cost. Missing usage or prices are not free work.

Managed execution is single-target and single-process. It rejects hidden
fallback/connectivity wrappers and opaque transports, holds stream permits until
local producers finish, and uses one retry owner. Existing interfaces and
unmanaged defaults remain compatible. Local cancellation cannot prove that
remote compute stopped. Reply recovery preserves observed built-in budgets and
remaining iterations, but does not provide exactly-once tools or spending
reservations. Recovery state uses schema 2; legacy writes retain schema 1.

## Subsequent routing work

Routing must bind the actual target before sizing/compression and revalidate
capabilities, host policy and admission at every fallback. Never mutate a shared
agent model to implement routing. Persist target IDs and policy versions rather
than clients or credentials. Compare quality, cost and load under explicit
fixed-deployment and multi-deployment scenarios before claiming a throughput
gain. Token estimates alone cannot enforce a strict serving-window guarantee.

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
| [Model cards #2727](https://github.com/agentscope-ai/agentscope/pull/2727) | Provider-qualified lookup and supported metadata fields; no automatic server-limit or pricing inference |
| [Routing #2758](https://github.com/agentscope-ai/agentscope/pull/2758) | Proposed after managed execution/accounting; no Jev dependency in these foundations |

Shared cache namespaces/versioned keys must precede cross-tenant cache sharing or
same-miss coalescing. Generic classifier APIs, Jev integration, Excel/multimodal
documents and complete realtime protocols remain conditional separate work.
