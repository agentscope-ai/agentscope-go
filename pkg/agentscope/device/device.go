// Package device provides hardware device connectivity for edge/embedded AI
// agents. It defines a Connector interface for hardware communication (serial,
// GPIO, CAN, I2C) and tools that wrap connectors for safe use by AI agents.
//
// All device drivers use pure Go (no CGO) via golang.org/x/sys/unix syscalls,
// making them cross-compilable to any Linux target (arm64, arm, riscv64, mips).
package device

import (
	"context"
	"fmt"
)

// Connector is the interface for hardware device communication.
// Implementations handle the low-level protocol (serial UART, GPIO, CAN, I2C).
type Connector interface {
	// Open initializes the hardware connection.
	Open() error

	// Close releases the hardware resources.
	Close() error

	// Command sends a command and returns the response.
	// The context allows timeout/cancellation of hardware operations.
	Command(ctx context.Context, cmd []byte) ([]byte, error)
}

// ConnectorState represents the connection state of a device.
type ConnectorState int

const (
	// StateDisconnected means the device is not connected.
	StateDisconnected ConnectorState = iota
	// StateConnected means the device is open and ready.
	StateConnected
	// StateError means the device encountered an error.
	StateError
)

// String returns a human-readable state name.
func (s ConnectorState) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnected:
		return "connected"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// Sensor is a read-only device that provides measurements.
// Sensor tools are auto-allowed (no permission prompt) since they cannot
// modify the physical world.
type Sensor interface {
	Connector

	// Read returns the current sensor reading as structured data.
	Read(ctx context.Context) (*SensorReading, error)
}

// SensorReading contains a sensor measurement.
type SensorReading struct {
	// Name is the sensor identifier (e.g. "temperature", "humidity").
	Name string `json:"name"`

	// Value is the numeric measurement.
	Value float64 `json:"value"`

	// Unit is the measurement unit (e.g. "°C", "%", "lux").
	Unit string `json:"unit"`

	// Raw is the raw bytes from the sensor, if available.
	Raw []byte `json:"raw,omitempty"`
}

// String formats the reading for display.
func (r *SensorReading) String() string {
	return fmt.Sprintf("%s: %.2f %s", r.Name, r.Value, r.Unit)
}

// Actuator is a device that can modify the physical world.
// Actuator tools require explicit permission (bypass-immune ASK).
type Actuator interface {
	Connector

	// Act performs a physical action. The command map contains action parameters.
	Act(ctx context.Context, params map[string]any) error

	// SafeState puts the actuator into a known-safe state (e.g. motor off,
	// valve closed). Called by the Watchdog on timeout.
	SafeState(ctx context.Context) error
}

// DeviceInfo describes a connected device for discovery and status reporting.
type DeviceInfo struct {
	// ID is a unique identifier for the device.
	ID string `json:"id"`

	// Type is the device category (e.g. "serial", "gpio", "can", "i2c").
	Type string `json:"type"`

	// Path is the OS device path (e.g. "/dev/ttyUSB0", "/dev/gpiochip0").
	Path string `json:"path"`

	// State is the current connection state.
	State ConnectorState `json:"state"`

	// Metadata holds device-specific extra information.
	Metadata map[string]string `json:"metadata,omitempty"`
}
