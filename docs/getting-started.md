# Getting started

## Requirements and installation

Use Go 1.25 or newer. The example below needs an Anthropic API key and a model
ID available to that account. You can choose a different adapter using the
[provider guide](model-providers.md).

Install the latest tagged release from the community module path. Commit
`go.mod` and `go.sum` with your application. If you use the former
`github.com/alanfokco/agentscope-go/v2` path, update your imports as described in
the [migration notes](../CHANGELOG.md#changed--repository-and-module-path-move).

```bash
mkdir agentscope-demo
cd agentscope-demo
go mod init example.com/agentscope-demo
go get github.com/agentscope-ai/agentscope-go/v2@latest
```

## Create an agent

Save this as `main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func main() {
	cm, err := model.NewAnthropicChatModel(&model.AnthropicConfig{
		SecretAPIKey:    model.NewSecretStr(os.Getenv("ANTHROPIC_API_KEY")),
		Model:           os.Getenv("ANTHROPIC_MODEL"),
		MaxOutputTokens: 1024,
	})
	if err != nil {
		log.Fatal(err)
	}

	assistant := agent.NewUnifiedAgent(
		"assistant", "You are a helpful assistant. Keep answers concise.", cm,
	)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	reply, err := assistant.Reply(ctx, "What is an AI agent? Explain in one sentence.")
	if err != nil {
		log.Fatal(err)
	}
	if text := reply.GetTextContent("\n"); text != nil {
		fmt.Println(*text)
	}
}
```

Set a real key and a model ID available to your account. The placeholders below
must be replaced. These commands use Bash/Zsh; in PowerShell use
`$env:NAME = 'value'` for environment variables.

```bash
export ANTHROPIC_API_KEY='your-api-key'
export ANTHROPIC_MODEL='your-model-id'
go mod tidy
go run .
```

This program explicitly reads the environment variables and passes them to the
adapter. The library does not automatically select a provider or model from
those variables. The context bounds this example's reply; HTTP client timeouts
can impose a shorter limit. See the adapter's `ClientOptions` and `HTTPClient`
configuration when changing that budget.

## Add a function tool

In the program above, add `encoding/json` and
`github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool` to the imports.
Before constructing the agent, define a function tool:

```go
clockTool := tool.NewFunctionTool(
    "get_utc_time", "Get the current time in UTC.",
    json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
    func(_ context.Context, _ map[string]any) (any, error) {
        return time.Now().UTC().Format(time.RFC3339), nil
    },
)
```

Replace the agent construction with:

```go
assistant := agent.NewUnifiedAgent(
    "assistant", "Use get_utc_time when asked for the current time.", cm,
    agent.WithToolkit(tool.NewToolkit(clockTool)),
)
```

Use `"What time is it in UTC? Use the tool."` as the input to `Reply`. The model
chooses when to call the tool, so use a model with tool-calling support. For more
examples and permission configuration, see [Tools](tools.md) and
[agent_v2](../examples/agent_v2/).

## Events and model streaming

`UnifiedAgent.ReplyStream` exposes lifecycle events for a reply, including tool
execution and confirmation events. It currently makes non-streaming model calls
internally. See [streaming](../examples/streaming/) for the event API and
[console](../examples/console/) for interactive tool confirmation.

For provider response chunks, use `ChatModel.ChatStream`; see
[model_call](../examples/model_call/). Check each response's `Error`, handle
channel closure and cancellation, and avoid appending final assembled content
to already collected deltas.

## Run repository examples

Examples require a checkout; `go get` does not put the repository's demos in your
application directory. From a separate working directory:

```bash
git clone https://github.com/agentscope-ai/agentscope-go.git
cd agentscope-go
go run ./examples/agent_pool
```

This demo uses simulated jobs and needs no API key. Other examples have their
own model choices and environment or service requirements. Consult the
[examples guide](examples.md) and the source of the demo you want to run.

## Next steps

- [Model providers](model-providers.md) — Configure another provider or a local server.
- [Tools](tools.md) — Add functions and configure tool permissions.
- [Middleware](middleware.md) — Extend model calls and agent behavior.
- [Deployment](deployment.md) — Integrate with an HTTP service or workspace backend.
- [Stability and limits](../STABILITY.md) — Check compatibility and deployment boundaries.
