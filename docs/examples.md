# Examples

Run examples from a checkout of this repository. Installing the library with
`go get` does not create an `examples/` directory in your application.

```bash
git clone https://github.com/agentscope-ai/agentscope-go.git
cd agentscope-go
go run ./examples/agent_pool
```

`agent_pool` uses simulated jobs and does not need a model API key. For a model
call with tools, start with [agent_v2](../examples/agent_v2/). Its model loader
checks `ANTHROPIC_API_KEY`, then `DASHSCOPE_API_KEY`, then `OPENAI_API_KEY`;
it uses the model IDs specified in that example's source.

Before running another example, inspect its source for provider configuration,
arguments and external requirements. Some use fixtures or simulated devices;
others need a model service, credentials, local tools or a running backend.
A compiled example is not evidence that an external integration has been tested.

For structured questions, run `go run ./examples/ask_user`. It uses a scripted
model and a simulated host answer, without credentials or user interaction.
A real application must collect the user's response through its own frontend;
see the [AskUser contract](tools.md#askuser).

## Catalog

| Example | Demonstrates |
|---|---|
| [simple](../examples/simple/) | A custom agent with a single model call |
| [agent_v2](../examples/agent_v2/) | UnifiedAgent with a function tool |
| [react_tool](../examples/react_tool/) | A custom FunctionTool |
| [react_builtin_tools](../examples/react_builtin_tools/) | The built-in coding toolkit |
| [streaming](../examples/streaming/) | Agent lifecycle events |
| [ask_user](../examples/ask_user/) | Structured questions with a simulated model and host answer |
| [console](../examples/console/) | Terminal chat and tool-call confirmation |
| [model_call](../examples/model_call/) | Direct model streaming, tool calls and structured output |
| [structured_output](../examples/structured_output/) | Structured output through tool calling |
| [multi_provider](../examples/multi_provider/) | Provider configuration and model cards |
| [multimodal](../examples/multimodal/) | Image input using URLs and base64 data |
| [multiagent](../examples/multiagent/) | Multi-agent conversation |
| [multiagent_multimodal](../examples/multiagent_multimodal/) | Multi-agent conversation with image input |
| [openai_response](../examples/openai_response/) | OpenAI Responses API |
| [middleware](../examples/middleware/) | Model-call and tool-execution hooks |
| [permission](../examples/permission/) | Tool permission modes |
| [tracing](../examples/tracing/) | Tracing with LoggerTracer |
| [tracing_otlp](../examples/tracing_otlp/) | An OTLP integration setup pattern |
| [agent_loop](../examples/agent_loop/) | Loop configuration and metrics |
| [embedding](../examples/embedding/) | Text embeddings and similarity |
| [long_term_memory](../examples/long_term_memory/) | Long-term memory middleware |
| [agentic_memory](../examples/agentic_memory/) | File-based memory and MEMORY.md |
| [rag_react](../examples/rag_react/) | Retrieval with an in-memory index |
| [pipeline_multi_agent](../examples/pipeline_multi_agent/) | Pipeline and MsgHub coordination |
| [agent_team](../examples/agent_team/) | Leader/worker agent coordination |
| [mcp](../examples/mcp/) | MCP tool discovery and calls |
| [a2a_http](../examples/a2a_http/) | Agent-to-agent communication over HTTP |
| [grpc_a2a](../examples/grpc_a2a/) | TCP messaging with newline-delimited JSON, not gRPC |
| [replay](../examples/replay/) | Simulated response tapes and file persistence |
| [replayview](../examples/replayview/) | A terminal viewer for RunJSONL logs |
| [rundiff](../examples/rundiff/) | Compare RunJSONL logs |
| [eval_harness](../examples/eval_harness/) | Score recorded response fixtures |
| [agent_pool](../examples/agent_pool/) | A bounded worker pool with simulated jobs |
| [hotreload](../examples/hotreload/) | Typed configuration reloads |
| [bench](../examples/bench/) | Load testing and latency reports |
| [quality_load](../examples/quality_load/) | Task quality under scheduled arrivals, with offline fixtures |
| [managed_inference](../examples/managed_inference/) | Shared inference admission and physical attempts with a local HTTP fixture |
| [qdrant_filter](../examples/qdrant_filter/) | Metadata filtering against a local Qdrant server |
| [wasm_sandbox](../examples/wasm_sandbox/) | WASM runtime discovery and sandbox configuration |
| [hub_install](../examples/hub_install/) | Component registries and installation APIs |
| [skill_partitions](../examples/skill_partitions/) | Per-agent workspace skill directories |
| [workspace_sharing](../examples/workspace_sharing/) | Session/workspace bindings and artifact access |
| [access_control](../examples/access_control/) | Resource grants and access checks |
| [document_parser](../examples/document_parser/) | Document parsing and chunking |
| [audit_logging](../examples/audit_logging/) | Sandbox policy checks and audit records |
| [guardrail](../examples/guardrail/) | Block, redact and warn with mock model responses |
| [spend_cap](../examples/spend_cap/) | Observed-cost budgets with a mock model |
| [agent_service](../examples/agent_service/) | HTTP service and SSE events |
| [webui](../examples/webui/) | Embedded browser UI |
| [dingtalk_channel](../examples/dingtalk_channel/) | DingTalk messages and confirmations |
| [scheduled_task](../examples/scheduled_task/) | One-shot and recurring tasks |
| [realtime_echo](../examples/realtime_echo/) | A realtime-interface echo client |
| [edge_offline](../examples/edge_offline/) | Cloud/local routing with Ollama |
| [edge_sensor](../examples/edge_sensor/) | Sensor middleware with a mock sensor |
| [edge_serial_robot](../examples/edge_serial_robot/) | Device tools with a mock serial device |
| [edge_fleet](../examples/edge_fleet/) | In-memory pub/sub fleet simulation |
| [werewolves](../examples/werewolves/) | A multi-agent Werewolves game |
| [k8s_workspace](../examples/k8s_workspace/) | Kubernetes workspace configuration and cluster tools |

## Next steps

- [Getting started](getting-started.md) — Install the library in your own application.
- [Model providers](model-providers.md) — Configure an adapter.
- [Runtime features](go-exclusive.md) — Explore replay, pools and other runtime capabilities.
- [Contribution checks](../AGENTS.md#validation) — Build, test and review changes.
