# Go Runtime Features

This guide covers six features provided by this Go repository. It does not assert feature exclusivity relative to a particular Python release.

## 1. Deterministic Replay

**Package:** `pkg/agentscope/replay`

Record model call responses during a session, then replay them in tape order through `OnModelCall`. Replay skips the model API, but does not compare the incoming prompt with the recorded request or intercept tool execution. Use mock/isolated tools for offline tests, and pass a non-nil model (for example `agenttest.NewMockModel()`) because the agent constructor and token counting still require it.

### Core Types

- **`Tape`** — a JSON-serializable sequence of `Entry` records with version metadata
- **`Entry`** — one recorded model call: agent name, model name, messages, tools, response, error, timestamp, duration
- **`Middleware`** — implements `middleware.Middleware` with `OnModelCall` hook

### Recording

```go
package main

import (
    "context"
    "log"
    "os"

    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/replay"
)

func main() {
    ctx := context.Background()
    cm, err := model.NewDashScopeChatModel(model.DashScopeConfig{
        APIKey: os.Getenv("DASHSCOPE_API_KEY"), Model: "qwen-plus",
    })
    if err != nil { log.Fatal(err) }
    recorder := replay.NewRecorder()
    a := agent.NewUnifiedAgent("analyst", "You are a data analyst.", cm,
        agent.WithMiddlewares(recorder),
    )
    if _, err := a.Reply(ctx, "Analyze the Q3 revenue trends"); err != nil { log.Fatal(err) }
    store, err := replay.NewFileStore("testdata")
    if err != nil { log.Fatal(err) }
    if err := store.Save(ctx, "q3_analysis", recorder.Tape()); err != nil { log.Fatal(err) }
}
```

### Replaying in Tests

```go
package example

import (
    "context"
    "strings"
    "testing"

    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agenttest"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/replay"
)

func TestQ3Analysis(t *testing.T) {
    ctx := context.Background()
    store, err := replay.NewFileStore("testdata")
    if err != nil { t.Fatal(err) }
    tape, err := store.Load(ctx, "q3_analysis")
    if err != nil { t.Fatal(err) }
    placeholder := agenttest.NewMockModel()
    replayer := replay.NewReplayer(tape)
    a := agent.NewUnifiedAgent("analyst", "You are a data analyst.", placeholder,
        agent.WithMiddlewares(replayer),
    )
    reply, err := a.Reply(ctx, "Analyze the Q3 revenue trends")
    if err != nil { t.Fatal(err) }
    text := reply.GetTextContent("\n")
    if text == nil || !strings.Contains(*text, "revenue") {
        t.Fatalf("unexpected reply: %v", text)
    }
    if len(placeholder.Calls()) != 0 { t.Fatal("replay called the placeholder model") }
}
```

### Properties

- **No model API keys in CI**: With a mock model, replay skips the model API
- **Offline tests**: Require mock or isolated tools as well; real tool calls can still perform network/file operations
- **Multi-turn**: Records entire multi-step ReAct conversations
- **JSON format**: Tapes are human-readable, diffable, and versionable in Git

---

## 2. Fan-out Agent Pool

**Package:** `pkg/agentscope/runtime` (type `AgentPool`)

A work-queue that dispatches inputs to a fixed set of worker goroutines, each owning a fresh agent instance created from a factory function. Ideal for batch processing, data pipelines, and high-throughput scenarios.

### Code Example

```go
package main

import (
    "context"
    "fmt"
    "sync"

    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/runtime"
)

// poolAgent adapts *agent.UnifiedAgent to the agent.Agent interface that
// runtime.AgentFactory requires.
type poolAgent struct{ a *agent.UnifiedAgent }

func (w poolAgent) ID() string { return w.a.Name() }

func (w poolAgent) Reply(ctx context.Context, args ...any) (*message.Msg, error) {
    // AgentPool.Submit enqueues a single string. Guard the index so a caller
    // that passes nothing degrades to an empty prompt instead of panicking.
    var input string
    if len(args) > 0 {
        input, _ = args[0].(string)
    }
    return w.a.Reply(ctx, input)
}

func (w poolAgent) Observe(ctx context.Context, msgs []*message.Msg) error {
    return w.a.Observe(ctx, msgs)
}

func (w poolAgent) Interrupt(context.Context, *message.Msg) error { return nil }
func (w poolAgent) SetConsoleOutputEnabled(bool)                  {}

var _ agent.Agent = poolAgent{}

func main() {
    cm, _ := model.NewDashScopeChatModel(model.DashScopeConfig{
        APIKey: "sk-...",
        Model:  "qwen-plus",
    })

    // AgentPool's factory must return agent.Agent, which *agent.UnifiedAgent
    // does not implement (it lacks ID/Interrupt/SetConsoleOutputEnabled and its
    // Reply takes a string, not variadic args). Adapt it; see poolAgent below.
    pool := runtime.NewAgentPool(
        func() agent.Agent {
            return poolAgent{a: agent.NewUnifiedAgent("classifier", "Classify the sentiment.", cm)}
        },
        runtime.Workers(8),
        runtime.QueueSize(100),
    )

    ctx := context.Background()
    defer pool.Close()

    // Fan out the classification tasks.
    inputs := []string{"Great product!", "Terrible service.", "It's okay I guess."}

    var wg sync.WaitGroup
    for _, input := range inputs {
        wg.Add(1)
        resultCh, err := pool.Submit(ctx, input)
        if err != nil {
            wg.Done()
            fmt.Printf("submit failed: %v\n", err)
            continue
        }
        go func(ch <-chan runtime.PoolResult) {
            defer wg.Done()
            res := <-ch
            if res.Err != nil || res.Output == nil {
                return
            }
            // GetTextContent returns *string: dereference and nil-check it.
            if txt := res.Output.GetTextContent("\n"); txt != nil {
                fmt.Printf("[%s] -> %s (took %v)\n", res.Input, *txt, res.Duration)
            }
        }(resultCh)
    }
    wg.Wait()
}
```

### Properties

- **No shared state**: Each worker owns its own agent instance
- **Bounded concurrency**: Worker count and queue size are configurable
- **Back-pressure**: Submitters block when the queue is full
- **Graceful shutdown**: `Close()` drains pending work

---

## 3. Hot-Reload Config

**Package:** `pkg/agentscope/hotreload`

Watch configuration files for changes and apply updates at runtime without restarting the process. Built on polling (not inotify) for cross-platform compatibility.

### Code Example

```go
package main

import (
    "context"
    "fmt"
    "time"

    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/hotreload"
)

type AgentConfig struct {
    SystemPrompt string   `json:"system_prompt"`
    MaxIters     int      `json:"max_iters"`
    Model        string   `json:"model"`
    Temperature  float64  `json:"temperature"`
}

func main() {
    ctx := context.Background()

    // Create a file watcher with 2-second polling
    w := hotreload.NewWatcher(hotreload.WatcherConfig{
        PollInterval: 2 * time.Second,
    })

    // Create a typed config reloader
    reloader, _ := hotreload.NewReloader[AgentConfig](w, "config/agent.json",
        hotreload.WithOnChange(func(old, new_ *AgentConfig) {
            fmt.Printf("Config updated: model %s → %s\n", old.Model, new_.Model)
        }),
    )

    w.Start(ctx)
    defer w.Stop()

    // Use the config — reads are lock-free via atomic pointer
    for {
        cfg := reloader.Get()
        fmt.Printf("Current model: %s, prompt: %s\n", cfg.Model, cfg.SystemPrompt)
        time.Sleep(5 * time.Second)
    }
}
```

### Properties

- **Generics**: `Reloader[T]` works with any config struct
- **Lock-free reads**: Uses `atomic.Pointer[T]` for zero-contention access
- **Custom parsers**: Default is JSON, override with `WithParser()` for YAML/TOML
- **Multi-file**: Watch multiple files with different handlers
- **Cross-platform**: Polling-based, works on Linux, macOS, Windows

---

## 4. WASM Sandbox

**Package:** `pkg/agentscope/wasm`

The CLI implementation enforces fuel, linear-memory, timeout, directory-grant, and output-capture settings through Wasmtime. Wasmer and wasm3 may be discovered, but execution with the default resource limits returns `ErrUnsupportedLimits`. Select Wasmtime explicitly and check both errors and result status. Empty `AllowedPaths` grants no host directories.

### Code Example

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/wasm"
)

func main() {
    ctx := context.Background()
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
}
```

### Properties

- **Memory limit**: Bounds WASM linear memory; it does not cap all host/runtime process memory
- **Time-limited**: Kills runaway modules
- **CPU-limited**: Fuel budget (wasmtime) prevents infinite loops
- **Network settings**: The CLI adapter does not pass flags enabling guest network access
- **Portability**: Requires a compatible module, WASI imports, and installed runtime on the target
- **No container runtime**: Lighter than Docker, no daemon needed

---

## 5. TCP Agent Mesh

**Package:** `pkg/agentscope/a2a/grpc`

Bidirectional agent-to-agent communication over TCP using newline-delimited JSON messages. Designed for building distributed agent networks where agents on different machines communicate directly.

### Code Example

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "time"

    mesh "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/a2a/grpc"
)

func main() {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    server, err := mesh.NewServer("127.0.0.1:0")
    if err != nil { log.Fatal(err) }
    defer server.Close()
    server.OnMessage(func(msg *mesh.Message) *mesh.Message {
        return &mesh.Message{ID: msg.ID, From: "router", To: msg.From, Payload: msg.Payload}
    })
    go func() {
        if err := server.Listen(ctx); err != nil { log.Print(err) }
    }()

    client, err := mesh.NewClient(server.Addr())
    if err != nil { log.Fatal(err) }
    defer client.Close()
    resp, err := client.Send(ctx, &mesh.Message{
        ID: "request-1", From: "agent-alpha", To: "agent-beta", Method: "analyze",
        Payload: json.RawMessage(`{"task":"analyze"}`),
    })
    if err != nil { log.Fatal(err) }
    fmt.Println(string(resp.Payload))
}
```

### Properties

- **Simple protocol**: Newline-delimited JSON over TCP, not the gRPC wire protocol
- **Streaming protocol**: `Client.Stream` accepts multiple responses using `IsStream`/`StreamEnd`. The basic `Server.OnMessage` callback returns a single message; multi-frame servers must use a transport handler.
- **Bidirectional**: Both sides can send and receive
- **Fixed receive limit**: The scanner is capped at 1 MiB; this is not exposed as a public buffer-size option
- **Cancellation and backpressure**: Each client stream buffers up to 64 messages. Cancellation, disconnect, or overflow closes it; buffered messages remain readable. Only receiving `StreamEnd` confirms success. Local cancellation does not stop remote work; retries need fresh IDs.
- **Complementary to A2A HTTP**: Use TCP mesh for internal clusters, HTTP A2A for cross-network

---

## 6. Agent Load Testing

**Package:** `pkg/agentscope/bench`

Define load test scenarios with configurable concurrency, duration, iterations, and ramp-up. The runner produces reports with throughput, latency percentiles, and error breakdowns.

### Code Example

```go
package main

import (
    "context"
    "fmt"
    "time"

    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/bench"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func main() {
    cm, _ := model.NewDashScopeChatModel(model.DashScopeConfig{
        APIKey: "sk-...",
        Model:  "qwen-plus",
    })

    runner := bench.NewRunner()
    report, _ := runner.Run(context.Background(), &bench.Scenario{
        Name:           "simple-qa",
        Concurrency:    10,
        Duration:       60 * time.Second,
        RampUpDuration: 10 * time.Second,
        Run: func(ctx context.Context, iteration int) error {
            a := agent.NewUnifiedAgent("bot", "Answer briefly.", cm)
            _, err := a.Reply(ctx, fmt.Sprintf("Question %d: What is Go?", iteration))
            return err
        },
    })

    fmt.Printf("Scenario: %s\n", report.Scenario)
    fmt.Printf("Throughput: %.2f iter/s\n", report.Throughput)
    fmt.Printf("Latency p50=%v p95=%v p99=%v\n",
        report.Latencies.P50, report.Latencies.P95, report.Latencies.P99)
    fmt.Printf("Success: %d, Failures: %d\n", report.Successes, report.Failures)
}
```

### Report Fields

| Field | Type | Description |
|-------|------|-------------|
| `Throughput` | `float64` | Iterations per second |
| `Latencies.P50` | `time.Duration` | Median latency |
| `Latencies.P95` | `time.Duration` | 95th percentile latency |
| `Latencies.P99` | `time.Duration` | 99th percentile latency |
| `Successes` | `int64` | Number of successful iterations |
| `Failures` | `int64` | Number of failed iterations |
| `Errors` | `map[string]int64` | Error message to count breakdown |

### Properties

- **Duration-based or iteration-based**: Set `Duration`, `Iterations`, or both
- **Ramp-up**: Gradually increase concurrency over `RampUpDuration`
- **Setup/Teardown**: Optional hooks for test fixture management
- **Concurrent-safe**: Atomic counters, no shared mutable state

### Regression baselines

`bench.Battery` runs named scenarios together, and `CheckBaseline` compares the
result against a saved baseline (p95 latency with fractional-millisecond
precision and 10% slack, plus success rate as an exact floor), returning one string per
regression, so a performance gate is a few lines in a normal Go test:

```go
reports, err := bench.RunBattery(ctx, bench.Battery{
    {Name: "simple-qa", Scenario: scenario},
})
if err != nil {
    t.Fatal(err)
}

base, err := bench.LoadBaseline("testdata/bench-baseline.json")
if err != nil {
    t.Skipf("no baseline recorded yet: %v", err)
}
for _, msg := range bench.CheckBaseline(reports, base) {
    t.Errorf("perf regression: %s", msg)
}

// Refresh only on purpose, never automatically:
//   bench.SaveBaseline(path, bench.BaselineFromReports(reports))
```

---

## Quick Reference

| Feature | Package | Key Type | Primary Use Case |
|---------|---------|----------|------------------|
| Deterministic Replay | `replay` | `Middleware`, `Tape` | CI/CD testing without API keys |
| Fan-out Agent Pool | `runtime` | `AgentPool` | Batch processing, high throughput |
| Hot-Reload Config | `hotreload` | `Watcher`, `Reloader[T]` | Live config updates without restart |
| WASM Sandbox | `wasm` | `Sandbox`, `CLIRuntime` | Untrusted code execution |
| TCP Agent Mesh | `a2a/grpc` | `Server`, `Client`, `Transport` | Distributed agent networks |
| Agent Load Testing | `bench` | `Runner`, `Scenario`, `Report` | Performance benchmarking |

## See Also

- [Architecture](architecture.md) — Package structure for these runtime capabilities
- [Deployment](deployment.md) — Production deployment with pools, replay, and hot-reload
- [Middleware](middleware.md) — Replay middleware hook details
- [Examples](examples.md) — Runnable demos for each feature
