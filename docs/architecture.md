# Architecture

## Overview

agentscope-go is organized as a single Go module at `github.com/agentscope-ai/agentscope-go/v2`. The `/v2` major-version suffix is part of every import path. All library code lives under `pkg/agentscope/`, runnable demos under `examples/`.

## Package Structure

```
pkg/agentscope/
├── config.go              # Global Init(), logging, ID factory
│
├── access/                # Multi-tenant access control (RBAC)
│   ├── access.go          # ResourceKind, PermissionLevel, Principal, Grant, Policy
│   ├── checker.go         # CheckAccess — evaluates policies against principals
│   ├── manager.go         # Manager — high-level grant/revoke/check operations
│   └── store.go           # Store interface for policy persistence
│
├── agent/                 # Agent abstractions
│   ├── agent.go           # Agent interface + AgentBase (identity, hooks, subscribers)
│   ├── unified_agent.go   # UnifiedAgent — native tool calling, streaming, middleware
│   ├── model_stream.go    # Provider stream consumption, event blocks and final validation
│   ├── loop_bridge.go     # UnifiedAgentRunner — bridges UnifiedAgent to loop.Loop
│   ├── user_agent.go      # UserAgent — human input via InputProvider
│   ├── a2a_agent.go       # A2AAgent — remote agent proxy via HTTP
│   ├── compress.go        # Context compression (trigger/reserve ratios, structured summary)
│   └── agent_state.go     # Serializable session state
│
├── a2a/                   # Agent-to-Agent protocol
│   ├── a2a.go             # A2A HTTP protocol types + client
│   └── grpc/              # TCP Agent Mesh transport
│       ├── transport.go   # Transport interface, Server, Client over newline-delimited JSON/TCP
│       └── transport_test.go
│
├── bench/                 # Agent load testing
│   ├── bench.go           # Scenario, Runner, Report, LatencyStats (p50/p95/p99)
│   ├── open_loop.go       # Finite arrival schedules, bounded callbacks, per-arrival outcomes
│   └── bench_test.go
│
├── hotreload/             # Hot-reload configuration
│   ├── hotreload.go       # Watcher — polling-based file change detection + Handler callbacks
│   ├── config_reloader.go # Reloader[T] — generic typed config reloader with atomic swap
│   └── hotreload_test.go
│
├── hub/                   # Component marketplace
│   ├── hub.go             # Hub interface, Card, ListOptions, ListResult
│   ├── mcp_hub.go         # MCP tool hub adapter
│   ├── skill_hub.go       # Skill hub adapter
│   ├── registry.go        # Multi-hub registry with search across sources
│   └── hub_test.go
│
├── model/                 # LLM provider interface
│   ├── model.go           # ChatModel interface, CallOptions, ToolSchema
│   ├── openai.go          # OpenAI Chat Completions adapter
│   ├── anthropic.go       # Anthropic Messages adapter
│   ├── dashscope.go       # DashScope (Qwen) adapter + shared OpenAI-compat types
│   ├── deepseek.go        # DeepSeek adapter
│   ├── gemini.go          # Google Gemini adapter
│   ├── ollama.go          # Ollama (local) adapter
│   ├── moonshot.go        # Moonshot/Kimi adapter
│   ├── xai.go             # xAI/Grok adapter
│   ├── openai_response.go # OpenAI Responses API adapter
│   ├── fallback.go        # FallbackChatModel (primary → fallback failover)
│   ├── structured_output.go # GenerateStructuredOutput (JSON Schema via tool call)
│   ├── wav.go             # PCM-to-WAV conversion for audio streaming
│   ├── http.go            # ClientOptions, defaultHTTPClient, mergeHeaders
│   ├── models/            # 78 YAML model cards (//go:embed)
│   └── pricing/           # Embedded default price card for cost governance
│
├── tool/                  # Tool system, 24 registered tool names
│   ├── tool.go            # Tool interface, BaseTool, FunctionTool, Toolkit, input validation + schema coercion
│   ├── builtin.go         # Shared constants (MaxFileSize, MaxInlineImageBytes) + shell/view aliases
│   ├── builtin_bash.go    # Bash tool with AST-level safety analysis
│   ├── builtin_read.go    # File read tool; images come back as base64 DataBlocks
│   ├── builtin_write.go   # File write tool (10 MB cap, atomic replacement)
│   ├── builtin_edit.go    # File edit tool (search/replace)
│   ├── builtin_multiedit.go   # MultiEdit — validate all edits before rewriting once
│   ├── builtin_apply_patch.go # ApplyPatch — validate a unified diff before rewriting
│   ├── builtin_glob.go    # Glob pattern matching
│   ├── builtin_grep.go    # Text search
│   ├── builtin_lsp.go     # Language-server queries
│   ├── builtin_notebook.go    # Jupyter notebook cell editing
│   ├── builtin_webfetch.go    # WebFetch (SSRF-guarded, see webfetch_ssrf.go)
│   ├── builtin_spawn.go   # Agent — spawn a sub-agent
│   ├── builtin_reset_tools.go # ResetTools — activate/deactivate tool groups
│   ├── builtin_schedule.go    # ScheduleCreate/Delete/List/View
│   ├── builtin_task.go    # Task management (create/get/list/update)
│   ├── compress_context.go    # compress_context — model-driven compression (#2143)
│   ├── orchestrator.go    # Sandbox policy checks + tool dispatch
│   ├── read_cache.go      # Read cache: FIFO eviction, backend-mtime freshness, copy-on-read
│   ├── bash_parser.go     # Command safety analysis (injection, dangerous paths, interpreter attacks)
│   ├── safety.go          # Dangerous file/directory lists
│   ├── workspace_jail.go  # Workspace-relative path confinement
│   ├── webfetch_ssrf.go   # SSRF guard for WebFetch
│   ├── chunk.go           # Output chunking helpers
│   ├── proc_unix.go / proc_windows.go  # Process-group isolation on Unix
│   └── backend.go         # Backend interface (exec shell, read/write files) + optional BackendStatter
│
├── message/               # Message types
│   ├── msg.go             # Msg struct with polymorphic ContentBlock
│   ├── block.go           # TextBlock, ThinkingBlock, ToolCallBlock, ToolResultBlock, DataBlock, HintBlock
│   └── append_event.go    # Event-to-message accumulation
│
├── event/                 # Streaming event system
│   ├── event.go           # 30 event types (reply, model, text, thinking, data, tool, HITL, sandbox)
│   └── streamcheck/       # Event-stream invariant validator (used by agenttest)
│
├── middleware/             # Agent middleware
│   ├── middleware.go       # Middleware interface (7 hooks), chain builders
│   ├── tracing.go         # TracingMiddleware (OpenTelemetry semantic conventions)
│   ├── tts.go             # TTSMiddleware (text → audio DataBlock injection)
│   ├── budget.go          # ReplyBudgetControlMiddleware (token budget enforcement)
│   ├── cost_tracker.go    # CostTrackerMiddleware (observed-cost threshold)
│   ├── guardrail.go       # GuardrailMiddleware (Block/Redact/Warn content rules)
│   ├── repetition.go      # RepetitionBreakerMiddleware (tool-call spin detection)
│   ├── watchdog.go        # ReplyWatchdogMiddleware (wall-clock + idle timeouts)
│   ├── cost_ledger.go     # CostLedger/CostTracking/ReplyCostBudget middleware
│   ├── runlog.go          # RunJSONL (full event stream + model calls as JSONL)
│   ├── stream_validator.go # StreamValidator (event-stream invariant checks)
│   └── memory/            # LongTermMemoryMiddleware (3 modes: static/agent/both)
│
├── replay/                # Record/replay + flight recorder + run logs
│   ├── replay.go          # Tape, Entry — recorded model call sequences
│   ├── middleware.go       # Middleware — record/replay via OnModelCall hook
│   ├── flight.go          # Flight recorder (ring buffer, dump-on-error tapes)
│   ├── diff.go            # RunJSONL parsing + LCS-aligned run diff
│   ├── eval.go            # Eval harness (scorers + EvalTape runner)
│   ├── evalkit/           # YAML eval suites: runner, scorers, LLM judge, A/B
│   └── store.go           # File-based tape persistence (JSON)
│
├── wasm/                  # WASM sandbox execution
│   ├── wasm.go            # Runtime interface, ExecRequest, ExecResult
│   ├── sandbox.go         # Sandbox — safe execution environment with resource limits
│   ├── cli_runtime.go     # CLIRuntime — Wasmtime limit enforcement
│   └── wasm_test.go
│
├── rag/                   # Retrieval-Augmented Generation
│   ├── rag.go             # Document, Embedder, Index interfaces, InMemoryIndex
│   ├── knowledge_base.go  # KnowledgeBase — high-level add/query
│   ├── qdrant_index.go    # QdrantIndex — Qdrant vector store backend
│   ├── qdrant_text_index.go # QdrantTextIndex — auto-embeds text then stores
│   ├── elasticsearch.go   # ElasticsearchIndex — BM25 + dense hybrid
│   ├── mongodb.go         # MongoDBIndex — Atlas $vectorSearch
│   ├── milvus.go          # MilvusIndex — Milvus vector store
│   ├── rerank.go          # Reranker interface + RerankedIndex wrapper
│   ├── llm_rerank.go      # LLMReranker — rerank via any ChatModel
│   └── parser/            # Document parsers
│       ├── parser.go      # Parser interface, ChunkConfig (Unit + Validate), ChunkText
│       ├── text.go        # TextParser (.txt, .md, .csv, .log)
│       ├── pdf.go         # PDFParser (stream-based text extraction)
│       ├── word.go        # WordParser (.docx XML extraction)
│       ├── excel.go       # ExcelParser (.xlsx → row-based text)
│       ├── ppt.go         # PPTParser (.pptx slide text extraction)
│       └── *_test.go      # Tests for each parser
│
├── formatter/             # Per-provider message formatting
├── permission/            # Permission engine (5 modes, rule matching, HITL)
├── pipeline/              # Pipeline (Then/If) + MsgHub (multi-agent routing)
├── credential/            # Provider credential types + factory
├── mcp/                   # MCP client (Stdio + HTTP) and server
├── embedding/             # Embedding models (4 providers) + cache
├── team/                  # Agent teams (leader/worker coordination)
├── workspace/             # Sandboxed execution environments
│   ├── local.go           # LocalWorkspace — directory-scoped
│   ├── docker.go          # DockerWorkspace — container-level
│   ├── e2b.go             # E2BWorkspace — cloud sandbox
│   ├── k8s.go             # K8sWorkspace — Kubernetes Pod-based
│   ├── opensandbox.go     # OpenSandboxWorkspace — OpenSandbox API
│   ├── daytona.go         # DaytonaWorkspace — Daytona dev environment
│   ├── applecontainer.go  # AppleContainerWorkspace — Apple Container framework
│   ├── bubblewrap.go      # BubblewrapWorkspace — Linux bwrap sandbox
│   └── gateway.go         # WorkspaceGateway — unified workspace router
│
├── tts/                   # Text-to-Speech models
│   ├── tts.go             # TTSModel interface
│   ├── dashscope.go       # DashScope standard TTS
│   ├── dashscope_realtime.go # CosyVoice realtime TTS
│   ├── openai.go          # OpenAI Audio Speech API
│   ├── gemini.go          # Gemini TTS (generateContent with audio modality)
│   └── models/            # 9 TTS model cards
│       ├── dashscope/     # CosyVoice v1/v2/v3, Qwen3-TTS
│       ├── openai/        # tts-1, tts-1-hd
│       └── gemini/        # gemini-2.5-flash-preview-tts
│
├── storage/               # State persistence (InMemory, File, SQL, Redis + RedisFullStorage)
├── service/               # HTTP Agent Service + SSE + AG-UI protocol
├── tracing/               # Tracer interface + OpenTelemetry + LoggerTracer
├── schedule/              # InMemoryScheduler + optional TaskRemover; cron parsing lives in app/
├── messagebus/            # InMemory + Redis pub/sub + registry operations
├── session/               # Session KV store
├── skill/                 # Reusable skill system + SkillManager registry
├── memory/                # Conversation memory + compression
├── prompt/                # Composable system prompt assembly from named sections
├── resilience/            # Circuit breaker + rate limiter wrappers for ChatModel
├── realtime/              # Realtime streaming interface + echo client
├── logging/               # Structured logging handlers and initialization
│
├── protocol/              # Shared enums: LoopState, ApprovalPolicy, PermissionProfile
├── loop/                  # Configurable agent loop (model → inspect → act → iterate)
├── runtime/               # SessionEngine, AgentManager, BudgetTracker, AgentPool
├── metrics/               # Counter/Histogram interfaces + InMemoryProvider + MetricsHook
├── platform/              # Cross-platform shell detection + PowerShell safety checks
├── sandbox/               # Filesystem/network/process/resource policy types
├── errors/                # Typed error hierarchy (Retriable, Throttled, PermissionDenied, Timeout)
├── tune/                  # Fine-tuning utilities
├── types/                 # Shared type definitions
├── config/                # Configuration loading and defaults
├── app/                   # Full application bootstrap (CreateApp)
├── agenttest/             # Test helpers, fixtures, fault injection (faults/)
├── providercontract/      # Test-only provider contract wall (usage, streaming, errors)
└── internal/              # httpx (HTTP+SSE), httpsec (SSRF guard), jsonx (JSON repair + schema coercion), fsutil (atomic writes)
```

## Core Concepts

### Agent

The `Agent` interface defines five methods: `ID()`, `Reply()`, `Observe()`, `Interrupt()`, `SetConsoleOutputEnabled()`.

`UnifiedAgent` is the primary implementation. It runs a ReAct loop: reasoning (model call) → acting (tool execution) → repeat until done or max iterations. Streaming via `ReplyStream()` returns `<-chan event.Event`.

### Message

`Msg` carries typed `ContentBlock` slices. Block types: `TextBlock`, `ThinkingBlock`, `ToolCallBlock`, `ToolResultBlock`, `DataBlock` (images/audio), `HintBlock`. JSON serialization uses a `type` field discriminator.

### Model

`ChatModel` provides `Chat()`, `ChatStream()`, `CountTokens()`. Nine provider adapters share common patterns: functional options via `CallOption`, retry logic, `ClientOptions` for HTTP customization. All nine adapters stream over SSE: eight go through `httpx.DoSSERequest`, and the OpenAI Responses adapter parses the `data:` frames itself. Ollama streams via its OpenAI-compatible `/v1/chat/completions` endpoint, not its native newline-delimited JSON API.

78 model cards are bundled across 8 provider directories, plus 9 TTS model cards.

### Tool

`Tool` embeds `permission.Checker` for fine-grained access control. `FunctionTool` wraps plain Go functions. `Toolkit` manages named `ToolGroup`s with activation/deactivation. Built-in tools include a Bash executor with AST-level command safety analysis.

### Middleware

The middleware interface defines seven hooks: `OnReply`, `OnReasoning`, `OnModelCall`, `OnActing`, `OnSystemPrompt`, `OnCompressContext`, and `OnCheckPermission`. `ListTools()` provides additional tools. The current UnifiedAgent and loop bridge call the permission engine directly; `OnCheckPermission` requires explicit integration using `BuildCheckPermissionChain` and is not automatically invoked on those paths.

### Deterministic Replay

The `replay` package records model call request/response pairs into a `Tape` (JSON-serializable sequence of `Entry` records). In replay mode, the middleware intercepts `OnModelCall` and returns pre-recorded responses instead of calling the LLM. Use a non-nil mock model and mock or isolated tools for offline tests: replay consumes tape entries in order, does not validate prompt equality, and does not intercept tool execution.

### Agent Loop (v3)

`loop.Loop` is a lower-level reasoning engine that drives the Reason → Inspect → Act state machine. It takes pluggable components:

- **`ModelCaller`** — wraps the LLM API call
- **`ToolExecutor`** — executes tool calls (single or batch)
- **`ContextManager`** — manages conversation history and compression
- **`ToolSchemaProvider`** — provides tool schemas for model calls
- **`Hook`** — lifecycle notifications (before/after model call, before/after tool exec, state transitions, loop start/end)

`Run()` returns `<-chan event.Event` for streaming; `RunSync()` blocks and returns the final `ChatResponse`. `UnifiedAgentRunner` bridges `UnifiedAgent` to `loop.Loop` via adapter types. The tool adapter uses the agent permission engine and acting middleware. Confirmation-required and external calls return tool errors; use `UnifiedAgent.ReplyStream` for interactive handshakes.

### Runtime (v3)

Session lifecycle management:

- **`SessionEngine`** — serializes `SubmitMessage()` turns, preserves the configured context manager/history, supports interruption, and tracks budgets. State snapshots still share message objects; persisted history is not an automatic restore lifecycle.
- **`Turn`** — wraps a single loop execution with hooks and budget enforcement
- **`AgentManager`** — spawns/stops subagents with concurrency limits via `BudgetTracker`
- **`AgentPool`** — worker pool pattern for parallel agent execution with fan-out dispatch
- **`BudgetTracker`** — enforces limits on turns, tokens, duration, and concurrency

### Metrics (v3)

Provider-agnostic instrumentation:

- **`Counter`** / **`Histogram`** — metric interfaces (no external dependencies)
- **`InMemoryProvider`** — stores metrics in memory with `Snapshot()` for testing
- **`MetricsHook`** — implements `loop.Hook` to automatically track model calls, tool executions, loop iterations, and active loops

### Sandbox (v3)

Sandboxed command execution:

- **`Policy`** — configures filesystem, network, process, and resource restrictions
- **`Sandbox`** interface — `Execute()`, `Setup()`, `Teardown()`
- **`NoopSandbox`** — passthrough execution with no restrictions (default)
- Provider registration via `RegisterProvider()` / `AutoSelect()`

These types describe policy and execution interfaces, not universal enforcement.
The orchestrator's current checks are name-based and incomplete for custom tools,
case aliases, network allowlists, and resource controls. Configuring its `Sandbox`
field does not by itself route tool calls through `Sandbox.Execute`. Use and test
an appropriate `tool.WithBackend` workspace adapter for shell/file isolation.

### WASM Sandbox

The CLI implementation enforces fuel, linear-memory, timeout, directory-grant, and output-capture settings through Wasmtime. Wasmer and wasm3 may be discovered, but execution with the default resource limits returns `ErrUnsupportedLimits`. Select Wasmtime explicitly and check both errors and result status. Empty `AllowedPaths` grants no host directories.

### Hot-Reload Configuration

The `hotreload` package provides a polling-based file watcher (`Watcher`) and a generic typed config reloader (`Reloader[T]`). Watch any config file for changes, and handlers are called with the new file contents. `Reloader[T]` adds atomic pointer swap and a typed `Get()` method, so your agent can pick up config changes without restarting.

### Component Hub

The `hub` package defines a `Hub` interface for browsing, searching, and installing components (MCP tools, skills) from remote registries. `MCPHub` and `SkillHub` are concrete adapters. The `Registry` type aggregates multiple hubs for unified search.

### Access Control

The `access` package implements multi-tenant RBAC with four permission levels (`none`, `read`, `write`, `admin`) across four resource kinds (`credential`, `agent`, `knowledge_base`, `session`). Principals can be users, groups, or organizations. Policies are stored via a pluggable `Store` interface.

### Agent Load Testing

The `bench` package provides a `Runner` that executes `Scenario` definitions with configurable concurrency, duration, iterations, and ramp-up. Reports include throughput, latency percentiles (p50/p95/p99), success/failure counts, and error breakdowns.

`Runner.RunOpenLoop` offers work on a finite schedule independently of callback
completion, bounds admitted callbacks, and retains every planned arrival's
outcome. Its report separates local rejection, timeout, cancellation, unfinished
work, and arrivals not offered before interruption. See the
[load-testing guide](benchmarks.md) for timing and cancellation semantics. These
callback outcomes do not by themselves measure agent task quality.

### TCP Agent Mesh

The `a2a/grpc` package provides a TCP-based transport for inter-agent communication using newline-delimited JSON messages. A `Server` accepts connections and dispatches incoming messages to a handler function. A `Client` connects to a remote server. The transport is not the gRPC wire protocol. Replies preserve the request ID. `Client.Stream` has a fixed 64-message buffer; cancellation, disconnect, or overflow can close it without `StreamEnd`. Consumers must observe `StreamEnd` for success and use fresh IDs on retries. The basic `Server.OnMessage` handler returns a single response.

## Data Flow

```
User Input
    │
    ▼
UnifiedAgent.Reply(ctx, input)
    │
    ├── OnReply middleware chain
    │       │
    │       ▼
    │   ReAct Loop (max N iterations)
    │       │
    │       ├── OnReasoning middleware chain
    │       │
    │       ├── OnSystemPrompt pipeline → build system prompt
    │       │
    │       ├── OnModelCall chain → ChatModel.Chat/ChatStream
    │       │       │
    │       │       └── Provider adapter → HTTP/SSE → LLM API
    │       │
    │       ├── Parse response → TextBlock / ToolCallBlock
    │       │
    │       └── If ToolCallBlock:
    │               │
    │               ├── Permission engine (OnCheckPermission needs explicit wiring)
    │               │       │
    │               │       ├── Permission check (Engine.CheckPermission)
    │               │       │       │
    │               │       │       ├── ALLOW → execute
    │               │       │       ├── ASK → emit RequireUserConfirmEvent → wait
    │               │       │       └── DENY → skip
    │               │
    │               ├── OnActing chain → Tool.Execute
    │               │
    │               └── Append ToolResultBlock → next iteration
    │
    ▼
ChatResponse (Content: []ContentBlock, Usage, Metadata)
```

The v3 `loop.Loop` provides the same state machine at a lower level. `UnifiedAgentRunner` bridges between the two: it wraps the agent's model and toolkit into `ModelCaller` / `ToolExecutor` adapters, letting `loop.Loop` drive the cycle while `UnifiedAgent` handles middleware, permissions, and HITL.

## Design Principles

- **Interfaces + embeddable defaults**: Define small interfaces, provide `BaseXxx` structs (e.g., `BaseTool`, `BaseMiddleware`)
- **Functional options**: Constructors take `opts ...Option` (e.g., `NewUnifiedAgent(name, prompt, model, opts...)`)
- **Streaming via channels**: `<-chan T` pattern. Goroutine writes deltas then final response, defers `close(ch)`
- **Context propagation**: `context.Context` as first argument everywhere
- **Explicit errors**: Return `(T, error)` rather than panicking (exception: `message.NewMsg` panics on invalid content type)
- **Generics where appropriate**: `Reloader[T]` for typed config hot-reload, avoiding `any` casts
- **Go-native concurrency**: `AgentPool` uses worker goroutines and channels, not thread pools
