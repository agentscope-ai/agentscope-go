# Fixed-deployment managed tasks

The experimental `pkg/agentscope/managedtask` package implements stage 3 of
[RFC #16](https://github.com/agentscope-ai/agentscope-go/issues/16). It executes
one fixed deployment independently of `Agent` and the classifier. Jev selection,
model pools, fallback, persisted routes and quality evaluation remain later stages.
The [stage documentation checklist](https://github.com/agentscope-ai/agentscope-go/issues/16)
tracks the agreed sequence. Availability in a source checkout does not imply a
published release.

Run the complete offline example:

```bash
go run ./examples/managed_task
```

It creates a loopback HTTP fixture, projects synthetic text, runs an explicitly
isolated evaluation, and demonstrates production rejection without qualification.
The requests are actual local HTTP attempts, marked `PhysicalAttempt`; their
model, usage and context bound are synthetic. They establish protocol/lifecycle
behavior, not live compatibility, model quality or savings.

## Entry point and supported paths

Construct `model.NewOpenAITextClient(model.OpenAIConfig{...})`, then pass it to
`managedtask.New(managedtask.Config{...})`. `Executor.Execute(ctx, task)` returns
a non-nil `*Result`, including on failure. Check `Status` and `Err` before using
`Text`; text is populated only on `Succeeded`.

A nil caller context returns `Rejected` with `ErrTask`, preserving `TaskID`,
without invoking host callbacks, acquiring capacity or recording attempts.
A nil executor returns `Rejected` with `ErrConfig`, including with a nil context.

| Path | Supported by this entry point |
|---|---|
| OpenAI Chat Completions JSON, one user text | Yes, through the explicit `OpenAITextClient` primitive |
| Compatible Chat Completions endpoints | Only where the host has verified this exact protocol, model identity, limits and quality; local fixtures alone do not qualify a live endpoint |
| Responses, other provider adapters, custom `ChatModel`/fallback wrappers | No |
| Streaming, tools, multimodal input/output, Agent rounds or helper calls | No |
| Jev/classifier invocation and model selection | No; use the independent [classifier API](classifier.md) outside this stage |

Ordinary `Chat`, `ChatStream`, Agent and middleware behavior is unchanged. The
new client reuses the existing OpenAI request representation, formatter and
response parser, with stricter validation around the latter. It does not use the
shared HTTP helper because that helper retries and reads unbounded responses.

`Task` contains host-owned `ID`, `Category`, plain `Text` and `Mode`.
Empty mode means `Production`. `Evaluation` bypasses qualification only: the host
must enforce isolation and still authorize and project disclosure. Untrusted task
text cannot supply host identity, categories, mode or permissions.

Terminal statuses are `Succeeded`, `Rejected`, `Failed`, `Canceled`, `TimedOut`,
`Unsupported`, `Truncated` and `Filtered`. Invalid tasks, policy/context failures,
capacity rejection and queue expiry are rejected. Exhausted retryable provider
errors and malformed responses fail. Tool calls (including legacy function calls)
or audio are unsupported even if accompanied by text and a `stop` finish reason.
A `length` result is truncated; filtering/refusal is filtered. These outcomes
never publish partial text. Unknown finish reasons, extra choices, missing concrete
model identity, a model different from the configured version, non-assistant
roles and non-string content fail validation.

## Deployment and host contracts

`Deployment` includes stable `ID`, actual `Endpoint`, pinned `Model`, configured
`ContextWindow`, and explicit `TextOnly`, `NonStreaming`, `ToolsDisabled`
capabilities. Endpoint and model must exactly match `Client.Target()`. Endpoint is
the full completion URL: `BaseURL` with trailing slashes removed, followed by
`/v1/chat/completions`. Query strings, credentials in URLs, fragments and non-HTTP(S)
schemes are rejected. Use HTTPS for remote services. Choosing/trusting a destination
and any transport proxy is the host's responsibility.

Production tasks require a matching `Qualification`: deployment ID, actual endpoint,
model version, category, evidence ID, dataset/rubric versions, positive sample size,
permitted fractional quality loss in [0,1], and positive latency margin. These are
host assertions, not library-certified quality evidence. Do not manufacture them
from protocol fixtures. Qualification validity/revocation and the interpretation
of the evidence belong to current host policy.

Required callbacks are trusted, concurrency-safe host code and receive copied
configuration. They must honor context and must not dispatch hidden model calls:

1. `Authorize(ctx, task, deployment)` checks current destination/data policy and
   qualification revocation. It runs before any projection or model request
   construction, then again after admission before every attempt.
2. `Project(ctx, task, deployment)` returns only approved text. This happens before
   request construction, once per task. A dispatch-time authorization must reject
   a prepared request if policy has changed to require different projection.
3. `InputTokenBound(ctx, deployment, finalJSON)` supplies a trustworthy upper bound
   over the exact final wire representation, including provider framing and any
   hidden server prompt. The JSON is a copy: mutation cannot change the request.
   The bound plus `MaxOutputTokens` must fit the configured window. Overflow is
   avoided by subtraction. No truncation or compression is performed.

A missing sizing callback is rejected at construction. Callback failure or an
invalid/excessive bound rejects the task before admission. `CountTokens` is an
estimate and must not be substituted for this upper-bound contract. No built-in
bound is claimed for arbitrary real models or compatible endpoints. Hosts lacking
trustworthy sizing evidence cannot enable this path safely. The example's byte
bound belongs only to its synthetic local fixture.

Host callbacks and provider bodies are omitted from returned error messages;
context error identity is preserved through `errors.Is`. This path emits no logs.
Successful text and provider model metadata are still untrusted provider data.

## Limits, retries and cancellation

`Config.Limits == nil` selects `DefaultLimits()`. To customize, copy that value
and pass its address. Non-nil limits must specify every field; zero waiting capacity
is allowed, other fields must be positive.

| Limit | Default | Scope |
|---|---:|---|
| `MaxTasks` | 32 | Concurrent task workers, including host callbacks, backoff and cleanup |
| `MaxQueued` | 16 | Waiting attempts, excluding active attempts |
| `MaxConcurrent` | 4 | Admitted attempts through local send/cleanup |
| `MaxAttempts` | 2 | Transport dispatch starts per task, including failed attempts |
| `TaskTimeout` | 30 seconds | Entire task, shortened by caller deadline |
| `QueueTimeout` | 5 seconds | Each wait for admission, within task deadline |
| `RetryBackoff` | 100 milliseconds | Fixed minimum between retryable attempts |
| `MaxRequestBytes` | 1 MiB | Original text and final serialized request; projected text also checked |
| `MaxResponseBytes` | 4 MiB | Body read after HTTP decompression |
| `MaxOutputTokens` | 512 | Completion reservation and `max_completion_tokens` on wire |

Task and queue saturation reject immediately. Admission is FIFO, single-process,
and shared only by callers using the **same Executor**. Separate executors have
separate capacity; this is not distributed quota enforcement. Byte limits bound
admitted payloads, not all Go allocations; formatting/serialization allocate
before the final serialized size check.

The executor is the only retry owner. Complete 429/5xx responses are retryable;
other HTTP failures, transport/read/size/validation failures are not automatically
retried. Provider `Retry-After` may extend the fixed backoff, always within the task
deadline. Backoff releases the model permit, and each retry rejoins admission and
rechecks current policy. The `MaxAttempts` cap is also checked at dispatch.

`OpenAITextClient` clones a standard `*http.Transport`, enforces HTTP/1, disables
redirects/cookies and body rewind, and rejects opaque custom RoundTrippers and
idempotency headers. Alternate protocol handlers are removed. Client total timeout
is replaced by the task context; configured transport phase timeouts may still
fail a request. Host dial/TLS/proxy hooks remain trusted code and must honor
cancellation without hidden requests. Close idle connections with
`client.CloseIdleConnections()` when the client is no longer needed.

Cancellation is arbitrated with dispatch accounting and terminal commitment. A
cancellation observed before dispatch yields zero attempts; after transport entry,
the attempt remains even if server receipt is unknown. Once a terminal result is
committed it is immutable; cancellation after that commitment does not revise it.
No retry starts after observed cancellation. Cancellation cannot prove remote
compute stopped or provide exactly-once delivery.

`Execute` returns a frozen terminal snapshot when its deadline/cancellation is
observed. The bounded worker retains its task slot and any acquired permit until
local request/callback cleanup actually ends. A host callback ignoring context
can therefore consume capacity after return. Late results cannot change the
snapshot. `Attempt.Completed == false` explicitly identifies unfinished local
work with unknown final usage; supplemental late accounting is not implemented.

## Accounting and cost uncertainty

Each actual dispatch has exactly one `Attempt`, including failure, with ordinal,
deployment/endpoint, requested and provider-reported model, duration, completion
flag, usage and error. Before dispatch there are no attempts. No logical aggregate
is charged a second time. Missing input/output/cache counts stay nil; a pointer to
zero is known zero. Recognizable valid counts survive HTTP/response validation
errors. Unreadable, oversized or malformed JSON cannot provide trusted usage.
A malformed individual count remains unknown without erasing valid siblings.

Optional host `Prices` specify per-token rates and currency. Without a cached rate,
input price applies flatly to all prompt tokens. With a cached rate, recognized
cache counts split cached/uncached input. Output price must include reasoning tokens
already counted in completion tokens. Prices must apply to the bound model; no
price is applied to unknown or mismatched model identity. Taxes, provider tiering,
minimum fees and billing reconciliation are outside this arithmetic.

`Cost.KnownSubtotal` sums only known priced components across **all** attempts,
including failures; `UnknownAttempts` counts attempts with any unknown component,
missing price, unfinished work or arithmetic overflow. A zero subtotal with unknown
attempts is not free work. Any non-nil `StrictBudget` is rejected; no monetary
ceiling is promised.

## Validation and relationship to other work

```bash
go test -race -count=1 ./pkg/agentscope/model ./pkg/agentscope/managedtask
go run ./examples/managed_task
```

Tests capture local HTTP requests and cover disclosure order, qualification,
context rejection, strict wire responses, bounded concurrency/queueing, deadlines,
retry caps, failed/partial usage, cancellation, frozen results and resource ownership.
They do not establish real-service compatibility or quality. No live provider is
called by default and no model combination gains production qualification here.

This entry point aligns with [RFC #11](https://github.com/agentscope-ai/agentscope-go/issues/11)
at the contract level: finite admission, a single retry owner, physical attempts,
unknown costs and ownership through local completion. The broader shared-governance
work in [PR #15](https://github.com/agentscope-ai/agentscope-go/pull/15) is not a
shipped dependency. This standalone stage does not govern ordinary Agent,
compression, embedding, streaming or helper paths. Admission/accounting can be
extracted into shared governance when those interfaces stabilize.

The inspected [Python routing implementation](https://github.com/agentscope-ai/agentscope/blob/083cbd1975c7b5ed055c69b0d19c150727e7f606/src/agentscope/middleware/_model_router.py)
mutates an Agent model around a reply. Go deliberately introduces an explicit
fixed-task boundary first; this stage performs no selection or Agent mutation.
The protocol follows the [official Chat Completions reference](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create).
