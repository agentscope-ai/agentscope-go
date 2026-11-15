package device

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

// testConnector is a mock Connector for testing.
type testConnector struct {
	opened    bool
	responses map[string]string
	calls     int
}

func newTestConnector() *testConnector {
	return &testConnector{
		responses: map[string]string{
			"STATUS": "OK X=0 Y=0 Z=0",
			"HOME":   "OK HOMED",
			"PING":   "PONG",
		},
	}
}

func (c *testConnector) Open() error {
	if c.opened {
		return fmt.Errorf("already open")
	}
	c.opened = true
	return nil
}

func (c *testConnector) Close() error {
	c.opened = false
	return nil
}

func (c *testConnector) Command(_ context.Context, cmd []byte) ([]byte, error) {
	if !c.opened {
		return nil, fmt.Errorf("not open")
	}
	c.calls++
	resp, ok := c.responses[string(cmd)]
	if !ok {
		return []byte("ERR UNKNOWN"), nil
	}
	return []byte(resp), nil
}

// testSensor is a mock Sensor for testing.
type testSensor struct {
	testConnector
	readValue float64
}

func newTestSensor(value float64) *testSensor {
	return &testSensor{
		testConnector: testConnector{
			responses: map[string]string{"READ": "OK"},
		},
		readValue: value,
	}
}

func (s *testSensor) Read(_ context.Context) (*SensorReading, error) {
	return &SensorReading{
		Name:  "temperature",
		Value: s.readValue,
		Unit:  "°C",
	}, nil
}

func TestDeviceTool_Execute(t *testing.T) {
	conn := newTestConnector()
	conn.opened = true

	dt := NewDeviceTool("test_device", "A test device", conn, false,
		WithDeviceTimeout(2*time.Second),
	)

	ctx := context.Background()
	input := map[string]any{"command": "STATUS"}

	resp, err := dt.Execute(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Errorf("expected success state, got %v", resp.State)
	}
	if len(resp.Content) == 0 {
		t.Fatal("expected content in response")
	}
	// Check text content
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", resp.Content[0])
	}
	if tb.Text != "OK X=0 Y=0 Z=0" {
		t.Errorf("unexpected response text: %q", tb.Text)
	}
}

func TestDeviceTool_Execute_InvalidInput(t *testing.T) {
	conn := newTestConnector()
	conn.opened = true

	dt := NewDeviceTool("test_device", "A test device", conn, false)

	ctx := context.Background()

	// Missing command field
	resp, err := dt.Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Errorf("expected error state for missing command, got %v", resp.State)
	}

	// Wrong type for command field
	resp, err = dt.Execute(ctx, map[string]any{"command": 123})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Errorf("expected error state for wrong type, got %v", resp.State)
	}
}

func TestDeviceTool_Execute_ConnectorNotOpen(t *testing.T) {
	conn := newTestConnector() // not opened

	dt := NewDeviceTool("test_device", "A test device", conn, false)

	ctx := context.Background()
	resp, err := dt.Execute(ctx, map[string]any{"command": "STATUS"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultError {
		t.Errorf("expected error state when connector not open, got %v", resp.State)
	}
}

func TestDeviceTool_Execute_Timeout(t *testing.T) {
	// Create a connector that blocks
	slowConn := &slowConnector{delay: 200 * time.Millisecond}
	slowConn.opened = true

	dt := NewDeviceTool("slow_device", "A slow device", slowConn, false,
		WithDeviceTimeout(50*time.Millisecond),
	)

	ctx := context.Background()
	resp, err := dt.Execute(ctx, map[string]any{"command": "SLOW"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should get an error response due to timeout
	if resp.State != message.ToolResultError {
		t.Errorf("expected error state on timeout, got %v", resp.State)
	}
}

type slowConnector struct {
	testConnector
	delay time.Duration
}

func (c *slowConnector) Command(ctx context.Context, cmd []byte) ([]byte, error) {
	select {
	case <-time.After(c.delay):
		return []byte("DONE"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestDeviceTool_Watchdog_Kicked(t *testing.T) {
	var triggered atomic.Int32
	wd := NewWatchdog(100*time.Millisecond, func() {
		triggered.Add(1)
	})
	wd.Start()
	defer wd.Stop()

	conn := newTestConnector()
	conn.opened = true

	dt := NewDeviceTool("wd_device", "Device with watchdog", conn, false,
		WithWatchdog(wd),
	)

	ctx := context.Background()

	// Execute a command — this should kick the watchdog
	_, _ = dt.Execute(ctx, map[string]any{"command": "STATUS"})

	// Wait less than watchdog timeout
	time.Sleep(50 * time.Millisecond)

	if triggered.Load() != 0 {
		t.Error("watchdog should not have triggered after successful command")
	}
}

func TestDeviceTool_Permissions_Actuator(t *testing.T) {
	conn := newTestConnector()
	dt := NewDeviceTool("actuator", "An actuator", conn, false)

	decision := dt.CheckPermissions(map[string]any{"command": "MOVE"}, &permission.Context{})
	if decision.Behavior != permission.BehaviorAsk {
		t.Errorf("actuator should require ASK permission, got %v", decision.Behavior)
	}
}

func TestDeviceTool_Permissions_Sensor(t *testing.T) {
	conn := newTestConnector()
	dt := NewDeviceTool("sensor", "A sensor", conn, true) // isSensor=true

	decision := dt.CheckPermissions(map[string]any{"command": "READ"}, &permission.Context{})
	if decision.Behavior != permission.BehaviorAllow {
		t.Errorf("sensor should auto-allow, got %v", decision.Behavior)
	}
}

func TestDeviceTool_IsReadOnly(t *testing.T) {
	conn := newTestConnector()

	// Sensor (read-only)
	sensorTool := NewDeviceTool("sensor", "A sensor", conn, true)
	if !sensorTool.IsReadOnly() {
		t.Error("sensor tool should be read-only")
	}

	// Actuator (not read-only)
	actuatorTool := NewDeviceTool("actuator", "An actuator", conn, false)
	if actuatorTool.IsReadOnly() {
		t.Error("actuator tool should not be read-only")
	}
}

func TestDeviceTool_IsExternalTool(t *testing.T) {
	conn := newTestConnector()
	dt := NewDeviceTool("device", "A device", conn, false)
	if !dt.IsExternalTool() {
		t.Error("device tool should be external")
	}
}

func TestSensorTool_Execute(t *testing.T) {
	sensor := newTestSensor(24.5)
	sensor.opened = true

	st := NewSensorTool("temp_sensor", "Temperature sensor", sensor)

	ctx := context.Background()
	resp, err := st.Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != message.ToolResultSuccess {
		t.Errorf("expected success, got %v", resp.State)
	}

	// Parse the JSON response
	tb, ok := resp.Content[0].(message.TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", resp.Content[0])
	}

	var reading SensorReading
	if err := json.Unmarshal([]byte(tb.Text), &reading); err != nil {
		t.Fatalf("failed to unmarshal sensor reading: %v", err)
	}
	if reading.Value != 24.5 {
		t.Errorf("expected 24.5, got %f", reading.Value)
	}
	if reading.Unit != "°C" {
		t.Errorf("expected °C, got %s", reading.Unit)
	}
}

func TestSensorTool_Permissions(t *testing.T) {
	sensor := newTestSensor(20.0)
	st := NewSensorTool("temp", "Temp", sensor)

	decision := st.CheckPermissions(nil, nil)
	if decision.Behavior != permission.BehaviorAllow {
		t.Errorf("sensor tool should auto-allow, got %v", decision.Behavior)
	}
}

func TestSensorTool_IsExternalTool(t *testing.T) {
	sensor := newTestSensor(20.0)
	st := NewSensorTool("temp", "Temp", sensor)
	if !st.IsExternalTool() {
		t.Error("sensor tool should be external")
	}
}
