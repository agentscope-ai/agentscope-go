# Tools

## Overview

Tools give agents the ability to execute actions. The `Tool` interface embeds `permission.Checker` for fine-grained access control.

## Built-in Tools

The tools below are available from `pkg/agentscope/tool`. Registration is
explicit; the enhanced toolkit includes only the coding tools listed below.
Workspace and device packages provide additional tools.

| Tool | Description | Safety |
|------|-------------|--------|
| `AskUser` | Collect structured choices through an external host | Opt-in; validates questions and answers; permission rules still apply |
| `Bash` | Execute shell commands | AST-level injection detection, dangerous path protection, read-only command recognition, interpreter-attack detection |
| `execute_shell_command` | Bare shell execution (`command` + optional `timeout`) | **None of `Bash`'s analysis.** No AST injection check, no interpreter-attack check, no read-only classification; `CheckPermissions` is the `BaseTool` passthrough. Register `Bash` unless you specifically need this shape |
| `Read` | Read files with line numbers, `offset`/`limit` (default 2000 lines, 1 MB file cap). Images come back as base64 `DataBlock`s | Path validation, per-line truncation at 2000 chars, `MaxInlineImageBytes` (256 KB) image cap |
| `view_text_file` | Bare text read (`path` or `file_path`), whole file as JSON | 1 MB cap and directory check, but it resolves with `filepath.Clean`/`Abs` only: it does **not** go through `resolvePath`, so it is **not** workspace-jailed and returns no images. Use `Read` when a jail is configured |
| `Write` | Create/overwrite files | 10 MB input cap, atomic replacement, unified diff in response metadata |
| `Edit` | Search/replace in files | Unified diff in response metadata |
| `MultiEdit` | Apply several edits to one file | Unified diff in response metadata |
| `ApplyPatch` | Apply a unified/patch-style diff | Unified diff in response metadata |
| `Glob` | File pattern matching | Read-only |
| `Grep` | Text search with regex | Read-only |
| `LSP` | Language-server queries | Read-only |
| `NotebookEdit` | Edit Jupyter notebook cells | Mutates the notebook file |
| `WebFetch` | Fetch a URL | SSRF guard |
| `Agent` | Spawn a sub-agent | Creates agents; not read-only |
| `ResetTools` | Activate/deactivate tool groups | Mutates the toolkit: changes which tools the agent can call. Not flagged `ReadOnly` |
| `compress_context` | Summarize older context on the model's own initiative | Rewrites the agent's message history; not concurrency-safe |
| `ScheduleCreate` / `ScheduleDelete` / `ScheduleList` / `ScheduleView` | Manage scheduled tasks | Need a `schedule.Scheduler` in the context |
| `task_create` | Create tasks with dependencies | Mutates the task store; bidirectional blocks/blockedBy |
| `task_get` | Get task details | Read-only |
| `task_list` | List all tasks | Read-only |
| `task_update` | Update task status/fields | Mutates the task store; dependency tracking |

`tool.NewEnhancedToolkit()` returns the coding-agent core: `Bash`, `Read`,
`Write`, `Edit`, `MultiEdit`, `ApplyPatch`, `Glob` and `Grep`. Everything else is
opt-in:

```go
// The coding-agent core (8 tools):
tk := tool.NewEnhancedToolkit()

// Or pick individually:
// tk := tool.NewToolkit(tool.BashTool(), tool.ReadTool(), tool.WriteTool())

tk.AddGroup("web", tool.WebFetchTool()) // grouped: activate/deactivate together
tk.ActivateGroup("web")
```

### Bash Tool Options

```go
tool.BashTool(
    tool.WithCwd("/path/to/workdir"),  // set working directory
)
```

## AskUser

`tool.AskUserTool()` lets a model ask structured questions through your
application's frontend. It is an external tool: the host presents the questions,
collects the user's choices or free text, and submits the result to the agent.
Register it only in an application that supports this interaction.

```go
assistant := agent.NewUnifiedAgent("assistant", "Help the user.", cm,
    agent.WithToolkit(tool.NewToolkit(tool.AskUserTool())),
    agent.WithPermissionContext(permission.NewContext(permission.ModeDefault)),
)
```

Here `cm` is your `model.ChatModel`. The permission context is required: it also
initializes the channels used for confirmation and external-result submission.
Consume `assistant.ReplyStream` and handle both `RequireUserConfirmEvent` and
`RequireExternalExecutionEvent`; plain `Reply`, the loop runner and the stock
console do not provide an AskUser UI. The complete
[offline example](../examples/ask_user/main.go) shows the event flow with a scripted
model and a simulated answer. It does not demonstrate real user authorization.

### Questions and answers

Input uses `AskUserParams`, containing one to four `AskUserQuestion` values.
Each question has unique question text, a nonblank header of at most 12 Unicode
code points, and two to four `AskUserOption` values. Option labels are unique
within a question; labels and descriptions must be nonblank. `Context` carries
supporting material. `MultiSelect` enables multiple choices; `Preview` is allowed
only for single-select questions. The host should always offer free-text input,
without adding a synthetic "Other" option.

For each external call, use its ID and name in the submitted `ToolResultBlock`.
A successful result has readable `Output` plus structured `Metadata`. For example,
the metadata for the question "Which client language?" with an option labeled
"Go" is:

```json
{
  "answers": [
    {"question": "Which client language?", "selected": ["Go"]}
  ]
}
```

The exported `AskUserMetadata` and `AskUserAnswer` types match this JSON shape.
`Metadata` itself is a `map[string]any`; `map[string]any{"answers": answers}`
accepts a typed `[]tool.AskUserAnswer`. Include exactly one answer for every
question, using its exact text. Selected labels must belong to that question
and must not repeat. A single-select answer accepts at most one label. Every
answer needs a selection or nonblank `other` free text; free text may accompany
a selection. Answer order need not match question order.

### Validation, permissions and recovery

Invalid input becomes an error tool result before external handoff. Successful
answers are checked against the original questions; invalid success metadata
becomes an error result without retaining that metadata. Error, denied and
interrupted results do not require answers. `SubmitExternalResult` returns no
error: observe `ToolResultEndEvent` for the outcome. Submission transfers ownership
of the result and its nested values; do not mutate them afterward.

AskUser allows the question interaction at the tool level in normal permission
modes. Explicit deny/ask rules still apply; `ModeDontAsk` denies the tool. Asking
or answering a question does not authorize another tool action. The host remains
responsible for collecting a real response and applying its own access controls.

Checkpoint resume checks that a submitted call still names an active external
tool and honors current permissions and validation before requesting execution.
Missing, inactive or no-longer-external tools produce error results. Validated
metadata survives recorded results, terminal events and `Msg.AppendEvent`.
Multi-block external output is retained in state, with supported text/data events
emitted in order subject to the existing event data-size cap. Reading all text
from a block list requires inspecting `Output`; `GetOutputText()` returns only
the first text block.

Custom tools can implement the optional `tool.InputValidator` for semantic input
checks after schema validation, and `tool.ExternalResultValidator` to validate
successful external results. The required `Tool` interface is unchanged. See the
[upstream design](design/upstream-sync-v2.0.11.md) for Python references and the
intentional differences in permission and invalid-result handling.

## Custom Function Tools

Wrap any Go function as a tool:

```go
weatherTool := tool.NewFunctionTool(
    "get_weather",
    "Get current weather for a city",
    json.RawMessage(`{
        "type": "object",
        "properties": {
            "city": {"type": "string", "description": "City name"}
        },
        "required": ["city"]
    }`),
    func(ctx context.Context, input map[string]any) (any, error) {
        city, _ := input["city"].(string)
        return map[string]any{"city": city, "temp": "22°C"}, nil
    },
)
```

## Tool Groups

Organize tools into activatable groups:

```go
tk := tool.NewToolkit(weatherTool, searchTool, calcTool)
tk.AddGroup("research", searchTool, calcTool)
tk.ActivateGroup("research")   // only research tools available
tk.DeactivateGroup("research")  // restore defaults
```

The `ResetTools` meta-tool lets agents manage groups themselves.

## MCP Tools

Discover and use remote tools via Model Context Protocol:

```go
client, _ := mcp.NewHttpClient(ctx, &mcp.HttpConfig{URL: "http://mcp-server:8080"})
mcpToolkit, _ := mcp.NewMCPToolkit(ctx, client)
merged := mcp.MergeToolkits(mcpToolkit, localToolkit)
```

### Choosing an MCP client

| Client | Use for |
|--------|---------|
| `StdioClient` | MCP servers run as local subprocesses (e.g. `npx @modelcontextprotocol/server-filesystem`) |
| `HttpClient` | Simple 2024-11-05-era servers answering bare JSON-RPC POST |
| `streamablehttp.Client` | Spec-compliant Streamable HTTP servers (spec 2025-03-26 and later), backed by the official go-sdk |

```go
import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/mcp/streamablehttp"

client, err := streamablehttp.NewClient(ctx, &streamablehttp.Config{
    BaseURL: "http://localhost:8080/mcp", // required
    // Optional:
    // HTTPClient: &http.Client{Timeout: 30 * time.Second},
    // Headers:    map[string]string{"Authorization": "Bearer " + token},
    // ProtocolVersion: "2025-03-26", // pin only for testing/debugging
})
if err != nil {
    return err
}
defer client.Close() // idempotent; session ends here, not with the handshake ctx

mcpToolkit, err := mcp.NewMCPToolkit(ctx, client)
```

`Headers` are injected only when the SDK did not set the same header, so they
never override protocol headers. `ProtocolVersion` is empty by default: the
SDK negotiates automatically, and both the 2026-07-28 revision and older 2025
versions (e.g. 2025-03-26) work through the same client. The context passed to
`NewClient` governs the connect handshake only — the session lives until
`Close`, which is idempotent and concurrency safe; `CallTool` is safe for
concurrent use.

The go-sdk dependency (pinned to v1.8.0) is opt-in: only importing
`mcp/streamablehttp` compiles it, while importers of `mcp` alone do not need
it. See [the self-contained demo](../examples/mcp_streamable/), which includes
an in-process spec-compliant server.

Tool results that carry MCP `structuredContent` without any text block are
surfaced as a JSON text block in the `ToolResponse`, on all three clients;
results that already include text content are returned unchanged. On the
Streamable HTTP client, numbers in that fallback JSON pass through the go-sdk
decode (`float64`), so integers beyond 2^53 lose precision; the stdio and
bare-HTTP clients carry the wire JSON verbatim.

MCP servers may also elicit user input in the middle of a tool call. Set
`ElicitationHandler` on `mcp.StdioConfig` (subprocess servers) or
`streamablehttp.Config` (Streamable HTTP servers) to declare the capability
and resolve `elicitation/create` requests through your own UI; the handler
receives the server's message and requested JSON Schema and answers with
accept/decline/cancel plus optional form content. Without a handler the
capability stays undeclared and elicitation requests fail fast with -32601.
The bare-POST `mcp.HttpClient` has no inbound channel and cannot elicit.

## Document Parsers (RAG)

The `rag/parser` package converts common file formats into `rag.Document` slices ready for indexing. Each parser supports configurable text chunking with overlap.

### Supported Formats

| Parser | Extensions | Description |
|--------|-----------|-------------|
| `TextParser` | `.txt`, `.md`, `.csv`, `.log` | Plain text with configurable chunking |
| `PDFParser` | `.pdf` | Stream-based text extraction (FlateDecode + plain text) |
| `WordParser` | `.docx` | XML-based text extraction from Word documents |
| `ExcelParser` | `.xlsx` | Row-based text extraction from spreadsheets |
| `PPTParser` | `.pptx` | Slide text extraction from PowerPoint |

### Usage

```go
import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/rag/parser"

// Parse a PDF into document chunks
p := &parser.PDFParser{Cfg: parser.DefaultChunkConfig()}
f, _ := os.Open("report.pdf")
docs, _ := p.Parse(ctx, f, "report.pdf")

// Each doc has Content (text) and Meta (source, page, etc.)
for _, doc := range docs {
    fmt.Printf("Chunk: %s... (source: %s)\n", doc.Content[:80], doc.Meta["source"])
}
```

### Chunk Configuration

Control how text is split into chunks:

```go
cfg := parser.ChunkConfig{
    MaxChunkSize: 1000, // in Unit
    Overlap:      200,  // in Unit, must be < MaxChunkSize
    Unit:         parser.ChunkUnitChars, // or parser.ChunkUnitApproxTokens
}
if err := cfg.Validate(); err != nil { // call at API boundaries instead of
    return err                         // relying on ChunkText's silent fallbacks
}

p := &parser.TextParser{Cfg: cfg}
```

`ChunkUnitApproxTokens` sizes chunks in approximate tokens using
`approxTokenChars = 4` characters per token, a rule of thumb for English BPE. It
under-counts CJK: one token is roughly one to one-and-a-half Chinese characters,
so a chunk configured as 1000 approximate tokens holds about 1000 characters but
closer to 700 to 1000 real tokens. CJK chunks therefore come out larger in token
terms than configured. With a hard token ceiling and a CJK corpus, lower
`MaxChunkSize`, or use `ChunkUnitChars` and size it yourself.

### Integration with RAG Pipeline

Parse documents and add to a knowledge base:

```go
// Parse
p := &parser.WordParser{Cfg: parser.DefaultChunkConfig()}
docs, _ := p.Parse(ctx, file, "manual.docx")

// Index
index := rag.NewInMemoryIndex()
index.AddDocuments(ctx, docs)

// Query
results, _ := index.Query(ctx, "installation steps", 5)
```

## Vector Store Backends

The `rag` package supports multiple vector store backends for document retrieval:

| Backend | Description | Use Case |
|---------|-------------|----------|
| `InMemoryIndex` | Simple linear scan, no external dependencies | Prototyping, small datasets |
| `QdrantIndex` | Qdrant vector database backend | Production, large-scale retrieval |
| `QdrantTextIndex` | Auto-embeds text then stores in Qdrant | Production with automatic embedding |

### InMemoryIndex

```go
index := rag.NewInMemoryIndex()
index.AddDocuments(ctx, docs)
results, _ := index.Query(ctx, "search query", 10)
```

### QdrantIndex

```go
// Requires a *qdrant.Client from github.com/qdrant/go-client/qdrant
qdrantClient, _ := qdrant.NewClient(&qdrant.Config{Host: "localhost", Port: 6334})
index, _ := rag.NewQdrantIndex(rag.QdrantConfig{
    Client:     qdrantClient,
    Collection: "my-docs",
})
```

### QdrantTextIndex

Combines embedding generation with Qdrant storage — no need to pre-compute vectors:

```go
qdrantClient, _ := qdrant.NewClient(&qdrant.Config{Host: "localhost", Port: 6334})
embedder, _ := embedding.NewOpenAIEmbeddingModel(
    &embedding.OpenAICompatConfig{APIKey: os.Getenv("OPENAI_API_KEY")})
index, _ := rag.NewQdrantTextIndex(rag.QdrantTextConfig{
    Client:     qdrantClient,
    Collection: "my-docs",
    // EmbeddingModel and rag.Embedder are different interfaces: bridge them.
    // Passing embedder directly does not compile.
    Embedder:   embedding.AsEmbedder(embedder),
})
// AddDocuments automatically embeds text before storing
index.AddDocuments(ctx, docs)
```

### Result scores

`Document.Score` is normalized so that higher means more relevant across every
backend (Elasticsearch, Qdrant, MongoDB, Milvus). Milvus L2 distance is negated,
so callers that negated it themselves must stop or they will double-negate.
Comparing scores across backends does not require knowing which metric each one
used.

### Reranked Retrieval

Improve retrieval precision by wrapping an Index with a Reranker:

```go
rerankedIdx := rag.NewRerankedIndex(baseIndex, myReranker, 3)
results, _ := rerankedIdx.Query(ctx, "search query", 5)
```

`RerankedIndex` overwrites `Document.Score` with the rerank score.

### LLM-based reranking

`rag.NewLLMReranker` turns any `ChatModel` into a `Reranker` via
structured-output judging, with a per-(query, document) score cache:

```go
rr := rag.NewLLMReranker(cm,
    rag.WithLLMRerankerDocChars(500),      // per-document excerpt, truncated by rune
    rag.WithLLMRerankerPromptRunes(24000), // whole-prompt bound (default 24000)
    rag.WithLLMRerankerCacheMax(1000),     // default 512; overflow drops the cache
)
idx := rag.NewRerankedIndex(baseIndex, rr, 3)
```

The prompt bound shrinks the per-document budget rather than dropping candidates,
because a dropped candidate would score 0 and sink in the ranking. Document text
is interpolated into the judge prompt as-is, so adversarial corpus content can
influence scores. Compared with a real cross-encoder this costs more latency and
tokens per uncached batch, and the scores are only as calibrated as the judge
model.

## Reading images (upstream #2114)

The Read tool returns image files as base64 `DataBlock` results. The nine
extensions in `imageMediaTypesByExt` are `.png .jpg .jpeg .gif .webp .bmp .tiff
.tif .ico`. They come back as images instead of mojibake text, on both the host and
workspace-backend paths. PDF page rendering is not supported (it would require
a rasterizer dependency).

Every image response also carries a leading text placeholder such as
`[shot.png: image/png, 12345 bytes]`. The agent's tool pipeline is string-based,
so without the placeholder an image-only result would reach the model as an empty
string and read as an empty file.

Whether the model actually sees the pixels depends on the provider:

| Provider path            | What the model receives                  |
| ------------------------ | ---------------------------------------- |
| OpenAI **Responses** API | native `input_image` parts (upstream #2389) |
| Chat Completions, Anthropic, Gemini, DashScope | the text placeholder only |

Nothing wires the model's capabilities into the tool layer yet, so Read cannot
decline to return an image for a model that cannot see it, and the model gets the
placeholder. The data is already in the repo: bundled model cards carry
`InputTypes` and `ModelCard.SupportsImages()`. `model.ResolveContextSize` can look
up a card through `ModelNamer`, but most provider adapters do not implement that
optional interface.
Upstream additionally checks a `model_input_types` field this repo does not
have.

Images larger than `tool.MaxInlineImageBytes` (256 KB of raw file bytes) are
reported as text instead of inlined. The token estimator decodes base64 back to
raw bytes and divides by four: `model.countTokensByBytes` adds `len(base64) * 3 / 4`
and the total is then divided by 4. An inlined image therefore costs roughly
fileSize/4 tokens. 256 KB is about 64k tokens, already a large fraction of a
context window, and a 1 MB screenshot would be about 250k tokens and trigger an
immediate compression.

`ToolResultBlock.Output` can be a plain `string` or a `[]message.ContentBlock`.
`GetOutputText()` returns the string or the first text block; inspect the block
list to consume all text and media.

## Agent-driven compression (upstream #2143)

```go
a := agent.NewUnifiedAgent("bot", "...", cm,
    agent.WithToolkit(tk),
    agent.WithContextConfig(&agent.ContextConfig{
        TriggerRatio: 0.8, // automatic compression fires here
        // AgentDrivenTriggerRatio: 0.4, // default is TriggerRatio/2
    }),
    agent.WithAgentDrivenCompression(), // registers the compress_context tool
)
```

The model can call `compress_context` itself when history grows unwieldy,
instead of waiting for the automatic token-threshold compression.

Three things to know:

- It compresses at a lower threshold than the automatic path
  (`ContextConfig.AgentDrivenTriggerRatio`, default `TriggerRatio/2`). At the same
  threshold the agent would always compress first and the model could never
  trigger the tool.
- The reply reports what happened. When the context is still below that threshold
  the tool says nothing was compressed, rather than claiming success on a no-op.
- It runs sequentially and needs no confirmation. The tool rewrites the shared
  message history, so it is `ConcurrencySafe: false` and never joins a parallel
  tool batch, and it returns an allow decision so an ASK-mode permission engine
  does not stall every model-initiated compression.

The compression split keeps unfinished tool calls out of the summarized portion.
When the model calls `compress_context` from inside the acting loop, the assistant
message holding the batch's tool calls is already in the context, and summarizing
it would orphan the results that land moments later.

For custom pipelines, wire it manually with `tool.NewCompressContextTool(fn)`,
where `fn` is a `tool.CompressFunc` returning a `tool.CompressionResult`:

```go
tk := tool.NewToolkit(
    tool.ReadTool(),
    tool.NewCompressContextTool(func(ctx context.Context) (tool.CompressionResult, error) {
        compressed, err := myCompressor.Run(ctx)
        return tool.CompressionResult{Compressed: compressed}, err
    }),
)
```

## Manual compaction

`UnifiedAgent.Compact` compresses the conversation on demand — the host-side
equivalent of a `/compact` command:

```go
compressed, err := a.Compact(ctx)
```

It runs the same structured-summary path as automatic compression but skips the
trigger-threshold check, and reports whether the context actually changed. The
automatic threshold itself is configurable as `ContextConfig.TriggerRatio`
(default 0.8); no separate option is needed.

Three things to know:

- It requires `WithContextConfig`. Without a context window and reserve target
  there is nothing to compact toward, and `Compact` returns an error.
- Call it between replies, never concurrently with an active `Reply` or
  `ReplyStream`: compaction rewrites the context the running reply is reading,
  and with [reply recovery](reply-recovery.md) enabled it would break the
  guarantee that a resumed reply sees exactly the context its checkpoint
  recorded.
- It emits no `compacted` event — that event only exists inside a reply loop —
  so hosts should use the returned bool. An empty conversation is a no-op
  (`false`, nil error), not an error.

Manual compaction composes with the experimental
[content-block compaction](context-compaction.md) splitter and runs through the
`OnCompressContext` middleware chain like the automatic path.

## Schema-guided argument repair (upstream #2496)

Tool-call arguments are repaired in two layers: malformed JSON goes through
syntax repair, and **every** call is coerced toward the tool's input schema —
quoted numbers (`"limit":"5"` → `5`), stringified booleans, lone values
wrapped into arrays, stringified objects parsed. Coercion never drops keys;
uncoercible values reach JSON-Schema validation unchanged and fail loudly.

## Read cache in workspaces (upstream #2092)

The read cache validates freshness against the filesystem that served the read:
backends implementing the optional `tool.BackendStatter` interface provide their
own mtime (`workspace.ToolBackend` does, via POSIX `stat`), and backend writes and
edits invalidate cached copies, so `Read` followed by `Edit` works in a
workspace.

The path passed to that `stat` must name the same file `ReadFile` read, and the
correct spelling is backend-specific (`workspace.ExecPathResolver`):

| Backend | Path spelling | Confidence |
|---|---|---|
| Docker, Daytona, AppleContainer | absolute in-sandbox path | Verified. `docker exec` has no `-w`, so a relative path would resolve against the image WORKDIR |
| K8s, bubblewrap | caller-relative path | By design. bubblewrap binds its host root to `/`, so an absolute `BasePath`-joined path would name a file that does not exist inside the sandbox |
| E2B, OpenSandbox | caller-relative path | Unverified. File operations and command execution go through two independent API channels, neither of which passes a working directory, so this is correct only if the service resolves both against the same base |
| backends without `BackendStatter` | not applicable | Nothing is cached. A host `os.Stat` of a workspace-relative path is meaningless, or matches an unrelated host file |

A wrong spelling is not merely a wasted exec. It supplies a freshness key for the
wrong file, and the cache can then serve stale content as fresh.

## WASM Sandbox

The CLI implementation enforces fuel, linear-memory, timeout, directory-grant, and output-capture settings through Wasmtime. Wasmer and wasm3 may be discovered, but execution with the default resource limits returns `ErrUnsupportedLimits`. Select Wasmtime explicitly and check both errors and result status. Empty `AllowedPaths` grants no host directories.

```go
rt, err := wasm.NewCLIRuntime("wasmtime")
if err != nil { log.Fatal(err) }
sandbox := wasm.NewSandbox(wasm.SandboxConfig{
    Runtime:        rt,
    MaxMemory:      64 * 1024 * 1024,
    MaxDuration:    5 * time.Second,
    MaxOutputBytes: 1024 * 1024,
})
result, err := sandbox.Run(ctx, "tools/transform.wasm", []byte(`{"text":"hello"}`))
if err != nil { log.Fatal(err) }
if result.ExitCode != 0 || result.OutputTruncated {
    log.Fatalf("WASM exit=%d, output truncated=%v", result.ExitCode, result.OutputTruncated)
}
fmt.Println(string(result.Stdout))
```

This is complementary to workspace sandboxing (Docker, K8s, etc.) — WASM provides lighter-weight isolation without requiring container infrastructure. See [Deployment](deployment.md) for more sandbox options.

## Permission System

Every tool execution goes through the permission engine:

| Mode | Behavior |
|------|----------|
| `Default` | Requires explicit allow rules or user confirmation |
| `AcceptEdits` | Allows file modifications, asks for shell commands |
| `Explore` | Read-only operations only |
| `Bypass` | Allows everything (for sandboxed environments) |
| `DontAsk` | Denies anything that would normally ask |

```go
permCtx := permission.NewContext(permission.ModeAcceptEdits)
a := agent.NewUnifiedAgent("bot", "...", cm,
    agent.WithPermissionContext(permCtx),
)
```

## Tool-level Middleware

Attach middleware to individual tools:

```go
type AuditMiddleware struct{}

func (m *AuditMiddleware) Wrap(ctx context.Context, name string, input map[string]any, next tool.ToolHandler) (any, error) {
    log.Printf("tool %s called with %v", name, input)
    return next(ctx, name, input)
}

myTool.AddMiddleware(&AuditMiddleware{})
```

## See Also

- [Architecture](architecture.md) — Tool system in the broader design
- [Middleware](middleware.md) — Agent-level middleware (including OnActing hook)
- [Deployment](deployment.md) — Workspace sandboxing for tool execution
- [Go Runtime Features](go-exclusive.md) — WASM sandbox details
