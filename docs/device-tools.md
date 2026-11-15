# Device Tools

The `device` package provides hardware connectivity for AI agents running on edge/embedded systems. All drivers are pure Go (no CGO) using `golang.org/x/sys/unix` syscalls.

## Architecture

```
Agent Loop
    │
    ├── DeviceTool (actuator, requires permission)
    │       └── Connector (Serial/GPIO/CAN/I2C)
    │
    ├── SensorTool (read-only, auto-allowed)
    │       └── Sensor (read-only Connector)
    │
    ├── Watchdog (safety timer)
    │
    └── SensorMiddleware (injects readings into system prompt)
```

## Connector Interface

All hardware communication goes through a unified interface:

```go
type Connector interface {
    Open() error
    Close() error
    Command(ctx context.Context, cmd []byte) ([]byte, error)
}
```

### Available Connectors

| Connector | Build Tag | Platform | Use Case |
|-----------|-----------|----------|----------|
| `SerialConnector` | `linux` | Linux | UART devices, robot arms, PLCs |
| `GPIOConnector` | `linux` | Linux | Digital I/O, relays, LEDs |
| `CANConnector` | `linux` | Linux | Automotive, industrial bus |
| `I2CConnector` | `linux` | Linux | Sensors, displays, ADCs |

## Serial Port

```go
import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/device"

serial := device.NewSerialConnector("/dev/ttyUSB0",
    device.WithBaudRate(115200),
    device.WithDataBits(8),
    device.WithStopBits(1),
    device.WithParity('N'),
    device.WithTimeout(2*time.Second),
)

if err := serial.Open(); err != nil {
    log.Fatal(err)
}
defer serial.Close()

resp, err := serial.Command(ctx, []byte("STATUS\n"))
```

## GPIO

```go
// Read a button (input)
button := device.NewGPIOConnector("/dev/gpiochip0", 17)
button.Open()
val, _ := button.Command(ctx, []byte("R"))  // Returns [0] or [1]

// Control an LED (output)
led := device.NewGPIOConnector("/dev/gpiochip0", 18, device.WithGPIOOutput())
led.Open()
led.Command(ctx, []byte{'W', 1})  // Turn on
led.Command(ctx, []byte{'W', 0})  // Turn off
```

## CAN Bus

```go
can := device.NewCANConnector("can0",
    device.WithCANTimeout(500*time.Millisecond),
)
can.Open()
defer can.Close()

// Send frame: [4-byte ID (LE)][data...]
cmd := make([]byte, 6)
binary.LittleEndian.PutUint32(cmd[:4], 0x123)
cmd[4] = 0x01 // data byte 1
cmd[5] = 0xFF // data byte 2

resp, err := can.Command(ctx, cmd)
```

## I2C

```go
sensor := device.NewI2CConnector("/dev/i2c-1", 0x48)  // TMP102 at address 0x48
sensor.Open()
defer sensor.Close()

// Read temperature register (0x00)
data, err := sensor.Command(ctx, []byte{0x00})
temp := float64(int16(data[0])<<4|int16(data[1])>>4) * 0.0625
```

## DeviceTool (Agent Integration)

Wrap any Connector as a tool the agent can call:

```go
robotTool := device.NewDeviceTool(
    "robot_arm",
    "Control the robot arm. Commands: HOME, MOVE X<n> Y<n> Z<n>, GRIP, RELEASE",
    serialConnector,
    false,  // not a sensor (actuator)
    device.WithDeviceTimeout(5*time.Second),
    device.WithWatchdog(wd),
)

tk := tool.NewToolkit(robotTool)
```

### Permission Model

- **Sensors** (read-only): Auto-allowed. Cannot modify the physical world.
- **Actuators** (read-write): Require explicit permission (bypass-immune ASK). The agent must get human approval before commanding physical devices.

## Watchdog

Safety timer that triggers a safe-state function if the agent loop stops responding:

```go
wd := device.NewWatchdog(5*time.Second, func() {
    // Called if Kick() is not called within 5s
    motor.Command(ctx, []byte("STOP"))
    valve.Command(ctx, []byte("CLOSE"))
})

wd.Start()
defer wd.Stop()

// In the agent loop:
for {
    // ... do work ...
    wd.Kick()  // Signal that the agent is still alive
}
```

## SensorMiddleware

Injects sensor readings into the system prompt so the agent has environmental awareness.

`device.Sensor` is `Connector` plus `Read(ctx) (*SensorReading, error)`, and the
library ships no concrete implementation. `NewI2CConnector`, `NewSerialConnector`,
`NewGPIOConnector` and `NewCANConnector` all return plain `Connector` values,
which cannot be passed to `Register`. Wrap one:

```go
type tmp102 struct{ device.Connector }

func (s tmp102) Read(ctx context.Context) (*device.SensorReading, error) {
    data, err := s.Command(ctx, []byte{0x00}) // TMP102 temperature register
    if err != nil {
        return nil, err
    }
    celsius := float64(int16(data[0])<<4|int16(data[1])>>4) * 0.0625
    return &device.SensorReading{Name: "temp", Value: celsius, Unit: "°C"}, nil
}

sm.Register("temperature", tmp102{device.NewI2CConnector("/dev/i2c-1", 0x48)})
```

`examples/edge_sensor` does this with a mock. `device.NewSensorTool(name,
description, sensor, opts...)` wraps the same `Sensor` as an auto-allowed
read-only tool. The concrete connectors are `//go:build linux` (see the table at
the top of this page), so this code compiles on Linux only; build it with
`GOOS=linux` elsewhere.

```go
sm := device.NewSensorMiddleware(
    device.WithMaxTokensBudget(200),  // Limit context bloat
    device.WithPollTimeout(2*time.Second),
)
sm.Register("temperature", tempSensor)
sm.Register("humidity", humiditySensor)

// Before each model call:
sm.Poll(ctx)
enrichedPrompt := sm.EnrichPrompt(ctx, basePrompt)
```

The enriched prompt looks like:

```
[SENSOR DATA]
[{"sensor":"temperature","name":"temp","value":24.5,"unit":"°C"},...]
[/SENSOR DATA]

You are an environmental monitoring agent...
```
