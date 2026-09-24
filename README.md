<p align="center">
  <img src="https://img.alicdn.com/imgextra/i1/O1CN01nTg6w21NqT5qFKH1u_!!6000000001621-55-tps-550-550.svg" alt="AgentScope" width="120" />
</p>

# AgentScope Go

[![CI](https://github.com/agentscope-ai/agentscope-go/actions/workflows/ci.yml/badge.svg)](https://github.com/agentscope-ai/agentscope-go/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go)](go.mod)
[![License](https://img.shields.io/badge/License-Apache--2.0-blue)](LICENSE)

[Español](README.es-ES.md) · [Quick start](#quick-start) · [Examples](#examples) · [Documentation](#documentation) · [Contributing](#contributing)

AgentScope Go is a Go library for building LLM applications with agents, tools
and multi-agent workflows. It implements the core concepts of
[AgentScope](https://github.com/agentscope-ai/agentscope) through Go interfaces,
contexts and channels, and can be embedded in a service or command-line program.

## What you can build

- **Tool-using assistants:** connect model calls to Go functions, manage context
  and request human confirmation for tool execution. Use [AskUser](docs/tools.md#askuser)
  to collect structured choices through your own frontend.
- **Multi-agent workflows:** coordinate agents with pipelines, message routing
  and leader/worker teams.
- **Applications with memory:** combine retrieval, conversation state and memory
  middleware with your own data sources.
- **Services and experiments:** expose an HTTP service or browser UI, record
  model responses, evaluate runs and inspect traces.

## Quick start

You need **Go 1.25+** and, for the program below, an Anthropic API key and an
available model ID. Other adapters are listed under [Model providers](#model-providers).

### Install in your application

Install the latest tagged release from the community module path. Applications
using `github.com/alanfokco/agentscope-go/v2` need to update their import prefix;
see the [module migration notes](CHANGELOG.md#changed--repository-and-module-path-move).

From a new directory:

```bash
mkdir agentscope-demo
cd agentscope-demo
go mod init example.com/agentscope-demo
go get github.com/agentscope-ai/agentscope-go/v2@latest
```

### Create an agent

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

Set your key and a model ID available to your account, then run the program.
These shell commands use Bash/Zsh syntax; in PowerShell, set environment variables
with `$env:NAME = 'value'`.

```bash
export ANTHROPIC_API_KEY='your-api-key'
export ANTHROPIC_MODEL='your-model-id'
go mod tidy
go run .
```

The program makes a model API request and prints its reply. To add function
calling, see [the tool example](examples/agent_v2/); for configuration and next
steps, see [Getting started](docs/getting-started.md).

## Model providers

Adapters are available for **OpenAI** (Chat Completions and Responses),
**Anthropic**, **DashScope**, **DeepSeek**, **Gemini**, **Moonshot**, **xAI** and
**Ollama**. See [provider configuration](docs/model-providers.md) and the
[adapter source](pkg/agentscope/model/) for options and defaults.

Tool calling, multimodal input, thinking and usage reporting depend on the
provider and model. Provider `ChatStream` methods expose response chunks;
`UnifiedAgent.ReplyStream` exposes lifecycle events and currently uses
non-streaming model calls internally.

For local models, configure the context window and request timeout for your
server and device. Model cards describe model capabilities; they do not configure
the server. See [edge deployment](docs/edge-deployment.md) and
[the tracked local-model limitations](https://github.com/agentscope-ai/agentscope-go/issues/8).

For agents sharing an inference service, see [managed inference](docs/managed-inference.md)
for opt-in admission, physical request accounting and explicit reply recovery.

## Examples

Examples run from a repository checkout, separately from the application above.
This worker-pool demo uses simulated jobs and needs no model API key:

```bash
git clone https://github.com/agentscope-ai/agentscope-go.git
cd agentscope-go
go run ./examples/agent_pool
```

Start with [agent_v2](examples/agent_v2/) for function tools,
[model_call](examples/model_call/) for direct model streaming, or
[webui](examples/webui/) for a browser interface. Check each example's source for
its model choice, environment variables and required services or runtimes.

<details>
<summary>Browse all examples</summary>

| Example | Demonstrates |
|---|---|
| [simple](examples/simple/) | A custom agent with a single model call |
| [agent_v2](examples/agent_v2/) | UnifiedAgent with a function tool |
| [react_tool](examples/react_tool/) | A custom FunctionTool |
| [react_builtin_tools](examples/react_builtin_tools/) | The built-in coding toolkit |
| [streaming](examples/streaming/) | Agent lifecycle events |
| [ask_user](examples/ask_user/) | Structured questions with a simulated model and host answer |
| [console](examples/console/) | Terminal chat and tool-call confirmation |
| [model_call](examples/model_call/) | Direct model streaming, tool calls and structured output |
| [structured_output](examples/structured_output/) | Structured output through tool calling |
| [multi_provider](examples/multi_provider/) | Provider configuration and model cards |
| [multimodal](examples/multimodal/) | Image input using URLs and base64 data |
| [multiagent](examples/multiagent/) | Multi-agent conversation |
| [multiagent_multimodal](examples/multiagent_multimodal/) | Multi-agent conversation with image input |
| [openai_response](examples/openai_response/) | OpenAI Responses API |
| [middleware](examples/middleware/) | Model-call and tool-execution hooks |
| [permission](examples/permission/) | Tool permission modes |
| [tracing](examples/tracing/) | Tracing with LoggerTracer |
| [tracing_otlp](examples/tracing_otlp/) | An OTLP integration setup pattern |
| [agent_loop](examples/agent_loop/) | Loop configuration and metrics |
| [embedding](examples/embedding/) | Text embeddings and similarity |
| [long_term_memory](examples/long_term_memory/) | Long-term memory middleware |
| [agentic_memory](examples/agentic_memory/) | File-based memory and MEMORY.md |
| [rag_react](examples/rag_react/) | Retrieval with an in-memory index |
| [pipeline_multi_agent](examples/pipeline_multi_agent/) | Pipeline and MsgHub coordination |
| [agent_team](examples/agent_team/) | Leader/worker agent coordination |
| [mcp](examples/mcp/) | MCP tool discovery and calls |
| [a2a_http](examples/a2a_http/) | Agent-to-agent communication over HTTP |
| [grpc_a2a](examples/grpc_a2a/) | TCP messaging with newline-delimited JSON, not gRPC |
| [replay](examples/replay/) | Simulated response tapes and file persistence |
| [replayview](examples/replayview/) | A terminal viewer for RunJSONL logs |
| [rundiff](examples/rundiff/) | Compare RunJSONL logs |
| [eval_harness](examples/eval_harness/) | Score recorded response fixtures |
| [agent_pool](examples/agent_pool/) | A bounded worker pool with simulated jobs |
| [hotreload](examples/hotreload/) | Typed configuration reloads |
| [bench](examples/bench/) | Load testing and latency reports |
| [quality_load](examples/quality_load/) | Task quality under scheduled arrivals, with offline fixtures |
| [managed_inference](examples/managed_inference/) | Shared inference admission and physical attempts with a local HTTP fixture |
| [qdrant_filter](examples/qdrant_filter/) | Metadata filtering against a local Qdrant server |
| [wasm_sandbox](examples/wasm_sandbox/) | WASM runtime discovery and sandbox configuration |
| [hub_install](examples/hub_install/) | Component registries and installation APIs |
| [skill_partitions](examples/skill_partitions/) | Per-agent workspace skill directories |
| [workspace_sharing](examples/workspace_sharing/) | Session/workspace bindings and artifact access |
| [access_control](examples/access_control/) | Resource grants and access checks |
| [document_parser](examples/document_parser/) | Document parsing and chunking |
| [audit_logging](examples/audit_logging/) | Sandbox policy checks and audit records |
| [guardrail](examples/guardrail/) | Block, redact and warn with mock model responses |
| [spend_cap](examples/spend_cap/) | Observed-cost budgets with a mock model |
| [agent_service](examples/agent_service/) | HTTP service and SSE events |
| [webui](examples/webui/) | Embedded browser UI |
| [dingtalk_channel](examples/dingtalk_channel/) | DingTalk messages and confirmations |
| [scheduled_task](examples/scheduled_task/) | One-shot and recurring tasks |
| [realtime_echo](examples/realtime_echo/) | A realtime-interface echo client |
| [edge_offline](examples/edge_offline/) | Cloud/local routing with Ollama |
| [edge_sensor](examples/edge_sensor/) | Sensor middleware with a mock sensor |
| [edge_serial_robot](examples/edge_serial_robot/) | Device tools with a mock serial device |
| [edge_fleet](examples/edge_fleet/) | In-memory pub/sub fleet simulation |
| [werewolves](examples/werewolves/) | A multi-agent Werewolves game |
| [k8s_workspace](examples/k8s_workspace/) | Kubernetes workspace configuration and cluster tools |

</details>

The [examples guide](docs/examples.md) provides running instructions and links
to the same catalog.

## Documentation

| Topic | Guide |
|---|---|
| Setup and a first agent | [Getting started](docs/getting-started.md) |
| Models and tools | [Providers](docs/model-providers.md) · [Tools](docs/tools.md) |
| Middleware and memory | [Middleware](docs/middleware.md) |
| Application deployment | [Deployment](docs/deployment.md) · [Execution and session limits](docs/adversarial-hardening.md) |
| Runtime and evaluation | [Runtime features](docs/go-exclusive.md) · [Load and quality testing](docs/benchmarks.md) · [Replay and evaluation source](pkg/agentscope/replay/) |
| Local models and devices | [Edge deployment](docs/edge-deployment.md) · [Device tools](docs/device-tools.md) · [Multi-device coordination](docs/multi-device.md) · [Offline operation](docs/offline-operation.md) |
| Implementation and compatibility | [Source map](CLAUDE.md) · [API stability](STABILITY.md) · [Changelog](CHANGELOG.md) |

## Project status

Stability varies by package. The core model, message, agent, tool, permission,
formatter and error APIs have documented stability commitments; other packages
include experimental interfaces. Read [STABILITY.md](STABILITY.md) for the exact
scope and remaining hardening work. Features on `main` may not be in a release.

For deployments that execute shell commands or access files, configure and test
the appropriate workspace backend and permissions. Permission checks alone do
not provide complete process, network or resource isolation. Deployment
requirements depend on the selected providers and backends.

## Contributing

Bug reports, regression tests, documentation and focused feature contributions
are welcome. For a bug, include the version or commit, configuration and a minimal
reproduction with secrets removed. For a larger feature, open an issue to discuss
the use case and API before starting a broad implementation.

Read [CONTRIBUTING.md](CONTRIBUTING.md) for setup and the PR process, and
[AGENTS.md](AGENTS.md) for validation and review requirements. Small PRs with a
clear purpose are easier to review; there is no need to take on a whole subsystem.
Please follow our [Code of Conduct](CODE_OF_CONDUCT.md).

Report suspected vulnerabilities privately through [SECURITY.md](SECURITY.md),
not in a public issue.

## License and citation

Licensed under [Apache-2.0](LICENSE). For research using AgentScope, see
[AgentScope: A Flexible yet Robust Multi-Agent Platform](https://arxiv.org/abs/2402.14034).
