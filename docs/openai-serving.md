# OpenAI-compatible agent serving

`openaiserver` is an **Experimental**, standard-library HTTP adapter for
request-owned `UnifiedAgent` instances. It implements a text-only subset of
Chat Completions, not the Responses API. Existing Chat Completions clients can
point their base URL at the host's `/v1` endpoint. Compatibility with Codex or
any other Responses-only client is outside this API.

## Mount a handler

```go
handler, err := openaiserver.New(openaiserver.Config{
    Models: map[string]model.ChatModel{"support": configuredModel},
    MaxConcurrent: 16,
    ReplyTimeout: 2 * time.Minute,
    AgentFactory: func(r *openaiserver.RequestInfo) (*agent.UnifiedAgent, error) {
        // The host can read authenticated identity from r.Request.Context().
        // Construct tenant-specific tools and permissions here.
        return r.NewAgent("support", openaiserver.AgentConfig{
            Toolkit: tool.NewToolkit(lookup),
        }), nil
    },
})
if err != nil {
    return err
}
mux.Handle("/v1/", handler)
```

Imports use the module prefix
`github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/` for `agent`, `model`,
`openaiserver` and `tool`; `time` and `net/http` are standard-library packages.
`configuredModel` and `lookup` are host-owned provider and tool implementations.
See the [complete runnable example](../examples/openai_server/main.go).

The host owns authentication, authorization, logging, tracing, server timeouts,
and graceful shutdown. Mount metrics separately using the existing metrics
provider. Any framework supporting `http.Handler` can mount the same handler;
there is no framework dependency. A nil factory creates a tool-free agent.

## Request ownership and conversation history

Each factory invocation must call `r.NewAgent` exactly once and return that
agent. Returning an arbitrary/shared agent is rejected before execution. Do not
start the agent inside the factory, retain the request, or reuse its state.
Factories must honor `r.Request.Context()`; arbitrary host functions cannot be
forcibly interrupted.

The client sends the full history on every request. Leading `system` and
`developer` messages are combined into the system prompt; prior `user` and
`assistant` messages become history. The final message must be a nonempty `user`
input. Text strings and arrays of text parts are supported. Historical function
calls require unique IDs, JSON-object arguments and matching tool results before
the next conversation message. They are hydrated as **finished**, never executed
again. Unmatched, duplicate and unfinished exchanges return 400. The helper
applies validated state through `agent.WithState`. `AgentConfig` accepts tools,
permissions, an iteration limit, buffered model calls and trusted middleware;
it does not accept arbitrary `AgentOption` functions or shared state.

Configure retry/fallback behavior through `r.ModelConfig` before `r.NewAgent`.
The helper wraps both primary and fallback models so neither bypasses request
options or tool restrictions. `AgentConfig.MaxIters` defaults to 20; negative
values are invalid. `BufferedModel` selects buffered provider calls without
changing the HTTP response format.
Host models must support concurrent calls. Shared tool implementations,
middleware and permission contexts must be concurrency-safe or constructed
separately per request. The handler copies the model registry, not model objects.
Host middleware remains trusted code; it must preserve the request context,
normal model and permission pipelines and must not start detached work.

Automatic context compression, background precompression and checkpoint
recovery are excluded from this stateless v1 construction surface. Provider
context-size failures return 400 with a request to shorten `messages`, instead
of invoking the Agent's implicit compaction recovery and its internal synthetic
tools. Client-owned history remains unchanged. Use the ordinary Agent API when
those stateful features are needed.

## Tools and output

The deployment registers executable tools. If the request includes `tools`, its
names must be a subset of the agent's active tools; unknown names return 400
before a provider call. Omitted `tools` uses all active host tools, while `[]`
selects none. Request schemas are sent to the model, but execution still uses
the host toolkit's schema validation and permission checks. Model responses
calling a tool outside the selected subset fail before execution, including on
fallback models. `tool_choice` accepts only `"auto"` and `"none"`.

Tools run on the server. Neither `tool_calls` nor private tool results are
emitted as structured client tool calls. Thinking/reasoning events are omitted.
Agent text deltas, including text from intermediate model rounds, are published
in order. Interactive permission prompts and external-tool handoffs cannot be
answered by this API and fail explicitly; they are never automatically approved.
Use the regular service/AG-UI interface for interactive workflows.

Successful replies use `finish_reason: "stop"`, including a successful final
summary after the agent's iteration limit. The agent's event interface does not
carry the provider's token-limit stop reason, so `"length"` is not inferred.

**Retries may duplicate tool side effects.** This endpoint provides no
idempotency-key hook or exactly-once guarantee. Prefer read-only/idempotent tools
or implement deduplication in the business operation.

## Parameters, limits and errors

| Input | Behavior |
|---|---|
| `model` | Required registry name; unknown names return 404 |
| `messages` | Required full text history ending in a user input |
| `stream` | SSE when true; one JSON completion otherwise |
| `stream_options.include_usage` | An extra terminal usage chunk when usage is known; requires streaming |
| `tools`, `tool_choice` | Registered subsets, `auto` / `none` only |
| `temperature`, `top_p`, `max_tokens` | Validated and forwarded via model call options; provider support varies |
| `stop` | Validated string or 1–4 strings, then ignored: the current `CallOptions` has no stop field |
| `n` | Omitted or 1 only |
| Non-text content, `response_format`, `seed`, `logprobs`, other unsupported fields | 400; never silently dropped |

No server session, session-pinning header, client-executed tools, async callback,
or multimodal generation is implemented. `GET /v1/models` returns a sorted list
from `Config.Models`. The two routes are exact; mount a prefix with
`http.StripPrefix` if needed.

Zero config limits select a 4 MiB body cap, 32 concurrent replies and a
five-minute reply timeout. Negative limits are rejected. Body overflow returns
413. Saturation returns 429 with `Retry-After: 1`; model discovery does not
consume a reply slot. The timeout covers factory construction and execution,
and client disconnection cancels execution. Slots remain held until accepted
agent cores finish cleanup; a tool ignoring cancellation keeps its slot until
it returns. Slow client writes also need host-level write/deadline policy.

Errors have an `error` object with `message`, `type` and `code`. Invalid requests
return 400, unknown models 404, concurrency saturation 429, construction/internal
failures 500, provider failures 502, retryable provider failures 503, and reply
deadline expiry 504. Provider/factory error details are not reflected to clients.
Before text is published, failures use JSON with the appropriate HTTP status.
After streaming starts, failures use an SSE error envelope followed by `[DONE]`,
without a successful terminal choice or usage chunk. Successful SSE includes an
assistant-role chunk, incremental text deltas, a `stop` choice, optional usage,
and `[DONE]`. Host middleware must preserve flushing (or expose `Unwrap`).

Usage reports the final successful model call's provider-reported input count
(`EffectiveInputTokens`, including cached input) plus output tokens aggregated
over the reply's model calls by `turn_stats`. It is not a sum of all model input
tokens or a billing total. The final provider input count is captured directly
to avoid a stale `last_input_tokens` value. If the final call omits usage,
the completion/terminal usage chunk is omitted, even if an earlier call reported
some tokens. Explicit provider zero usage remains zero. Usage is finalized only
after the event stream closes because `turn_stats` may follow `ReplyEndEvent`.

## Offline demonstration

```sh
go run ./examples/openai_server -self-test
go run ./examples/openai_server
```

The example uses a simulated model and order tool, without API keys or a live
database. With the server running:

```sh
curl http://127.0.0.1:8080/v1/models
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"order-assistant","messages":[{"role":"user","content":"Where is order 123?"}],"stream":true}'
```

The protocol follows the supported subset of the
[Chat Completions reference](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create).
The Python upstream's
[OpenAI chat adapter](https://github.com/agentscope-ai/agentscope/blob/main/src/agentscope/model/_openai_chat/_model.py)
was inspected for text/tool and provider-usage conversion. It is an outbound
model adapter, not this inbound server; this Go handler is the stdlib design
agreed in [#82](https://github.com/agentscope-ai/agentscope-go/issues/82#issuecomment-6487314213).
