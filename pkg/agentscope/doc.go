// Package agentscope provides the core framework for building multi-agent LLM applications in Go.
//
// It is the Go implementation of the AgentScope project (https://github.com/agentscope-ai/agentscope),
// offering Go-idiomatic APIs while maintaining full feature parity with the Python version.
//
// # Quick Start
//
//	as.Init()
//	cm, _ := model.NewDashScopeChatModel(model.DashScopeConfig{APIKey: "sk-...", Model: "qwen-plus"})
//	a := agent.NewUnifiedAgent("bot", "You are helpful.", cm)
//	reply, _ := a.Reply(ctx, "Hello!")
//
// # Architecture
//
// The framework is organized into subpackages under pkg/agentscope/:
//
//   - agent: Agent interface and implementations (UnifiedAgent, UserAgent, A2AAgent, ReActAgent)
//   - model: ChatModel interface with 9 provider adapters + 54 bundled model cards
//   - tool: Tool interface with 17 built-in tools and safety analysis
//   - message: Msg type with polymorphic ContentBlock variants
//   - event: 30 event types for streaming lifecycle
//   - middleware: 7-hook chain (6 onion + 1 pipeline) for intercepting agent behavior
//   - permission: Permission engine with 5 modes for tool execution control
//   - pipeline: Sequential pipeline with MsgHub for multi-agent orchestration
//   - loop: Universal agent loop with state machine and event streaming
//   - runtime: Session engine, turn orchestration, AgentPool, and BudgetTracker
//   - metrics: Counter/Histogram interfaces with MetricsHook + Prometheus provider
//   - protocol: Shared types for loop state, events, and results
//   - sandbox: Sandbox execution policies (Allow/Deny/AskUser)
//   - platform: Cross-platform shell detection and safety analysis
//   - errors: Structured AgentError with category classification, retryable
//     marking, and sentinel errors (ErrModelRateLimited, ErrToolDenied, …), all
//     compatible with errors.Is / errors.As
//   - skill: Reusable skill system with SkillManager registry
//   - schedule: InMemoryScheduler for periodic agent task execution
//   - team: Agent teams with leader/worker coordination
//   - prompt: Composable system prompt assembly from named sections
//   - resilience: Circuit breaker and rate limiter wrappers for ChatModel
//   - audit: Structured audit logging (InMemory/File/Multi/Nop backends)
//   - a2a: A2A protocol types + HTTP client; a2a/grpc for TCP agent mesh
//   - mcp: MCP client (Stdio + HTTP transports) + MCP server
//   - workspace: 8 sandbox backends (Local, Docker, E2B, K8s, OpenSandbox, Daytona, Apple, Bubblewrap)
//   - rag: Index + KnowledgeBase + 5 vector stores (InMemory, Qdrant, QdrantText, Elasticsearch, Milvus)
//   - storage: 4 backends (InMemory, File, Redis, SQL)
//   - embedding: 4 providers + batch + cache
//   - tts: 3 TTS providers (DashScope, OpenAI, Gemini)
//   - device: Hardware connectors (Serial/GPIO/CAN/I2C) + DeviceTool + Watchdog
//   - messagebus: InMemory + Redis pub/sub; messagebus/mqtt for edge/IoT
//   - replay: Deterministic record/replay of LLM calls
//   - hotreload: Typed generic config reloader with file watching
//   - wasm: WASM sandbox (wasmtime/wasmer/wasm3 backends)
//   - tracing: Tracer interface + OTel + LoggerTracer
//   - hub: MCP Hub + Skill Hub + Registry
//   - access: Resource sharing with users/groups/orgs
//   - credential: 9 provider credential types + auto-detect from env
//
// See the README and examples/ directory for comprehensive usage examples.
package agentscope
