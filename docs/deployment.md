# Deployment

## Agent Service

agentscope-go includes a built-in HTTP Agent Service for deploying agents as web services.

### Basic Setup

```go
svc := service.New(service.Config{
    Addr:           ":8080",
    AllowedOrigins: []string{"*"},
}, chatModel, func(name, prompt string, _ model.ChatModel) *agent.UnifiedAgent {
    return agent.NewUnifiedAgent(name, prompt, chatModel,
        agent.WithToolkit(tool.NewEnhancedToolkit()),
    )
})
svc.ListenAndServe()
```

### REST Endpoints

These routes belong to `service.Service`. The separate `app` package has its own route layout.

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/session` | Create a new session |
| GET | `/api/sessions` | List all sessions |
| POST | `/api/chat` | Sync chat; JSON body includes `session_id` and `message` |
| GET | `/api/chat/stream` | SSE chat; query parameters `session_id` and `message` |
| POST | `/api/confirm` | HITL confirmation; JSON body includes `session_id` and `tool_calls` |
| GET | `/api/models` | List available models |

The `/api/confirm` body, like the body of the `app` package's
`POST /api/chat/{sessionID}/confirm`, carries no reply ID, so the agent matches
the answer by tool-call ID alone. A confirmation posted after its reply was
canceled can therefore still answer a later reply's call that reuses the same
ID. [`service.NewScoped`](scoped-service.md) binds confirmations to the reply
that asked.

### Full Application

For production deployments with multi-session management, credentials, scheduling, and workspace isolation:

```go
app, _ := app.CreateApp(&app.AppConfig{
    Addr:           ":8080",
    AllowedOrigins: []string{"https://your-frontend.com"},
    Storage:        redisStorage,
    MessageBus:     messagebus.NewInMemoryMessageBus(),
})
```

### Canceling an App chat

For the Experimental `app` package, send
`POST /api/chat/{sessionID}/cancel` to cancel the active ordinary or streaming
chat, together with running tasks registered for that session in the App's
background-task manager. HTTP 200 acknowledges the cancellation request; it
does not mean cleanup has already finished. Canceling an idle session is a no-op.

Both `POST /api/chat/{sessionID}` and `POST /api/chat/{sessionID}/stream` retain
the request's parent cancellation and deadline. After cancellation, a new chat
for the same session receives HTTP 409 until the accepted Agent cores have
finished, including synchronous tool cleanup and child cores sharing the run
context. Closing a forwarded event stream alone does not release this ownership.
Delayed middleware cannot start a core after the request's execution scope closes.

Cancellation is cooperative: a tool that ignores its context retains the run
slot until it returns. Independently launched workers and unregistered background
work are outside this scope. The endpoint does not delete the session or its
history, provide cross-process cancellation, or prove that remote provider work
or billing has stopped. These routes are separate from `service.Service`.

The lifecycle follows Python's [chat-run registry](https://github.com/agentscope-ai/agentscope/blob/9b94a63407ec59881fde19300b61e469a0a7d8b2/src/agentscope/app/_manager/_chat_run_registry.py)
and [session cancellation dispatcher](https://github.com/agentscope-ai/agentscope/blob/9b94a63407ec59881fde19300b61e469a0a7d8b2/src/agentscope/app/_manager/_cancel_dispatcher.py).
Go uses per-run context cancellation and in-process ownership; it does not port
Python's message-bus broadcast or separate parked-session `interrupt` operation.

## Workspace Sandboxing

For untrusted tool execution, use isolated workspaces:

| Workspace | Isolation Level | Use Case |
|-----------|----------------|----------|
| `LocalWorkspace` | Directory-scoped | Development, trusted agents |
| `DockerWorkspace` | Container-level | Production, semi-trusted |
| `E2BWorkspace` | Cloud sandbox | Remote sandbox for tool execution |
| `K8sWorkspace` | Kubernetes Pod | Production clusters, multi-tenant |
| `OpenSandboxWorkspace` | Cloud sandbox API | Remote sandbox-as-a-service |
| `DaytonaWorkspace` | Dev environment | Daytona-managed dev containers |
| `AppleContainerWorkspace` | Apple Container | macOS-native lightweight containers |
| `BubblewrapWorkspace` | Linux bwrap | Minimal Linux sandboxing without Docker |

### Docker Workspace

```go
ws, err := workspace.NewDockerWorkspace(ctx, &workspace.DockerWorkspaceConfig{
    Image:   "python:3.11-slim",
    WorkDir: "/workspace",
})
if err != nil { log.Fatal(err) }
defer ws.Close(context.Background())
backend := workspace.NewToolBackend(ws)
ctx = tool.WithBackend(ctx, backend)
// Pass ctx to tool execution; only tools that consume the backend use this workspace.
```

### Kubernetes Workspace

Run agent tool execution inside hardened ephemeral Kubernetes Pods:

```go
runAsNonRoot := true
runAsUser := int64(1000)
ws, err := workspace.NewK8sWorkspace(&workspace.K8sConfig{
    Namespace:             "agent-sandbox",
    PodName:               "agent-workspace",
    Image:                 "ubuntu:22.04",
    APIServer:             "https://kubernetes.default.svc",
    SecretToken:           model.NewSecretStr(os.Getenv("K8S_TOKEN")),
    PodTTLSeconds:         3600,  // activeDeadlineSeconds: stop after 1h; Close deletes the Pod
    DisableServiceAccount: true,  // no SA token inside pod
    SecurityContext: &workspace.PodSecurityContext{
        RunAsNonRoot: &runAsNonRoot,
        RunAsUser:    &runAsUser,
    },
    Resources: &workspace.ResourceRequirements{
        CPULimit:      "2000m",
        MemoryLimit:   "1Gi",
        CPURequest:    "200m",
        MemoryRequest: "256Mi",
    },
    Labels: map[string]string{
        "app.kubernetes.io/managed-by": "agentscope",
    },
})
if err != nil { log.Fatal(err) }
defer ws.Close()
backend := workspace.NewToolBackend(ws)
ctx = tool.WithBackend(ctx, backend)
```

### Kubernetes Cluster Tools

Read-only tools for querying existing clusters (no mutation, secrets blocked):

```go
getTool := workspace.NewKubectlGetTool("/path/to/kubeconfig")
logTool := workspace.NewKubectlLogTool("/path/to/kubeconfig")
tk := tool.NewToolkit(getTool, logTool)
```

`kubectl_get` supports: pods, deployments, services, configmaps, events, nodes, namespaces, ingresses, jobs, cronjobs, statefulsets, daemonsets, replicasets, pvc, hpa. Secrets are explicitly blocked.

### OpenSandbox Workspace

Use the OpenSandbox API for fully managed cloud sandboxes:

```go
ws, _ := workspace.NewOpenSandboxWorkspace(workspace.OpenSandboxConfig{
    APIKey:   os.Getenv("OPENSANDBOX_API_KEY"),
    BaseURL:  "https://api.opensandbox.dev",
    Template: "python:3.11",
})
```

### Daytona Workspace

Leverage Daytona for development-oriented sandbox environments:

```go
ws, _ := workspace.NewDaytonaWorkspace(workspace.DaytonaConfig{
    BaseURL:     "https://daytona.example.com",
    APIKey:      os.Getenv("DAYTONA_API_KEY"),
    WorkspaceID: "my-workspace",
})
```

### Apple Container Workspace

On macOS, use Apple's Container framework for lightweight native isolation:

```go
ws, _ := workspace.NewAppleContainerWorkspace(workspace.AppleContainerConfig{
    Image: "swift:latest",
    Name:  "agent-sandbox",
})
```

### Bubblewrap Workspace

Minimal Linux sandboxing via `bwrap` without needing Docker:

```go
ws, _ := workspace.NewBubblewrapWorkspace(workspace.BubblewrapConfig{
    RootDir:      "/tmp/agent-sandbox",
    AllowNetwork: false,
})
```

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

Key properties:
- **Memory limit**: Bounds WASM linear memory; it does not cap all host/runtime process memory
- **Time-limited**: Execution timeout
- **CPU-limited**: Fuel (instruction count) budget
- **Portability**: Requires a compatible module, WASI imports, and installed runtime on the target
- **Network settings**: The CLI adapter does not pass flags enabling guest network access

## Hot-Reload Configuration

Update agent configuration at runtime without restarting. The `hotreload` package watches files for changes and notifies handlers.

### File Watcher

```go
w := hotreload.NewWatcher(hotreload.WatcherConfig{
    PollInterval: 2 * time.Second,
})

w.Watch("config/agent.json", func(evt hotreload.ChangeEvent, data []byte) error {
    log.Printf("Config changed at %s", evt.Timestamp)
    // Parse and apply new config
    return nil
})

w.Start(ctx)
defer w.Stop()
```

### Typed Config Reloader

For type-safe config with automatic JSON unmarshaling:

```go
type AgentConfig struct {
    SystemPrompt string   `json:"system_prompt"`
    MaxIters     int      `json:"max_iters"`
    Model        string   `json:"model"`
    Tools        []string `json:"tools"`
}

reloader, _ := hotreload.NewReloader[AgentConfig](w, "config/agent.json",
    hotreload.WithOnChange(func(old, new_ *AgentConfig) {
        log.Printf("Prompt changed: %q -> %q", old.SystemPrompt, new_.SystemPrompt)
    }),
)

// Read the current config (lock-free atomic pointer)
cfg := reloader.Get()
```

## Agent Pool (High-Throughput Deployment)

Fan out work across bounded workers for high-throughput batch processing. There
are two pools with different shapes; pick by whether you want a handler or one
agent instance per worker.

### Handler pool (`runtime.NewPool`)

This is what `examples/agent_pool` uses. Each request carries its own result
channel, backpressure shows up as `ErrPoolFull` from `Submit`, and the pool stops
with `Shutdown(ctx)`. There is no `Close` method.

```go
pool := runtime.NewPool(
    runtime.PoolConfig{MaxWorkers: 8, QueueSize: 100, WorkerTimeout: 10 * time.Second},
    func(ctx context.Context, req *runtime.Request) *runtime.Result {
        out, err := a.Reply(ctx, req.Input)
        if err != nil {
            return &runtime.Result{RequestID: req.ID, Error: err}
        }
        text := ""
        if txt := out.GetTextContent("\n"); txt != nil { // GetTextContent returns *string
            text = *txt
        }
        return &runtime.Result{RequestID: req.ID, Output: text}
    },
)

resultCh := make(chan *runtime.Result, 1)
if err := pool.Submit(&runtime.Request{
    ID: "req-001", Input: "Summarize this document...", Ctx: ctx, ResultCh: resultCh,
}); err != nil {
    log.Printf("submit: %v", err) // ErrPoolFull under backpressure, ErrPoolClosed after Shutdown
}
res := <-resultCh
log.Printf("Result: %s (%v)", res.Output, res.Duration)
log.Printf("Stats: %+v", pool.Stats())

if err := pool.Shutdown(ctx); err != nil {
    log.Printf("shutdown: %v", err)
}
```

### Per-worker agent pool (`runtime.NewAgentPool`)

Each worker owns its own agent instance. Dependencies captured by the factory may
still be shared and must support concurrent use, and agent history persists
across the jobs one worker handles.

`AgentFactory` is `func() agent.Agent`, and the `agent.Agent` interface requires
`ID() string`, `Reply(ctx, ...any)`, `Observe`, `Interrupt` and
`SetConsoleOutputEnabled`. `*agent.UnifiedAgent` has `Name()` and
`Reply(ctx, string)` and none of the rest, so passing one directly does not
compile. Bridge it with an adapter:

```go
// poolAgent adapts *agent.UnifiedAgent to agent.Agent for runtime.AgentFactory.
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

// UnifiedAgent has no equivalent for these two. Cancellation goes through the
// context passed to Reply, and UnifiedAgent exposes no console-output toggle
// (that method lives on AgentBase, which it does not embed), so both are no-ops.
func (w poolAgent) Interrupt(context.Context, *message.Msg) error { return nil }
func (w poolAgent) SetConsoleOutputEnabled(bool)                  {}

var _ agent.Agent = poolAgent{} // compile-time check

pool := runtime.NewAgentPool(
    func() agent.Agent {
        return poolAgent{a: agent.NewUnifiedAgent("worker", "You are a data processor.", cm,
            agent.WithToolkit(tool.NewEnhancedToolkit()))}
    },
    runtime.Workers(8),
    runtime.QueueSize(100),
)
defer pool.Close() // AgentPool has Close; Pool has Shutdown instead

for _, item := range workItems {
    resultCh, err := pool.Submit(ctx, item)
    if err != nil {
        log.Printf("submit: %v", err)
        continue
    }
    go func(ch <-chan runtime.PoolResult) {
        res := <-ch
        if res.Err != nil {
            log.Printf("Error: %v", res.Err)
            return
        }
        if res.Output == nil {
            return
        }
        // GetTextContent returns *string, so dereference it and check for nil.
        // Printing the pointer yields an address.
        if txt := res.Output.GetTextContent("\n"); txt != nil {
            log.Printf("Result: %s", *txt)
        }
    }(resultCh)
}
```

## Deterministic Replay for CI/CD

Record model responses once, then replay them in tape order. Replay skips model API calls but does not validate prompt equality or intercept tool side effects. Use mock or isolated tools for offline CI.

### Recording

```go
recorder := replay.NewRecorder()
a := agent.NewUnifiedAgent("bot", "You are a test agent.", cm,
    agent.WithMiddlewares(recorder),
)
if _, err := a.Reply(ctx, "Summarize the Q3 report"); err != nil { log.Fatal(err) }
store, err := replay.NewFileStore("testdata")
if err != nil { log.Fatal(err) }
if err := store.Save(ctx, "q3_summary", recorder.Tape()); err != nil { log.Fatal(err) }
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

func TestQ3Summary(t *testing.T) {
    ctx := context.Background()
    store, err := replay.NewFileStore("testdata")
    if err != nil { t.Fatal(err) }
    tape, err := store.Load(ctx, "q3_summary")
    if err != nil { t.Fatal(err) }
    placeholder := agenttest.NewMockModel()
    replayer := replay.NewReplayer(tape)
    a := agent.NewUnifiedAgent("bot", "You are a test agent.", placeholder,
        agent.WithMiddlewares(replayer),
    )
    reply, err := a.Reply(ctx, "Summarize the Q3 report")
    if err != nil { t.Fatal(err) }
    text := reply.GetTextContent("\n")
    if text == nil || !strings.Contains(*text, "revenue") {
        t.Fatalf("unexpected reply: %v", text)
    }
    if len(placeholder.Calls()) != 0 { t.Fatal("replay called the placeholder model") }
}
```

The test uses a non-nil offline mock because `NewUnifiedAgent` rejects nil models. Only model responses are replayed; real tools, if configured, can still execute.

## Scheduled Tasks

### In-process scheduling

```go
// ag is the agent that runs each scheduled task, e.g. an *agent.UnifiedAgent.
// Reply is a method on the agent, not a package-level function.
scheduler := schedule.NewInMemoryScheduler()
defer scheduler.Close()

scheduler.Schedule(ctx, &schedule.Task{
    Name:     "daily-report",
    RunAt:    time.Now().Add(24 * time.Hour), // omit to fire immediately
    Interval: 24 * time.Hour,
}, func(ctx context.Context, task *schedule.Task) error {
    _, err := ag.Reply(ctx, "Generate the daily summary report.")
    return err
})
```

`RunAt` fires once at that instant. `Interval` fires immediately when `RunAt` is
zero, then every `Interval` after that first fire, so
`Schedule(&Task{Interval: 24 * time.Hour}, fn)` runs `fn` before `Schedule`
returns. Set `RunAt` as well to delay the first fire. A custom `Scheduler` implementation may also implement the optional
`schedule.TaskRemover` interface; see "Custom Scheduler implementations" below.

The two ways to schedule differ in ways that matter:

| | `schedule` package | `app` HTTP API |
|---|---|---|
| Schedule shape | `RunAt` (one-shot) or `Interval` (fixed duration) | five-field cron / `@`-aliases |
| First fire | **immediately**, when `RunAt` is zero | next matching minute-grid slot |
| Persistence | none (process-local) | whatever `AppConfig.Storage` provides |
| Stop it | `Scheduler.Cancel` | `DELETE /api/schedule/{id}` (see below) |

### Scheduling through the HTTP API

The `app` package (`app.CreateApp`, see [Full Application](#full-application))
exposes cron-style schedules. These routes are not on `service.Service`; the two
packages have separate route layouts:

```
POST   /api/schedule          {"session_id": "...", "cron_expr": "0 9 * * 1-5", "input": "..."}
GET    /api/schedule
GET    /api/schedule/{id}
PATCH  /api/schedule/{id}     {"input": "...", "status": "paused"}
DELETE /api/schedule/{id}
```

`cron_expr` is standard five-field cron (minute hour day-of-month month
day-of-week) with `*`, values, ranges (`a-b`), steps (`*/n`, `a-b/n`, `a/n`),
comma lists, month and day-of-week names, and these aliases: `@hourly`,
`@daily` and `@midnight`, `@weekly`, `@monthly`, `@yearly` and `@annually`, plus
`@every_5m`, `@every_10m`, `@every_30m`, `@every_1h` and `@every_12h`. Day-of-week
accepts 0-7 where both 0 and 7 are Sunday. When both day fields are restricted, a
day matches if either matches (Vixie cron semantics), so `0 9 */1 * 1-5` fires
every day, not only on weekdays: only a literal `*` counts as unrestricted, so
`*/1` counts as restricted.

This is Vixie cron. The Quartz-only extensions (`?`, `L`, `W`, `#`, and a leading
seconds field) are rejected with 400 rather than misparsed, so an expression
migrated from Quartz fails at creation instead of firing at the wrong times. Drop
the seconds field and replace `?` with `*`.

The expression is parsed and validated before anything is persisted: a malformed
one, or one that cannot fire within the next 8 years (`0 0 30 2 *`), is rejected
with **HTTP 400**. Omit `cron_expr` for a single run about a second later, or
set `run_once: true` to fire once at the next matching slot without repeating.
After the single fire the record reports `status: "completed"` and its task entry
is dropped.

### Timezone: cron runs in the process timezone

Next-fire times are computed in `time.Local` of the server process, and the API
has no per-schedule timezone field. Set `TZ` in the container or pod spec, and
pin it explicitly rather than inheriting the host:

```yaml
env:
  - name: TZ
    value: Asia/Shanghai
```

Two consequences worth knowing:

- Zones whose UTC offset is not a whole hour (Asia/Kolkata +5:30, Asia/Tehran
  +3:30, Asia/Yangon +6:30, Australia/Darwin +9:30, America/St_Johns −3:30,
  Asia/Kathmandu +5:45) are supported. The scheduler works on local calendar
  fields rather than absolute-time rounding.
- On a DST fall-back day a repeated local hour is walked through, so an
  expression targeting it can fire twice; on a spring-forward day a time inside
  the gap is skipped to the next matching day.

### Stopping a schedule

Use `DELETE /api/schedule/{id}`. It marks the record canceled and cancels the
armed task, and a cancel that races with a re-arm also cancels the freshly armed
task, so no further chat runs.

`PATCH {"status": "paused"}` also stops the chain, and the change is one-way: no
armed task is left to resume, so setting `active` again is refused with 409 and
the status stays as it was. Create a new schedule instead. Any non-active status
behaves the same way, including an empty one, so two PATCH calls cannot reopen
the path.

The status field is not validated against a vocabulary, so
`PATCH {"status": "canceled"}` marks the record canceled without canceling the
armed task, and the next fire still runs one chat before the chain stops. Use
`DELETE` to cancel a schedule.

### Upgrade note: schedules fire at different times and no longer fire on creation

Before this release `cron_expr` was not parsed as cron. Only `@hourly`, `@daily`,
`@every_5m`, `@every_10m` and `@every_30m` were recognized. Everything else,
including `@every_1h`, `@every_12h`, `@weekly`, `@yearly` and any five-field
expression such as `0 9 * * *`, became an hourly interval without any diagnostic.
Recognized values became a `schedule.Interval` with no `RunAt`, and the in-memory
scheduler runs an Interval task immediately, before starting the ticker.

Both halves changed:

| | before | after |
|---|---|---|
| First fire | immediately at creation | at the next matching minute-grid slot |
| `@every_5m` created 10:03 | 10:03, 10:08, 10:13 … | 10:05, 10:10, 10:15 … |
| `@every_12h` | hourly (unrecognized → default) | 00:00 and 12:00 |
| `@every_1h` | hourly (unrecognized → default) | on the hour |
| `0 9 * * *` | hourly (unrecognized → default) | daily at 09:00 |

Persisted schedules keep their stored expression but fire on the new grid, and no
longer fire on creation. Audit them after upgrading in both directions: a job that
used to run hourly may now run twice a day or once a day, and a job that used to
run once at deploy time now waits for its next slot. Expressions the old parser
accepted and the new one rejects now answer 400; see "Scheduling through the HTTP
API" above for the accepted syntax.

### Custom Scheduler implementations

`InMemoryScheduler` is process-local: schedules do not survive a restart and are
not shared between replicas. For a persistent or distributed backend, implement
`schedule.Scheduler`. Two details matter:

- A cron schedule re-arms by scheduling a fresh one-shot task per fire.
  Implement `schedule.TaskRemover` (`Remove(taskID string)`) so spent entries can
  be dropped; otherwise a per-minute schedule accumulates one entry per fire for
  the life of the process. `InMemoryScheduler` implements it.
- The manager publishes the record before calling `Schedule`, and a callback may
  run synchronously inside `Schedule`. Do not assume the callback runs later, and
  do not call back into the manager while holding your own lock.

## Production Checklist

- [ ] Set `permission.ModeDefault` or `ModeAcceptEdits` (never `Bypass` in production)
- [ ] Use `DockerWorkspace`, `K8sWorkspace`, or `E2BWorkspace` for tool execution
- [ ] Configure `ClientOptions.Timeout` for your expected response times
- [ ] Set up `TracingMiddleware` with OpenTelemetry exporter
- [ ] Use `ReplyBudgetControlMiddleware` to cap token spending; use `CostTrackerMiddleware` with `WithMaxCostUSD` to stop subsequent calls after accounted cost reaches the threshold (single or concurrent calls can overshoot)
- [ ] Rotate API keys and use `model.SecretStr` to prevent key leakage in logs
- [ ] Put the Agent Service behind authentication (it has no built-in auth)
- [ ] Use Redis-backed storage and message bus for multi-instance deployments
- [ ] Configure `access` policies for multi-tenant resource sharing
- [ ] Enable `hotreload` to update agent configs without downtime
- [ ] Use `replay` tapes in CI to test agent behavior deterministically
- [ ] Configure `GuardrailMiddleware` for output content filtering
- [ ] Use `AgentPool` with appropriate worker counts for batch workloads

## See Also

- [Architecture](architecture.md) — Package structure and design
- [Go Runtime Features](go-exclusive.md) — Replay, Pool, Hot-reload, WASM, TCP Mesh, Bench
- [Examples](examples.md) — Runnable demos for all deployment patterns
