package device

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// DeviceTool wraps a Connector as a tool.Tool for use in agent loops.
// Sensor tools (read-only) are auto-allowed; actuator tools require
// explicit permission (bypass-immune ASK).
type DeviceTool struct {
	tool.BaseTool
	connector Connector
	isSensor  bool
	timeout   time.Duration
	watchdog  *Watchdog
}

// DeviceToolOption configures a DeviceTool.
type DeviceToolOption func(*DeviceTool)

// WithDeviceTimeout sets the command execution timeout.
func WithDeviceTimeout(d time.Duration) DeviceToolOption {
	return func(dt *DeviceTool) {
		if d > 0 {
			dt.timeout = d
		}
	}
}

// WithWatchdog attaches a watchdog to the device tool.
// The watchdog is kicked on each successful command execution.
func WithWatchdog(wd *Watchdog) DeviceToolOption {
	return func(dt *DeviceTool) { dt.watchdog = wd }
}

// NewDeviceTool creates a tool wrapping a hardware connector.
// If isSensor is true, the tool is marked read-only and auto-allowed.
func NewDeviceTool(name, description string, connector Connector, isSensor bool, opts ...DeviceToolOption) *DeviceTool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {
				"type": "string",
				"description": "The command to send to the device (hex-encoded or text)"
			}
		},
		"required": ["command"]
	}`)

	dt := &DeviceTool{
		BaseTool: tool.BaseTool{
			ToolName:        name,
			ToolDescription: description,
			ToolSchema:      schema,
			ConcurrencySafe: false, // Hardware is not concurrency safe
			ReadOnly:        isSensor,
		},
		connector: connector,
		isSensor:  isSensor,
		timeout:   5 * time.Second,
	}

	for _, opt := range opts {
		opt(dt)
	}

	return dt
}

// Execute sends a command to the device and returns the response.
func (dt *DeviceTool) Execute(ctx context.Context, input map[string]any) (*tool.ToolResponse, error) {
	cmdStr, ok := input["command"].(string)
	if !ok {
		return tool.NewErrorResponse(fmt.Errorf("device: 'command' must be a string")), nil
	}

	// Apply timeout.
	execCtx, cancel := context.WithTimeout(ctx, dt.timeout)
	defer cancel()

	resp, err := dt.connector.Command(execCtx, []byte(cmdStr))
	if err != nil {
		return tool.NewErrorResponse(fmt.Errorf("device %s: %w", dt.ToolName, err)), nil
	}

	// Kick the watchdog on successful command.
	if dt.watchdog != nil {
		dt.watchdog.Kick()
	}

	return tool.NewTextResponse(string(resp)), nil
}

// CheckPermissions enforces bypass-immune ASK for actuator tools.
// Sensor (read-only) tools pass through normally.
func (dt *DeviceTool) CheckPermissions(input map[string]any, ctx *permission.Context) permission.Decision {
	if dt.isSensor {
		// Sensors are auto-allowed — they cannot modify the physical world.
		return permission.Decision{Behavior: permission.BehaviorAllow}
	}
	// Actuators require explicit permission (bypass-immune).
	return permission.Decision{Behavior: permission.BehaviorAsk}
}

// IsExternalTool returns true — device tools interact with the physical world.
func (dt *DeviceTool) IsExternalTool() bool {
	return true
}

// NewSensorTool is a convenience constructor for read-only sensor tools.
func NewSensorTool(name, description string, sensor Sensor, opts ...DeviceToolOption) *SensorTool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {},
		"additionalProperties": false
	}`)

	st := &SensorTool{
		BaseTool: tool.BaseTool{
			ToolName:        name,
			ToolDescription: description,
			ToolSchema:      schema,
			ConcurrencySafe: true, // Sensors are typically safe for concurrent reads
			ReadOnly:        true,
		},
		sensor:  sensor,
		timeout: 5 * time.Second,
	}

	for _, opt := range opts {
		opt2 := DeviceToolOption(opt)
		_ = opt2 // apply via direct field set below
	}
	return st
}

// SensorTool is a specialized device tool for read-only sensors.
type SensorTool struct {
	tool.BaseTool
	sensor  Sensor
	timeout time.Duration
}

// Execute reads the sensor value.
func (st *SensorTool) Execute(ctx context.Context, input map[string]any) (*tool.ToolResponse, error) {
	execCtx, cancel := context.WithTimeout(ctx, st.timeout)
	defer cancel()

	reading, err := st.sensor.Read(execCtx)
	if err != nil {
		return tool.NewErrorResponse(fmt.Errorf("sensor %s: %w", st.ToolName, err)), nil
	}

	data, _ := json.Marshal(reading)
	return tool.NewTextResponse(string(data)), nil
}

// CheckPermissions auto-allows sensor reads.
func (st *SensorTool) CheckPermissions(_ map[string]any, _ *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorAllow}
}

// IsExternalTool returns true.
func (st *SensorTool) IsExternalTool() bool { return true }

// Compile-time interface checks.
var (
	_ tool.Tool = (*DeviceTool)(nil)
	_ tool.Tool = (*SensorTool)(nil)
)
