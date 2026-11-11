# Model Providers

The adapters below implement the `ChatModel` interface with `Chat`, `ChatStream`, and `CountTokens`.

## Configuration Pattern

Every provider follows the same pattern:

```go
cm, err := model.NewXxxChatModel(model.XxxConfig{
    APIKey: "...",
    Model:  "model-name",
    // Optional:
    ClientOptions: &model.ClientOptions{
        Timeout:        120 * time.Second,
        DefaultHeaders: map[string]string{"X-Custom": "value"},
        Transport:      customTransport, // for proxy
    },
})
// Note: Anthropic, OpenAI Responses, Volcengine and MiniMax take pointer configs.
```

## Providers

### OpenAI

```go
cm, _ := model.NewOpenAIChatModel(model.OpenAIConfig{
    APIKey: os.Getenv("OPENAI_API_KEY"),
    Model:  "gpt-4o",
})
```

Models: `gpt-4o`, `gpt-4o-mini`, `gpt-4.1`, `gpt-4.1-mini`, `gpt-4.1-nano`, `gpt-5.4`, `gpt-5.5`, `gpt-audio-mini`, `o3`, `o4-mini`

### OpenAI Responses API

```go
cm, _ := model.NewOpenAIResponseModel(&model.OpenAIResponseConfig{
    APIKey:          os.Getenv("OPENAI_API_KEY"),
    Model:           "gpt-4.1",
    ReasoningEffort: "high",
})
```

Uses the Responses API (`/v1/responses`) instead of Chat Completions. Default timeout is 5 minutes for long reasoning tasks.

A response whose `status` is not `completed` (for example `incomplete` at
`max_output_tokens`, or `failed`) keeps its content, ID and usage and reports
the status on `ChatResponse.Error`, from `Chat` and on the final `ChatStream`
response. A stream ends at `response.completed`, `response.incomplete` or
`response.failed`. A response without a `status` is treated as complete.

### Anthropic

```go
cm, _ := model.NewAnthropicChatModel(&model.AnthropicConfig{
    APIKey:          os.Getenv("ANTHROPIC_API_KEY"),
    Model:           "claude-sonnet-4-20250514",
    MaxOutputTokens: 4096,
})
```

Models: `claude-opus-4-5`, `claude-opus-4-6`, `claude-opus-4-7`, `claude-opus-4-8`, `claude-sonnet-4-5`, `claude-sonnet-4-6`, `claude-haiku-4-5`

Extended thinking: `model.WithThinking(true, 10000)` enables thinking with a 10K token budget.

Anthropic responses retain `redacted_thinking` blocks (including their opaque
`data`) and thinking signatures in both buffered and streamed calls. Empty
thinking text with a nonempty signature is retained when formatting history;
empty unsigned thinking and empty text remain filtered. Return these blocks
unchanged when continuing tool use. This change is unreleased,
with local wire-fixture coverage; no live API validation is claimed.

The preservation follows the
[Python Anthropic formatter](https://github.com/agentscope-ai/agentscope/blob/d74b833943d84ba8e6b8440b0fd4d46bcdc59569/src/agentscope/formatter/_anthropic_formatter.py).
Go uses its existing `RedactedThinkingBlock` and `ThinkingBlock.Extra` signature
field rather than Python's thinking-block attributes. Existing nonempty unsigned
thinking behavior is unchanged; no new signature validation is introduced.

### DashScope (Alibaba Qwen)

```go
cm, _ := model.NewDashScopeChatModel(model.DashScopeConfig{
    APIKey:  os.Getenv("DASHSCOPE_API_KEY"),
    BaseURL: os.Getenv("DASHSCOPE_BASE_URL"), // optional override
    Model:   "qwen-plus",
})
```

Models: `qwen-long`, `qwen-plus`, `qwen3.5-omni-plus`, `qwen3.5-plus`, `qwen3.6-max-preview`, `qwen3.6-plus`, `qwen3.7-max`, `qwen3.7-plus`

GLM models via DashScope: `glm-5.2` (131K context, 8K output)

### DeepSeek

```go
cm, _ := model.NewDeepSeekChatModel(model.DeepSeekConfig{
    APIKey: os.Getenv("DEEPSEEK_API_KEY"),
    Model:  "deepseek-chat",
})
```

Models: `deepseek-chat`, `deepseek-reasoner`, `deepseek-v4-flash`, `deepseek-v4-pro`

### Google Gemini

```go
cm, _ := model.NewGeminiChatModel(model.GeminiConfig{
    APIKey: os.Getenv("GEMINI_API_KEY"),
    Model:  "gemini-2.5-flash",
})
```

Models: `gemini-2.5-flash`, `gemini-2.5-pro`, `gemini-3-flash-preview`, `gemini-3.1-pro-preview`

### Ollama (Local)

```go
cm, _ := model.NewOllamaChatModel(model.OllamaConfig{
    BaseURL:     "http://localhost:11434", // default
    Model:       "llama4",
    ContextSize: 8192, // the server's num_ctx; not sent to the server
})
```

No API key required. Models: `deepseek-r1-14b`, `llama4`, `qwen3-14b`, `qwen3.5-9b`

`ContextSize` tells context compression the window the server runs with; see
[edge deployment](edge-deployment.md#match-the-agents-context-size-to-the-servers-window).

Thinking models return their reasoning as thinking blocks when Ollama sends
it in the `reasoning` field (Ollama 0.11 or later). The adapter sends no think
control, so the server default applies. From Ollama 0.12.4, models such as
`qwen3` and `deepseek-r1` think by default. Ollama 0.11 to 0.12.3 fill the
field only for models such as `gpt-oss`; other models' reasoning stays inline
in the text. `model.WithSeed` is sent as `seed`.

A separate adapter talks to Ollama's native `/api/chat` endpoint:

```go
cm, _ := model.NewOllamaNativeChatModel(model.OllamaNativeConfig{
    BaseURL: "http://localhost:11434", // default
    Model:   "llama4",
    NumCtx:  8192, // sent as options.num_ctx on every request
})
```

`NumCtx` sizes the server's window per request and is also reported through
`ContextSizer`, so context compression aligns with the server without a
Modelfile. The native adapter also maps `model.WithThinking` to the `think`
toggle, supports native structured output via `model.WithResponseFormat`
(`"json"` or an embedded schema object), and reports usage from the server's
actual token counts. Tool calling is supported: `model.WithTools` maps to the
native `tools` field; `model.WithToolChoice` has no native counterpart, so its
tool whitelist filters the sent tools while a non-default mode is ignored with
a warning.

### Moonshot (Kimi)

```go
cm, _ := model.NewMoonshotChatModel(model.MoonshotConfig{
    APIKey: os.Getenv("MOONSHOT_API_KEY"),
    Model:  "kimi-k2.6",
})
```

Models: `kimi-k2.5`, `kimi-k2.6`, `kimi-k3`, `moonshot-v1-8k`, `moonshot-v1-32k`, `moonshot-v1-128k`

**Kimi K3** — Latest Kimi model with 256K context, 64K output. Supports text, vision (JPEG/PNG/GIF/WebP), video (MP4), and extended thinking.

### xAI (Grok)

```go
cm, _ := model.NewXAIChatModel(model.XAIConfig{
    APIKey: os.Getenv("XAI_API_KEY"),
    Model:  "grok-3",
})
```

Models: `grok-3`, `grok-3-fast`, `grok-3-mini`, `grok-4.3`

### Volcengine Ark

Available on main (Unreleased), through direct Go construction:

```go
cm, err := model.NewVolcengineChatModel(&model.VolcengineConfig{
    SecretAPIKey: model.NewSecretStr(os.Getenv("ARK_API_KEY")),
    Model:        os.Getenv("ARK_MODEL"), // Your enabled model ID.
})
```

The default API prefix is `https://ark.cn-beijing.volces.com/api/v3`; the adapter
appends `/chat/completions`. A custom `BaseURL` must include its version/path
prefix, with an optional trailing slash. Do not pass a full chat URL. Bearer
API-key authentication is used; `SecretAPIKey` takes precedence over `APIKey`.
`HTTPClient` and `ClientOptions` follow the shared configuration rules.
The default targets the standard Ark data plane; Coding Plan uses different
endpoints and must be configured according to its own service documentation.

Both `Chat` and `ChatStream` support text and tool calls. Options map as follows:

| Call option | Ark request |
|---|---|
| Thinking unset | Omit `thinking`; use the provider default |
| `WithThinking(true, 0)` | `thinking.type = "enabled"` |
| `WithThinkingDisabled()` or `WithThinking(false, 0)` | `thinking.type = "disabled"` |
| `WithReasoningEffort("low")` | Top-level `reasoning_effort` |
| `WithMaxTokens(n)` | `max_tokens` |
| Temperature, top-p, tools, tool choice | Corresponding OpenAI-compatible fields |

Positive thinking budgets are rejected before dispatch; use zero for the toggle.
This differs from Anthropic's budget-based thinking. The effort values exposed by
the [Python Ark adapter](https://github.com/agentscope-ai/agentscope/blob/9c006f3865c53a3769a4c9411b3b7b4b579195e6/src/agentscope/model/_volcengine/_model.py)
are `minimal`, `low`, `medium` and `high`. Values are forwarded without rewriting;
model-specific restrictions on effort, thinking and tool choice remain provider
errors. See [Ark thinking controls](https://docs.volcengine.com/docs/ark/deep-thinking?lang=zh)
and [endpoint/authentication](https://docs.volcengine.com/docs/ark/base-url-and-authentication?lang=zh).
Other shared options (including seed, voice, response format and additional
`WithRetries` attempts) are not mapped by this adapter. Buffered calls still use
the shared HTTP helper's retry policy; streaming has no setup retry loop.

Responses retain text, reasoning, tool calls, stop reasons, usage and cache-read
tokens. `CountTokens` remains an estimate. Streams emit deltas and an assembled
`IsLast` response; inspect each chunk's `Error`, and check `ctx.Err()` after channel
closure. Do not append final assembled text to the deltas again. Supply a context
deadline and cancel when abandoning a stream; see [streaming](streaming.md).

This slice does not add `provider: volcengine` to the credential/app factory,
Responses API, media support, AK/SK signing or model/pricing cards. Construct the
adapter directly. The [runnable example](../examples/volcengine/) requires a real
API key and model ID. Adapter tests use local HTTP/SSE fixtures; they do not
establish live Ark compatibility.

### MiniMax

Direct Go construction for MiniMax's Anthropic-compatible Messages API:

```go
cm, err := model.NewMiniMaxChatModel(&model.MiniMaxConfig{
    SecretAPIKey:    model.NewSecretStr(os.Getenv("MINIMAX_API_KEY")),
    Model:          os.Getenv("MINIMAX_MODEL"),
    MaxOutputTokens: 4096,
})
```

The default prefix is `https://api.minimax.io/anthropic`; the adapter appends
`/v1/messages`. A custom `BaseURL` is the complete prefix with an optional
trailing slash, not the full Messages URL. `SecretAPIKey` takes precedence over
`APIKey`. Requests use Bearer authentication; required authentication/content-type
headers take precedence over default headers regardless of casing. Custom
`HTTPClient`/`ClientOptions` follow the shared configuration rules.

| Option | MiniMax request |
|---|---|
| Thinking unset | Omit `thinking`; retain model default |
| `WithThinking(true, 0)` | `thinking.type = "adaptive"`, without a token budget |
| `WithThinkingDisabled()` / `WithThinking(false, 0)` | `thinking.type = "disabled"` |
| `WithReasoningEffort(value)` | `output_config.effort` |
| `WithMaxTokens(n)` | Override configured `max_tokens` for this call |
| Tools / tool choice | Anthropic-format tools and tool selection |

`MaxOutputTokens` defaults to 4096; a negative configuration value or nonpositive
per-call limit is rejected. Thinking consumes the output limit too: generation
can end with reasoning but no text. Positive thinking budgets are rejected before
dispatch; use zero for the toggle. `max_tokens` stop reasons normalize to
`model.StopReasonLength` in both call paths.

According to the [official compatibility guide](https://platform.minimax.io/docs/api-reference/text-anthropic-api),
M3 defaults to thinking off and allows disabling, while M3.1-Flash thinks by
default and rejects disabling. Flash effort values include `low`, `medium`,
`high`, `xhigh` and `max`. The adapter forwards effort without rewriting it;
model-specific rejection remains observable. Structured-output fallback may try
disabled thinking, receive a rejection, then proceed to its next strategy; it
does not establish that a model can stop thinking.

The official overview describes tool choice as supported, while the detailed
[Messages reference](https://platform.minimax.io/docs/api-reference/text-chat-anthropic)
lists only `auto`. This adapter converts `auto`, `required` and named choices
to Anthropic shapes, preserving service rejections. Local wire tests establish
serialization, not acceptance by a live model. Use `auto` for the example.
Unsupported sampling controls, seed, voice, response format and additional
`WithRetries` attempts are not mapped. Buffered requests retain the shared HTTP
helper's retry policy; streaming has no setup retry loop. Anthropic prompt-cache
control markers are not added.

Preserve the full assistant reply (thinking, optional signature, text and tool
calls) before adding tool results. MiniMax also expands merged AgentScope
assistant histories into assistant/user-result messages for the provider,
preserving block order without changing the stored history.
The [runnable example](../examples/minimax/)
demonstrates this with local tool data and real model calls. Streams emit deltas
followed by an assembled `IsLast` response; inspect `Error` on every chunk and
`ctx.Err()` after closure. Do not append final assembled text to deltas again.
Use a context deadline and cancel if abandoning the stream.

Provider usage is distinct from estimated `CountTokens`. MiniMax's
[cache guide](https://platform.minimax.io/docs/api-reference/text-prompt-caching)
defines `input_tokens` as new input, with cached input reported separately.
The adapter retains those raw counts and reports effective total input as new
input plus cache-read/cache-creation tokens. Reported zero/cache-only usage and
optional trailing stream usage updates are retained.

The adaptive-thinking mapping follows [Python MiniMaxChatModel](https://github.com/agentscope-ai/agentscope/blob/f37adb1d467b7801cde830ddf2ac5cf5c5fd03bd/src/agentscope/model/_minimax/_model.py).
Effort support follows current provider docs and goes beyond that wrapper,
which suppresses output configuration. This slice covers direct text/tool
construction: app credential registration, media, other MiniMax APIs and model
cards/pricing catalogs are deferred. Tests use local fixtures; live MiniMax
compatibility has not been validated.

## Tool-call history in Anthropic Messages

On main (unreleased), the Anthropic formatter expands merged assistant history
into ordered assistant and user messages. Consecutive tool results share a user
message, keeping all calls from the round before their results; subsequent
assistant content starts another segment. Stored messages are not mutated.
`AnthropicFormatter.Format` and `FormatMultiAgent` may therefore return more
messages than they receive. Multi-agent sender labels are added only to the
sender's own content, never before tool results.

This fixes the continuation after `UnifiedAgent` executes tools; callers of the
formatter must still provide matching IDs and a valid call/result sequence.
Thinking preservation and other provider options are separate work in #125.
The boundary handling follows the
[Python Anthropic formatter](https://github.com/agentscope-ai/agentscope/blob/d74b833943d84ba8e6b8440b0fd4d46bcdc59569/src/agentscope/formatter/_anthropic_formatter.py).
Go retains its existing typed-block conversion and renders before splitting so
filtered empty or unsupported blocks do not split consecutive tool results.
Validation uses local HTTP fixtures, not a live Anthropic request.

## Tool-call history in OpenAI-compatible chat

**Unreleased fix for [#7](https://github.com/agentscope-ai/agentscope-go/issues/7):**
the formatter now expands a merged agent reply into an assistant message with
`tool_calls`, one `role: "tool"` message per result, and any subsequent assistant
messages. Versions v2.0.10 and v2.0.11 could keep only the last tool result from
such a reply, causing OpenAI or DeepSeek to reject the next request with a missing
preceding `tool_calls` error.

This follows the result-boundary splitting used by the
[Python OpenAI formatter](https://github.com/agentscope-ai/agentscope/blob/5ff52f877de12d66a30d55af279dd4f42b1590f3/src/agentscope/formatter/_openai_formatter.py#L343).
The Go fix retains the existing provider-specific text, thinking and media
conversion. Agent state and checkpoint formats are unchanged. Direct users of
`OpenAIFormatter.Format` or `FormatMultiAgent` should allow multiple output
messages per input message. Custom histories must still supply matching tool-call
IDs and results in the correct order.

## Text-to-Speech (TTS)

agentscope-go includes TTS models for audio synthesis, usable standalone or via `TTSMiddleware`.

### DashScope TTS

Standard and realtime (CosyVoice) TTS:

```go
ttsModel, _ := tts.NewDashScopeTTSModel(tts.DashScopeConfig{
    APIKey: os.Getenv("DASHSCOPE_API_KEY"),
    Model:  "cosyvoice-v3-flash",
    Voice:  "longxiaochun",
})
audio, _ := ttsModel.Synthesize(ctx, "Hello world")
```

Models: `cosyvoice-v1`, `cosyvoice-v2`, `cosyvoice-v3-flash`, `cosyvoice-v3-plus`, `qwen3-tts-flash`, `qwen3-tts-flash-realtime`

### OpenAI TTS

```go
ttsModel, _ := tts.NewOpenAITTSModel(&tts.OpenAITTSConfig{
    APIKey: os.Getenv("OPENAI_API_KEY"),
    Model:  "tts-1-hd",
    Voice:  "alloy",
})
```

Models: `tts-1`, `tts-1-hd`

### Gemini TTS

Uses the Gemini generateContent API with audio response modality:

```go
ttsModel, _ := tts.NewGeminiTTSModel(tts.GeminiTTSConfig{
    APIKey:       os.Getenv("GEMINI_API_KEY"),
    Model:        "gemini-2.5-flash-preview-tts",
    Voice:        "Kore",
    SpeakingRate: 1.0,
})
audio, _ := ttsModel.Synthesize(ctx, "Hello from Gemini TTS")
```

Models: `gemini-2.5-flash-preview-tts`

## Common Call Options

All providers accept the common `CallOption` type; supported options and mappings
vary by adapter. Consult the provider sections above:

```go
resp, _ := cm.Chat(ctx, msgs,
    model.WithTemperature(0.7),
    model.WithMaxTokens(1024),
    model.WithTopP(0.9),
    model.WithTools(toolSchemas),
    model.WithToolChoice(&model.ToolChoice{Mode: "auto"}),
    model.WithThinking(true, 5000),
    model.WithReasoningEffort("high"),
    model.WithVoice("alloy"),
    model.WithRetries(3, time.Second),
    model.WithSeed(42), // passed through on OpenAI-family providers
)
```

For `OpenAIChatModel.Chat` and `AnthropicChatModel.Chat`, `model.WithRetries`
configures additional attempts and the delay between them. Their retry waits
honor the caller's context: cancellation and deadlines return errors matching
`context.Canceled` and `context.DeadlineExceeded`, respectively.

## Fallback Model

Automatic failover from a primary to a backup provider:

```go
cm := model.NewFallbackChatModel(primaryModel, fallbackModel)
```

## Structured Output

Force any model to produce JSON matching a schema:

```go
result, _ := model.GenerateStructuredOutput(ctx, cm, msgs, jsonSchema)
```

Use `model.GenerateStructuredOutputWithUsage` when the call's tokens must land in
a budget or a cost ledger. It returns `(json.RawMessage, *ChatUsage, error)` and
accumulates usage across strategy attempts such as retries and fallbacks, instead
of reporting only the last one. Agent-driven compression uses it, and the reply
loop re-emits the total as a `model_call_end` event so budgets and cost tracking
include compression spend.

## Model Cards

Query bundled model metadata:

```go
card, _ := model.GetModelCard("claude-sonnet-4-6")
// card.Name, card.Provider, card.ContextSize, card.OutputSize

models := model.ListModels()          // all 78 model cards
models = model.ListModels("openai")   // filter by provider
```

78 model cards + 9 TTS model cards are bundled and accessible at runtime via `//go:embed`.

## Model Card Summary

| Provider | Chat Models | Embedding Models | TTS Models |
|----------|------------|------------------|------------|
| OpenAI | 13 | 2 | 2 |
| Anthropic | 10 | — | — |
| DashScope | 15 (incl. GLM-5.2) | 2 | 6 |
| DeepSeek | 4 | — | — |
| Gemini | 9 | 2 | 1 |
| Moonshot | 8 (incl. Kimi K3) | — | — |
| Ollama | 4 | — | — |
| xAI | 9 | — | — |
| **Total** | **72** | **6** | **9** |

72 chat + 6 embedding = the 78 bundled cards under `pkg/agentscope/model/models/`.
The 9 TTS cards are a separate embed under `pkg/agentscope/tts/models/`. Verify
with `find pkg/agentscope/model/models -name '*.yaml' | wc -l`.
