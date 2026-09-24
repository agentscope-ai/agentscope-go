# Managed inference

Managed inference is an opt-in, single-process execution path for agents sharing
an inference service. It bounds active HTTP requests and queued demand, owns
retries, and records physical attempts separately from logical operations. The
required `ChatModel` and `EmbeddingModel` interfaces remain unchanged. Routing is
planned separately in [RFC #11](https://github.com/agentscope-ai/agentscope-go/issues/11).

Run the [example](../examples/managed_inference/) from a checkout:

```bash
go run ./examples/managed_inference
```

It runs six agents and a direct model stream against a local HTTP fixture. It
requires no credentials and demonstrates admission and accounting, not provider
quality or throughput. To use a real OpenAI Chat endpoint, set `OPENAI_API_KEY`
and pass `-base-url https://api.openai.com -model YOUR_MODEL`. This adapter appends
`/v1/chat/completions`; embedding adapters use their own base-path conventions.

## Bind a deployment and share its pool

The host owns one `inference.Controller` and `inference.Ledger`. Register a
`PoolConfig` with a unique ID, positive `MaxActive`, and nonnegative `MaxQueued`.
When queuing is enabled, `MaxQueuedWork` must be positive. A zero `MaxQueued`
rejects excess demand with `inference.ErrQueueFull` immediately. Queued work is
FIFO; cancellation removes it without starting a request.

Register each target with `Controller.Register(*inference.DeploymentConfig)`.
Its `Descriptor` identifies the provider label, actual adapter API, model, base
endpoint and optional configured context window. `PoolID` may be shared by chat
and embedding targets served by the same constrained resource. Different pools
do not share limits. Pool IDs are distinct from deployment IDs.

Wrap a supported adapter with `model.NewManagedChatModel(base, deployment)` or
`embedding.NewManagedEmbeddingModel(base, deployment, workers)`. These constructors
copy adapter configuration and headers, validate the actual API/model/endpoint
binding, and own a cloned HTTP transport. Share the resulting model across
agents. Do not mutate caller-owned option values during a call; custom caches
shared by embedding models must support concurrent access.

The managed path requires `inference.WithIdentity(ctx, inference.Identity{...})`
with a nonempty `TenantID`, supplied by trusted host code after authentication.
Prompts do not establish identity. UnifiedAgent supplies its own session ID and
agent name. `model.WithManagedPolicy` can restrict allowed tenants and deny tool
schemas. An empty allowlist permits any host-supplied identity; it does not
authenticate one. This is host policy, not a sandbox for model-generated actions.

`inference.WithEstimatedWork` sets queued demand units, defaulting to one. It is
an admission estimate, not a monetary reservation or a strict token bound.
Deployment `ContextSize` participates in the existing `ContextSizer` resolution;
explicit `agent.ContextConfig.ContextSize` still overrides it. Token estimates
and model cards do not guarantee that a request fits the serving configuration.

## Supported adapters

| Adapter | Descriptor API | Managed path |
|---|---|---|
| OpenAI Chat, DeepSeek, Moonshot, Ollama, DashScope Chat | `openai-chat` | JSON and SSE |
| XAI | `xai-chat` | JSON and SSE, including its separate reasoning usage convention |
| Anthropic | `anthropic` | JSON and SSE |
| Gemini Chat | `gemini-chat` | JSON and SSE; current adapter input is text-only |
| OpenAI-compatible embeddings | `openai-embedding` | Text batches |
| Gemini embeddings | `gemini-embedding` | Text batches |
| DashScope multimodal embeddings | `dashscope-multimodal` | Text, image and video inputs supported by this adapter |

`ManagedChatModel.Capabilities` describes implemented adapter paths, not every
model's capabilities. A server can support less. Direct media source formats and
tool-result rendering remain adapter-specific. Native `ResponseFormat` is
rejected because these adapters do not implement it; structured output uses the
existing synthetic-tool ladder. Optional `ThinkingDisabler` and
`StructuredFallbackClassifier` interfaces remain present only when implemented
by the underlying adapter.

Responses, arbitrary custom models, hidden fallback/connectivity wrappers,
custom `RoundTripper` implementations and custom transport protocol handlers are
rejected. Managed HTTP uses nonempty JSON POST bodies, disables redirects and
body replay, and checks requests against the registered endpoint. It still
performs configured network dialing/TLS through the standard transport. These
restrictions make the observed send boundary explicit; they are not an SSRF
policy for untrusted endpoint configuration.

## Retry and stream lifetime

`DeploymentConfig.MaxAttempts` is a total physical-send cap for one operation,
including retryable transport/status errors and sequential structured-output
fallbacks. It must be between 1 and 100. Retry delay uses exponential backoff
with jitter and respects `Retry-After`. Permits are released during backoff.
Managed calls bypass UnifiedAgent's outer retry loop; configuring agent fallback
models with a managed model is rejected. Per-call retry options do not create
another retry owner. Ordinary unmanaged calls keep their current behavior.

Reasoning, summary and repair attempts carry separate purpose labels. A summary's
structured-output ladder shares its attempt cap. Each embedding batch starts an
independent operation, with each retry entering the shared pool. Cache hits and
tool execution do not occupy inference permits. Positive embedding
`MaxConcurrency` bounds batch workers; zero retains each unmanaged constructor's
existing behavior. The managed constructor requires a positive worker count.
Workers retain input order, cancel siblings on failure and join started work.

Direct `ManagedChatModel.ChatStream` holds admission through response-body,
parser, adapter and forwarding completion. Setup failures can retry within the
cap; a started stream is never replayed after partial output. Raw protocol
termination is validated independently of the adapter's assembled final response.
Consumers must inspect `ChatResponse.Error`, detect a missing `IsLast` response,
and cancel the context when abandoning a stream. The final response contains
assembled content; do not append it again to accumulated deltas.

`Controller.Close` rejects queued/new requests and cancels active local HTTP
work. Cancellation or shutdown can close a stream without a final response, even
if the original caller context is still live. The controller does not wait for
application consumers and cannot prove that remote compute stopped. A model's
`Close` releases its owned idle HTTP connections. `UnifiedAgent.ReplyStream`
continues to emit lifecycle events around `Chat`; it is not provider streaming.

## Physical facts and quality reports

`Ledger` retains at most its configured capacity of attempts and the same number
of operations. Records contain host identity, task attribution, purpose,
timestamps, status and observed billing fields. They exclude prompts, bodies,
headers, endpoint URLs and provider error text. Deployment/identity labels are
host inputs; do not put credentials into them.

Attempts are the additive token/cost source. Operations are logical groupings
without a second cost total. Failed attempts remain visible. Usage categories
are disjoint: ordinary input excludes cache reads/writes. Partial usage has
explicit known flags. A missing usage field is different from a reported zero.
Optional `Price` rates are USD per million tokens; nil means unknown and a
non-nil zero means explicitly free. No price is inferred from the provider label.

Snapshots are detached, versioned observations. `KnownCostUSD` is a subtotal:
inspect `UnknownUsage`, `UnknownCost`, `Incomplete`, `CostOverflow` and `Dropped`
before interpreting it. The subtotal includes only attempts with the required
usage fields and prices for every nonzero billing category. It can include
observed amounts from interrupted streams, whose completeness flags remain
false; it does not price a partially unknown attempt. Retention loss remains ledger-wide in filtered views,
since lost records cannot be assigned to a run. Use a dedicated ledger for an
isolated experiment. Later completions never mutate an existing snapshot.

`middleware.NewCostLedgerFromAttempts` projects the same facts. In this mode,
logical `Record`/`CostTracking` calls add no cost, preventing double counting.
The legacy middleware ledger remains available for unmanaged applications.

Set `evalkit.LoadConfig.AttemptLedger` to the ledger used by managed models.
`RunLoad` attaches run ID, scenario, one-based iteration, task ID and repeat before
calling the model factory. Scoring receives the same task attribution and the
`scoring` purpose through its independent bounded context. Supply a trusted
identity on both execution and scoring contexts when both use managed models.
Use that context for scorer calls so their attempts join the report.

The final `LoadReport.Inference` snapshot and row `InferenceAttemptIDs` include
failed attempts and scoring calls observed before publication. Unmanaged or
mismatched task models and unmatched task attribution are marked incomplete.
Arbitrary custom factories, tools and scorers can make uninstrumented calls;
their hidden requests cannot be detected or priced. The report is not proof of
complete external billing. Late usage needs a new observation, not mutation of
the returned report. Existing task-level cost fields retain their older logical
semantics; use the physical snapshot for managed cost comparisons. See the
[quality/load guide](benchmarks.md) for arrival and goodput denominators.

## Explicit reply recovery

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

Legacy checkpoints keep schema 1. Checkpoints carrying recovery state use schema
2; older readers reject them. New readers accept older conversation state, but
explicit resume needs the typed recovery snapshot. Roll back to an older binary
using a pre-recovery checkpoint or start a fresh conversation; do not strip
budget fields to simulate a successful resume. Budget snapshots reject unknown
versions, negative/nonfinite counters and oversized namespaces on save/resume.

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
the middleware output is sealed are rejected without executing another core. This API applies to
UnifiedAgent replies; the loop bridge does not provide reply recovery.

## Upstream sources

The design was compared with Python AgentScope at
[`083cbd19`](https://github.com/agentscope-ai/agentscope/tree/083cbd1975c7b5ed055c69b0d19c150727e7f606),
including its [model base](https://github.com/agentscope-ai/agentscope/blob/083cbd1975c7b5ed055c69b0d19c150727e7f606/src/agentscope/model/_base.py),
[embedding batching](https://github.com/agentscope-ai/agentscope/blob/083cbd1975c7b5ed055c69b0d19c150727e7f606/src/agentscope/embedding/_openai/_model.py)
and model cards. Go keeps
small optional interfaces, standard-library HTTP transport and host-owned shared
pools; this is not a direct port of a Python admission controller.

Model metadata additions come from
[Python #2727](https://github.com/agentscope-ai/agentscope/pull/2727), commit
[`3ce1c467`](https://github.com/agentscope-ai/agentscope/commit/3ce1c467a9bff1e95563f2497676160fa1092a2e).
The twelve cards cover GPT-6 Astra, Claude Fable 5.1, four DashScope models
(DeepSeek V4.1 Flash, GLM-5.3, Qwen3.8 Flash and Qwen3.8 Omni Flash), DeepSeek
Flash, Gemini 3.7/3.8 Flash, Ollama Gemma4/Qwen3.8 27B and Grok 4.6. Only fields
recognized by Go's `ModelCard` are copied; Python UI parameter overrides are
omitted. `model.GetModelCardFor(provider, name)` provides provider-qualified,
owned metadata while the legacy name lookup stays unchanged. These upstream
catalog entries are not live verification of availability, prices or limits.
