# Getting Started

## Prerequisites

- **Go 1.25+**
- An API key from at least one supported model provider

## Installation

```bash
go get github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope
```

## Your First Agent

```go
package main

import (
    "context"
    "fmt"

    as "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func main() {
    as.Init()

    cm, err := model.NewDashScopeChatModel(model.DashScopeConfig{
        APIKey: "sk-...",
        Model:  "qwen-plus",
    })
    if err != nil {
        panic(err)
    }

    a := agent.NewUnifiedAgent("assistant", "You are a helpful AI assistant.", cm)

    reply, err := a.Reply(context.Background(), "Hello! What can you do?")
    if err != nil {
        panic(err)
    }

    if txt := reply.GetTextContent("\n"); txt != nil {
        fmt.Println(*txt)
    }
}
```

## Adding Tools

Give your agent the ability to call functions:

```go
weatherTool := tool.NewFunctionTool(
    "get_weather", "Get current weather for a city",
    json.RawMessage(`{
        "type": "object",
        "properties": {"city": {"type": "string"}},
        "required": ["city"]
    }`),
    func(ctx context.Context, input map[string]any) (any, error) {
        city, _ := input["city"].(string)
        return map[string]any{"city": city, "temp": "22°C"}, nil
    },
)

a := agent.NewUnifiedAgent("bot", "You are a weather assistant.", cm,
    agent.WithToolkit(tool.NewToolkit(weatherTool)),
    agent.WithReactConfig(agent.ReactConfig{MaxIters: 5}),
)
```

## Streaming Responses

Get real-time output as the model generates:

```go
ch, _ := a.ReplyStream(ctx, "Tell me a story.")
for evt := range ch {
    switch e := evt.(type) {
    case event.TextBlockDeltaEvent:
        fmt.Print(e.Delta)
    case event.ReplyEndEvent:
        fmt.Println()
    }
}
```

## Environment Variables

Set one of these API keys to get started:

| Variable | Provider |
|----------|----------|
| `DASHSCOPE_API_KEY` | Alibaba DashScope (Qwen) |
| `ANTHROPIC_API_KEY` | Anthropic (Claude) |
| `OPENAI_API_KEY` | OpenAI (GPT) |
| `DEEPSEEK_API_KEY` | DeepSeek |
| `GEMINI_API_KEY` | Google Gemini |
| `MOONSHOT_API_KEY` | Moonshot (Kimi) |
| `XAI_API_KEY` | xAI (Grok) |

Optional: `DASHSCOPE_BASE_URL` to override the DashScope endpoint.

## Running Examples

The project includes 54 examples. Run any of them:

```bash
export DASHSCOPE_API_KEY=sk-...
go run ./examples/agent_v2          # Tool calling
go run ./examples/streaming         # Streaming events
go run ./examples/multimodal        # Image input
go run ./examples/model_call        # Raw model API
go run ./examples/replay            # Deterministic replay
go run ./examples/agent_pool        # Agent pool fan-out
```

See [docs/examples.md](docs/examples.md) for the full list.

## Next Steps

- [Architecture](docs/architecture.md) — Understand the package structure
- [Model Providers](docs/model-providers.md) — Configure different LLM backends
- [Tools](docs/tools.md) — Built-in tools, custom function tools, and document parsers
- [Middleware](docs/middleware.md) — Intercept and extend agent behavior (7 hooks)
- [Deployment](docs/deployment.md) — Run as an HTTP service, workspace sandboxing, agent pools
- [Go Runtime Features](docs/go-exclusive.md) — Capabilities this Go repository provides

## Go Runtime Features (Quick Reference)

These are capabilities provided by this Go repository. This quick reference does not assert feature exclusivity against any particular Python release. See [Go Runtime Features](docs/go-exclusive.md) for the full wording and code examples:

| Feature | Package | Description |
|---------|---------|-------------|
| **Deterministic Replay** | `replay` | Record model calls, replay in CI without API keys |
| **Fan-out Agent Pool** | `runtime` | Worker pool for parallel batch processing (`NewPool` handler-based; `NewAgentPool` needs an `agent.Agent` adapter) |
| **Hot-Reload Config** | `hotreload` | Watch config files, apply changes without restart |
| **WASM Sandbox** | `wasm` | Execute WebAssembly modules with resource limits |
| **TCP Agent Mesh** | `a2a/grpc` | Direct TCP communication between distributed agents |
| **Agent Load Testing** | `bench` | Benchmark agents with concurrency and latency metrics |

See [Go Runtime Features](docs/go-exclusive.md) for detailed documentation with code examples.
