# Offline Operation

agentscope-go is designed for offline-first edge deployments where network connectivity is intermittent or unavailable.

## Design Principles

1. **Local-first**: All agent logic runs on-device without any cloud dependency
2. **Graceful degradation**: Cloud models enhance quality when available; local models ensure continuity when offline
3. **Automatic recovery**: The circuit breaker detects connectivity changes and switches models without manual intervention
4. **Data preservation**: MQTT QoS ensures messages are delivered when connectivity returns

## ConnectivityAwareModel

The core mechanism for offline operation. See [edge-deployment.md](edge-deployment.md) for configuration details.

### State Machine

```
         success            threshold failures
  ┌─────────────┐     ┌─────────────────────┐
  │             ▼     │                     ▼
  │          CLOSED ──┤                   OPEN
  │             ▲     │                     │
  │             │     │                     │ timeout elapsed
  │         success   │                     ▼
  │             │     │               HALF-OPEN
  │             └─────┼─────────────────────┘
  │                   │         failure
  └───────────────────┘
```

### Configuration Guidelines

| Environment | Failure Threshold | Recovery Timeout |
|-------------|-------------------|-----------------|
| Stable WiFi | 3-5 | 30s |
| Cellular/4G | 2-3 | 60s |
| Satellite | 1-2 | 300s |
| Air-gapped | N/A (use local only) | N/A |

## Running Without Any Network

For completely air-gapped deployments:

```go
// Just use Ollama directly — no ConnectivityAwareModel needed
local, _ := model.NewOllamaChatModel(model.OllamaConfig{
    Model: "qwen2.5:0.5b",
})
```

The agent functions identically to a cloud-connected agent, just with a smaller model.

## Message Queuing (MQTT)

For intermittent connectivity, use MQTT with QoS 1 or 2:

```go
transport, _ := mqtt.NewMQTTTransport("tcp://broker:1883",
    mqtt.WithAutoReconnect(true),
    mqtt.WithCleanSession(false),  // Resume session on reconnect
)

// QoS 1: At-least-once delivery (survives disconnects)
transport.Publish(ctx, "telemetry/temp", data, messagebus.WithQoS(1))
```

### QoS Levels

| QoS | Guarantee | Use Case |
|-----|-----------|----------|
| 0 | Fire and forget | High-frequency sensor data (OK to lose some) |
| 1 | At least once | Important events, commands |
| 2 | Exactly once | Critical commands (actuator safety) |

## Data Collection During Offline Periods

Pattern for buffering sensor data locally and syncing when reconnected:

```go
// Poll sensors continuously
sm.Poll(ctx)
readings := sm.Readings()

// Attempt to publish; MQTT client handles reconnection
for name, reading := range readings {
    data, _ := json.Marshal(reading)
    transport.Publish(ctx, "sensors/"+name, data, messagebus.WithQoS(1))
}
```

`CleanSession(false)` preserves the **subscription** side across reconnects, so
QoS >= 1 messages the broker queued for you while you were offline are redelivered
on reconnect.

It does not buffer outbound publishes. `MQTTTransport` sets no persistent store
(`SetStore`), and its `Publish` calls `token.Wait()` and returns `token.Error()`
to the caller when the client is disconnected. The adapter keeps no local queue.
Buffering outbound readings during an outage is the application's job: write them
to a local store (JSON Lines via `audit.NewFileLogger`, or `memory.FileStore`) and
drain it on reconnect. The loop above does not do this: it discards the error from
`Publish`, so readings produced during an outage are lost.

## Power Management

For battery-powered edge devices:

```go
// Use longer recovery timeouts to reduce probe frequency
cam := model.NewConnectivityAwareModel(local, cloud,
    model.WithFailureThreshold(1),       // Switch quickly to save power
    model.WithRecoveryTimeout(5*time.Minute), // Probe infrequently
)
```

### Sleep/Wake Pattern

```go
// Before sleep: stop watchdog and close connections
wd.Stop()
serial.Close()

// After wake: reopen and restart
serial.Open()
wd.Start()
sm.Poll(ctx)  // Get fresh sensor data
```

## Monitoring Without Cloud

Use the device's local filesystem for operational logs:

```go
import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/audit"

// NewFileLogger returns (*audit.FileLogger, error). The logger appends one JSON
// line per tool execution, permission decision, and error.
func setupAudit() (*audit.FileLogger, error) {
    return audit.NewFileLogger("/var/log/agent/audit.jsonl")
}
```

Logs can be synced to the cloud when connectivity is available, or retrieved via physical access (USB, serial console).
