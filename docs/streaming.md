# Agent streaming

`UnifiedAgent.ReplyStream` streams provider text and thinking deltas by default
when no middleware is installed. Ordinary reasoning rounds and the forced final
summary use `model.ChatModel.ChatStream`; the final assembled response supplies
history and usage once. `Reply` drains the same path synchronously and returns
terminal reply errors instead of treating earlier tool-round content as success.

For a typical successful round, content events arrive between model-call start
and end:

```text
reply_start
model_call_start
thinking_block_start
thinking_block_delta ...
text_block_start
text_block_delta ...
thinking_block_end
text_block_end
model_call_end
reply_end
```

Providers determine which blocks arrive and their ordering. Blocks have stable
presentation IDs within a model operation. Anonymous text/thinking deltas are
coalesced by kind; explicitly identified blocks stay separate. History retains
the provider's final content and metadata, so presentation block IDs and grouping
need not match history. Data blocks are published from the final response. Tool
execution waits for a successful final response and the existing validation and
permission checks; partial tool arguments are never executed speculatively.

The final assembled text/thinking must agree with the published prefix. Only
unpublished suffixes or new blocks are emitted from it. A rewritten final response
is an error, not permission to silently replace already displayed content.

## Buffered compatibility

Use `agent.WithModelStreaming(false)` when constructing an agent to keep buffered
model calls. Installing any legacy middleware also keeps the buffered path:
`OnModelCall` can block, redact or replace a complete response, and raw deltas
must not escape before that check. This includes middleware that only observes
events; there is no automatic inference that a custom chain is safe to stream.

A model that returns `model.ErrStreamNotSupported` before streaming starts falls
back to its `Chat` method. Other setup errors retain bounded configured retries
and fallback. A proxy that accepts only non-streaming requests should use the
buffered option; arbitrary HTTP or parsing errors do not imply lack of streaming
support. Custom models must implement `ChatStream` or explicitly return the
unsupported sentinel; embedding a nil model interface is not an implementation.

The loop bridge, compression and structured-output helper calls retain their
existing buffered paths. A stream-aware middleware API is separate future work.

## Failure and cancellation

Once a stream is established, the agent does not retry or switch models after
failure. A terminal error, channel closure without a final response, inconsistent
final content, length-limited completion or content-filtered completion ends the
model operation with an error. The failing generation is not committed to Agent
history; earlier successful tool rounds remain. Already displayed text cannot be
retracted and must be treated as incomplete when `ReplyEndEvent` reports failure.

OpenAI-compatible providers must send `[DONE]` or a nonempty `finish_reason`;
clean EOF without either and malformed JSON chunks are reported as failures.
Gemini final responses retain tool calls and require a completion reason;
Anthropic stop reasons are propagated. Custom providers remain responsible for
honest terminal `IsLast` and `Error` values.

Consume until channel closure and inspect terminal events. If stopping early,
cancel the context passed to `ReplyStream` so sends and the upstream request can
stop. The agent cancels the per-attempt context on completion or error as well.
Cancellation can close the public channel before closing events are delivered;
inspect `ctx.Err()`. This cannot force a custom provider that ignores its context
to terminate or prove that a remote service has stopped computing.

Try [the streaming example](../examples/streaming/). Actual token latency also
depends on the model, proxy buffering and network. Local protocol fixtures verify
incremental delivery; they are not live-service latency benchmarks.
