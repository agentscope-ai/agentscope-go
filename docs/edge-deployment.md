# Edge Deployment Guide

This guide covers deploying agentscope-go on edge devices (Jetson Nano/Orin, Raspberry Pi, RISC-V boards) for IoT and embedded AI applications.

## Cross-Compilation

With `CGO_ENABLED=0` agentscope-go builds to a single static binary with no shared-library dependencies, and no edge feature requires CGO. CI cross-compiles every package and example this way for linux/arm64, arm, mips64le and riscv64. That is a linking guarantee rather than a deployment one: the edge features still need their external services (an Ollama server for local inference, an MQTT broker for fleet coordination) and kernel support for the device connectors (serial chardev, SocketCAN, gpiochip, i2c-dev).

### ARM64 (Jetson, RPi 4/5, Apple Silicon)

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o agent ./examples/edge_offline/
```

### ARM (RPi 3, older boards)

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o agent ./examples/edge_offline/
```

### RISC-V 64-bit

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=riscv64 go build -ldflags="-s -w" -o agent ./examples/edge_offline/
```

### MIPS (routers, industrial gateways)

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=mips64le go build -ldflags="-s -w" -o agent ./examples/edge_offline/
```

## Binary Size Optimization

CI enforces an 18 MB ceiling on the stripped linux/arm64 `examples/edge_offline` binary. The current build is about 6 MB, so there is room for optional dependencies before the gate trips.

| Technique | Savings |
|-----------|---------|
| `-ldflags="-s -w"` | ~30% (removes debug/symbol info) |
| Avoid importing `prometheus`, `otel`, `qdrant` | 5-8MB |
| Use `//go:build` tags for optional features | Variable |
| UPX compression (optional) | ~60% additional |

### Minimal import strategy

Edge examples deliberately avoid importing heavy packages. The MQTT adapter is behind a build tag (`//go:build mqtt`) so non-MQTT builds pay no size cost.

## Ollama + agentscope-go on Edge

### Jetson Nano/Orin (ARM64)

```bash
# Install Ollama
curl -fsSL https://ollama.com/install.sh | sh

# Pull a small model suitable for edge
ollama pull qwen2.5:0.5b    # ~400MB, runs well on 4GB RAM
ollama pull phi3:mini        # ~2.3GB, needs 8GB RAM

# Run the edge agent
./agent
```

### Raspberry Pi 4/5 (ARM64)

```bash
# Install Ollama (ARM64 builds available)
curl -fsSL https://ollama.com/install.sh | sh

# Use the smallest available model
ollama pull tinyllama         # ~637MB
ollama pull qwen2.5:0.5b     # ~400MB

# Deploy
scp agent pi@raspberrypi:/usr/local/bin/
ssh pi@raspberrypi '/usr/local/bin/agent'
```

## ConnectivityAwareModel

The `ConnectivityAwareModel` automatically routes between local and cloud models:

```go
import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"

local, _ := model.NewOllamaChatModel(model.OllamaConfig{Model: "qwen2.5:0.5b"})
cloud, _ := model.NewOpenAIChatModel(model.OpenAIConfig{APIKey: key, Model: "gpt-4o-mini"})

cam := model.NewConnectivityAwareModel(local, cloud,
    model.WithFailureThreshold(3),     // 3 failures before switching
    model.WithRecoveryTimeout(30*time.Second), // probe cloud every 30s
)

// Use like any ChatModel — routing is automatic
resp, err := cam.Chat(ctx, msgs)
fmt.Println(cam.ActiveModel()) // "cloud" or "local"
```

### Behavior

| Circuit State | Behavior |
|--------------|----------|
| Closed | All calls go to cloud |
| Open | All calls go to local (no cloud attempts) |
| Half-Open | Single probe to cloud; if success, close circuit |

## Context Window and Timeouts for Local Models

Two settings that cloud deployments never touch matter on the edge. Neither is
discovered automatically today (see
[agentscope-go#8](https://github.com/agentscope-ai/agentscope-go/issues/8)), so
set both explicitly.

### Match the agent's context size to the server's window

Ollama's context window (`num_ctx`) is configured on the server, per model,
and defaults to a small value. The framework cannot read it back. With
`WithContextConfig` set but no `ContextSize` override, the framework uses a
128000-token window for the Ollama adapter, so with the default `TriggerRatio`
compression starts near 102400
estimated tokens and may trigger only after the server's configured context
limit has already been exceeded; by then Ollama has silently dropped the oldest
messages. Raise `num_ctx` in a Modelfile and tell the agent the same number:

```
# Modelfile
FROM qwen2.5:0.5b
PARAMETER num_ctx 8192
```

```bash
ollama create qwen-edge -f Modelfile
```

```go
import (
    "time"

    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

local, err := model.NewOllamaChatModel(model.OllamaConfig{
    Model: "qwen-edge",
    // UnifiedAgent calls the non-streaming Chat path, so the HTTP client's
    // timeout (60s by default) bounds each HTTP request. Even a 0.5B–3B
    // model on a Raspberry Pi can need minutes for one long answer.
    ClientOptions: &model.ClientOptions{Timeout: 10 * time.Minute},
})
if err != nil {
    return err
}

ag := agent.NewUnifiedAgent("edge", "You are a helpful assistant.", local,
    agent.WithContextConfig(&agent.ContextConfig{
        ContextSize: 8192, // same as num_ctx above
    }),
)
```

`ClientOptions.Timeout` is a per-request timeout: one `UnifiedAgent.Reply` can
issue several model requests, retries and tool calls. Put the overall reply
deadline on the context passed to `ag.Reply`. A zero `ClientOptions.Timeout`
keeps the 60s default. To remove the client timeout entirely, pass your own
`HTTPClient` with `Timeout: 0` and rely on that context deadline.

### Cap the output length

`model.WithMaxTokens(n)` reaches Ollama as `max_tokens` (it was dropped on the
wire before the fix for agentscope-go#8). Small models tend to run on; a cap
of a few hundred tokens bounds the generated length of each response. It does
not bound elapsed time: a slow device can still take minutes to produce a
capped reply, so keep the request timeout and context deadline above as well.

## Network Considerations

### Offline-First Design

- Agent logic runs entirely on-device
- Cloud model is an optimization, not a requirement
- Sensor data collection continues regardless of connectivity
- MQTT QoS 1/2 ensures message delivery when reconnected

### Bandwidth-Constrained Environments

- Use small local models (0.5B-3B parameters) for most queries
- Reserve cloud calls for complex reasoning
- Set aggressive failure thresholds (2-3) to minimize wasted bandwidth
- Consider MQTT QoS 0 for high-frequency sensor data

## Systemd Service

```ini
[Unit]
Description=AgentScope Edge Agent
After=network.target ollama.service
Wants=ollama.service

[Service]
Type=simple
ExecStart=/usr/local/bin/agent
Restart=always
RestartSec=5
Environment="OLLAMA_HOST=http://localhost:11434"

[Install]
WantedBy=multi-user.target
```

## Hardware Requirements

| Device | RAM | Storage | Models |
|--------|-----|---------|--------|
| Jetson Orin Nano | 8GB | 32GB+ | Up to 7B |
| Raspberry Pi 5 | 8GB | 32GB+ | Up to 3B |
| Raspberry Pi 4 | 4GB | 16GB+ | Up to 1B |
| RISC-V (StarFive) | 4GB | 16GB+ | Up to 1B |
