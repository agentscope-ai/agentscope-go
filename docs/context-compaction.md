# Experimental content-block compaction

`agent.WithContentBlockCompaction()` opts a UnifiedAgent into retaining recent
complete tool exchanges inside an accumulated assistant message. Use it with
`WithContextConfig`; without context compression configured it has no effect.
The default splitter and required interfaces remain unchanged.

```go
a := agent.NewUnifiedAgent("analyst", "Analyze diagnostic logs.", cm,
    agent.WithContextConfig(&agent.ContextConfig{
        ContextSize:     4096,
        TriggerRatio:    0.8,
        ReserveRatio:    0.3,
        ToolResultLimit: 1200,
    }),
    agent.WithContentBlockCompaction(),
)
```

The [compiled API example](../pkg/agentscope/agent/compress_blocks_example_test.go)
shows the imports and construction. This is an opt-in experiment from
[RFC #11](https://github.com/agentscope-ai/agentscope-go/issues/11), scoped in
[#53](https://github.com/agentscope-ai/agentscope-go/issues/53).

## Background pre-compression

`ContextConfig.PrecompressRatio` (opt-in, normalized to 0 when not below
`TriggerRatio`) hides summarization latency: once the estimated context
crosses `ContextSize*PrecompressRatio`, the agent generates the compaction
summary in a background goroutine from a context snapshot. When the hard
trigger later fires — or the host calls `Compact` — a still-valid result
applies instantly; stale results (checkpoint rollback, an interleaved
compaction) are discarded and the synchronous path runs as before. At the
automatic trigger, and when the `compress_context` tool compresses at its
lower threshold, the synchronous path also runs when the context is still at
that threshold after the result went in. Forced compactions (`Compact` and
the compaction of context-overflow recovery) apply the result alone.

The summary covers the snapshot prefix: messages appended since the snapshot
stay raw, so the window refills somewhat sooner after an applied
pre-compression than after a synchronous one. Background generations burn
tokens and are accounted like synchronous ones (per-reply usage and a
model-call-end event). A failed background generation, or one that formats
to a blank summary, is discarded without an immediate retry — the
synchronous path owns retries and the truncation fallback. Each later round
past the precompress ratio starts a new generation, so a model that keeps
failing or returning blank fields costs one background call per round. The
synchronous path does not reject a blank summary: such a model still ends
with one at the trigger.
Pre-compression is unavailable with `WithContentBlockCompaction`.

## Retention policy

Successive model/tool rounds can share one assistant message. The default
splitter works at message boundaries; the experiment also considers boundaries
immediately after completed tool results inside this agent's assistant messages.
It selects the largest eligible recent suffix within the estimated reserve
target, keeping the next exchange's preceding thinking, text and media intact.
Individual content blocks and tool-result outputs are indivisible.

This prelude can span adjacent assistant messages belonging to this agent,
including imported or restored histories with signed or redacted thinking in a
separate message before the tool call. A nonempty message with another role or
agent name ends that association. Empty messages are treated conservatively and
do not separate the prelude from its call. The complete prelude travels with the
call/result group; if it cannot fit, the existing wholesale fallback applies.

Call/result intervals are checked across the whole context. Interleaved calls
such as `call-a, call-b, result-a, result-b` stay together, including when spread
across messages. Success, error, denied and interrupted results are terminal;
a running result is not. Calls with no terminal result remain pinned during
normal splitting, regardless of their recorded call state. This also protects
an in-flight `compress_context` call when combined with
[`WithAgentDrivenCompression`](tools.md#agent-driven-compression-upstream-2143).

The target includes estimates for the system prompt, existing summary and tool
schemas. It does not include a summary that has not yet been generated, and
`CountTokens` remains adapter-specific estimation. Pinned groups can exceed the
target. If no complete group fits, or no compressible prefix remains, the
existing wholesale emergency fallback still applies, including its ability to
remove unfinished calls. Ambiguous repeated call IDs also use that fallback.
This option does not impose a strict context, token, quality or cost bound.

## State and recovery

Planning uses owned message/content containers before replacement. Identity,
role/name, timestamps, retained reasoning/media metadata, tool state and tool
metadata are preserved. A split message's historical usage belongs only to its
retained fragment, with an independent usage object for subsequent rounds.
Compression-call usage is still accounted separately.

An ordinary summary failure uses the existing truncation fallback with the
previous summary and the planned retained suffix. Cancellation prevents publishing
the split or cleaning its read cache. Existing image limiting can already have
run before splitting. Read-cache cleanup follows the retained `Read` calls.

Compression and checkpoints run at their existing safe points. Checkpoint schemas
are unchanged: legacy writes use schema 1, opt-in reply recovery uses schema 2.
Reapply the option when reconstructing an agent; it is configuration, not a new
checkpoint field. `ResumeReplyStream` resumes the recorded context and pending
tools. See [reply recovery](reply-recovery.md) for persistence and side-effect
limits; this option does not make tool execution exactly once across crashes.

## Reference and validation limits

Python AgentScope's [boundary-content splitter](https://github.com/agentscope-ai/agentscope/blob/9b94a63407ec59881fde19300b61e469a0a7d8b2/src/agentscope/agent/_agent.py#L2873-L3026)
is the reference. Go deliberately limits interior cuts to completed exchanges
and prefers retaining recent complete groups when they fit. It does not copy
Python's API or promise identical boundaries.

Offline public `Reply`/`ReplyStream` fixtures use a scripted model with the real
OpenAI adapter's token estimator. Direct fixtures cover parallel and cross-message
groups, unfinished calls, ownership, failures, cancellation, cache cleanup and
checkpoint recovery. Real-adapter HTTP/SSE captures inspect retained call/result
relationships for OpenAI and Anthropic, including Anthropic signed thinking.

The existing Anthropic formatter keeps tool results inside the assistant role
when given a merged assistant history. The capture test checks IDs and signatures
for that shape; it does not establish valid Anthropic protocol. Separated tool
messages additionally check the result's user role. No live provider was called;
fixture responses establish neither provider acceptance nor task quality,
latency improvements or savings.
