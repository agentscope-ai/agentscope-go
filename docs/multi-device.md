# Multi-Device Coordination

This guide covers coordinating multiple edge AI agents across devices using the PubSub messaging interface.

## Architecture

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│  Sensor Node │     │ Processor    │     │ Actuator     │
│  (RPi + I2C) │     │ (Jetson+LLM) │     │ (RPi + GPIO) │
│              │     │              │     │              │
│  Agent + Pub ├────►│ Sub + Agent  ├────►│ Sub + Agent  │
└──────────────┘     │ + Pub        │     └──────────────┘
                     └──────────────┘
        ▲                    │                    │
        └────────────────────┴────────────────────┘
                    MQTT Broker (Mosquitto)
```

## PubSub Interface

The `messagebus.PubSub` interface is the communication layer between devices:

```go
type PubSub interface {
    Publish(ctx context.Context, topic string, data []byte, opts ...PubOption) error
    Subscribe(ctx context.Context, topic string, opts ...SubOption) (<-chan PubSubMessage, error)
    Unsubscribe(topic string) error
    Close() error
}
```

## MQTT Setup

### Broker (any device or separate server)

```bash
# Install Mosquitto
sudo apt install mosquitto mosquitto-clients

# Enable and start
sudo systemctl enable mosquitto
sudo systemctl start mosquitto
```

### Agent Configuration

```go
//go:build mqtt

import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/messagebus/mqtt"

transport, err := mqtt.NewMQTTTransport("tcp://broker-ip:1883",
    mqtt.WithClientID("sensor-agent-01"),
    mqtt.WithTopicPrefix("fleet/zone-a/"),
    mqtt.WithAutoReconnect(true),
    mqtt.WithKeepAlive(30*time.Second),
    mqtt.WithOnDisconnect(func(err error) {
        log.Warn("MQTT disconnected:", err)
    }),
)
```

## Topic Design

Recommended topic hierarchy for fleet operations:

```
fleet/
├── {zone}/
│   ├── sensors/
│   │   ├── temperature    # Sensor readings
│   │   ├── humidity
│   │   └── motion
│   ├── commands/
│   │   ├── actuator       # Commands to actuators
│   │   └── mode           # Operation mode changes
│   ├── status/
│   │   ├── {device-id}    # Heartbeat/health
│   │   └── alerts         # Critical alerts
│   └── coordination/
│       ├── tasks          # Task distribution
│       └── acks           # Task acknowledgments
```

## Coordination Patterns

### 1. Sensor-Processor-Actuator Pipeline

The most common pattern: sensor nodes publish data, a processor node (with LLM) makes decisions, actuator nodes execute.

```go
// Sensor node
func sensorLoop(ctx context.Context, bus messagebus.PubSub, sensor device.Sensor) {
    for {
        reading, _ := sensor.Read(ctx)
        data, _ := json.Marshal(reading)
        bus.Publish(ctx, "sensors/temperature", data, messagebus.WithQoS(1))
        time.Sleep(5 * time.Second)
    }
}

// Processor node (has the LLM)
func processorLoop(ctx context.Context, bus messagebus.PubSub, model model.ChatModel) {
    ch, _ := bus.Subscribe(ctx, "sensors/temperature")
    for msg := range ch {
        // Use LLM to decide action
        decision := analyze(ctx, model, msg.Payload)
        if decision.Action != "" {
            data, _ := json.Marshal(decision)
            bus.Publish(ctx, "commands/actuator", data, messagebus.WithQoS(2))
        }
    }
}

// Actuator node
func actuatorLoop(ctx context.Context, bus messagebus.PubSub, device device.Connector) {
    ch, _ := bus.Subscribe(ctx, "commands/actuator")
    for msg := range ch {
        var cmd Command
        json.Unmarshal(msg.Payload, &cmd)
        device.Command(ctx, []byte(cmd.Action))
    }
}
```

### 2. Task Distribution

A coordinator distributes work across multiple identical workers:

```go
// Coordinator publishes tasks
bus.Publish(ctx, "coordination/tasks", taskData, messagebus.WithQoS(1))

// Workers subscribe with shared subscription (MQTT 5.0) or use unique topics
ch, _ := bus.Subscribe(ctx, "coordination/tasks/worker-"+myID)
```

### 3. Consensus / Voting

Multiple agents vote on a decision:

```go
// Each agent publishes its vote
bus.Publish(ctx, "coordination/votes/"+proposalID, voteData)

// Coordinator collects votes
ch, _ := bus.Subscribe(ctx, "coordination/votes/+")  // MQTT wildcard
```

## Health Monitoring

Each device publishes heartbeats:

```go
func heartbeat(ctx context.Context, bus messagebus.PubSub, deviceID string) {
    ticker := time.NewTicker(10 * time.Second)
    for range ticker.C {
        status := map[string]any{
            "device":  deviceID,
            "uptime":  time.Since(startTime).Seconds(),
            "model":   cam.ActiveModel(),
            "sensors": sm.FormatReadings(),
        }
        data, _ := json.Marshal(status)
        bus.Publish(ctx, "status/"+deviceID, data,
            messagebus.WithQoS(0),
            messagebus.WithRetain(true),  // New subscribers get last status
        )
    }
}
```

## Security Considerations

- Use MQTT over TLS (`tls://broker:8883`) for production
- Authenticate with username/password or client certificates
- Use topic ACLs on the broker to restrict publish/subscribe permissions
- Never put API keys or secrets in MQTT messages
- Consider message signing for actuator commands (prevent injection)

## Example: edge_fleet

See `examples/edge_fleet/` for a working demonstration of 3 agents coordinating via PubSub.

```bash
go run ./examples/edge_fleet/
```
