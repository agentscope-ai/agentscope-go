package device

import (
	"context"
	"fmt"
	"testing"
)

func TestConnectorState_String(t *testing.T) {
	tests := []struct {
		state ConnectorState
		want  string
	}{
		{StateDisconnected, "disconnected"},
		{StateConnected, "connected"},
		{StateError, "error"},
		{ConnectorState(99), "unknown"},
	}

	for _, tc := range tests {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("ConnectorState(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

func TestSensorReading_Fields(t *testing.T) {
	r := &SensorReading{
		Name:  "pressure",
		Value: 1013.25,
		Unit:  "hPa",
		Raw:   []byte{0x01, 0x02},
	}

	if r.Name != "pressure" {
		t.Errorf("unexpected Name: %s", r.Name)
	}
	if r.Value != 1013.25 {
		t.Errorf("unexpected Value: %f", r.Value)
	}
	if r.Unit != "hPa" {
		t.Errorf("unexpected Unit: %s", r.Unit)
	}
	if len(r.Raw) != 2 {
		t.Errorf("unexpected Raw length: %d", len(r.Raw))
	}
}

func TestDeviceInfo_Fields(t *testing.T) {
	info := DeviceInfo{
		ID:    "sensor-001",
		Type:  "i2c",
		Path:  "/dev/i2c-1",
		State: StateConnected,
		Metadata: map[string]string{
			"addr": "0x48",
		},
	}

	if info.ID != "sensor-001" {
		t.Errorf("unexpected ID: %s", info.ID)
	}
	if info.Type != "i2c" {
		t.Errorf("unexpected Type: %s", info.Type)
	}
	if info.State != StateConnected {
		t.Errorf("unexpected State: %v", info.State)
	}
	if info.Metadata["addr"] != "0x48" {
		t.Errorf("unexpected metadata: %v", info.Metadata)
	}
}

// mockConnector verifies the Connector interface contract.
type mockConnector struct {
	opened    bool
	openErr   error
	closeErr  error
	cmdResult []byte
	cmdErr    error
}

func (c *mockConnector) Open() error {
	if c.openErr != nil {
		return c.openErr
	}
	c.opened = true
	return nil
}

func (c *mockConnector) Close() error {
	c.opened = false
	return c.closeErr
}

func (c *mockConnector) Command(_ context.Context, _ []byte) ([]byte, error) {
	if !c.opened {
		return nil, fmt.Errorf("not open")
	}
	return c.cmdResult, c.cmdErr
}

func TestConnector_Interface(t *testing.T) {
	// Verify mock satisfies Connector.
	var _ Connector = (*mockConnector)(nil)

	c := &mockConnector{cmdResult: []byte("OK")}

	// Open
	if err := c.Open(); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if !c.opened {
		t.Error("expected opened=true after Open()")
	}

	// Command
	ctx := context.Background()
	resp, err := c.Command(ctx, []byte("test"))
	if err != nil {
		t.Fatalf("Command failed: %v", err)
	}
	if string(resp) != "OK" {
		t.Errorf("unexpected response: %s", resp)
	}

	// Close
	if err := c.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if c.opened {
		t.Error("expected opened=false after Close()")
	}

	// Command when closed should fail
	_, err = c.Command(ctx, []byte("test"))
	if err == nil {
		t.Error("expected error when calling Command on closed connector")
	}
}

func TestConnector_OpenError(t *testing.T) {
	c := &mockConnector{openErr: fmt.Errorf("device busy")}
	err := c.Open()
	if err == nil {
		t.Error("expected open error")
	}
	if !c.opened == false {
		t.Error("should not be opened on error")
	}
}

// mockSensorImpl verifies the Sensor interface contract.
type mockSensorImpl struct {
	mockConnector
	reading *SensorReading
}

func (s *mockSensorImpl) Read(_ context.Context) (*SensorReading, error) {
	if !s.opened {
		return nil, fmt.Errorf("not open")
	}
	return s.reading, nil
}

func TestSensor_Interface(t *testing.T) {
	var _ Sensor = (*mockSensorImpl)(nil)

	s := &mockSensorImpl{
		mockConnector: mockConnector{cmdResult: []byte("raw")},
		reading:       &SensorReading{Name: "temp", Value: 22.5, Unit: "°C"},
	}

	if err := s.Open(); err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	ctx := context.Background()
	reading, err := s.Read(ctx)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if reading.Value != 22.5 {
		t.Errorf("expected 22.5, got %f", reading.Value)
	}
}

// mockActuatorImpl verifies the Actuator interface contract.
type mockActuatorImpl struct {
	mockConnector
	safed bool
}

func (a *mockActuatorImpl) Act(_ context.Context, params map[string]any) error {
	if !a.opened {
		return fmt.Errorf("not open")
	}
	return nil
}

func (a *mockActuatorImpl) SafeState(_ context.Context) error {
	a.safed = true
	return nil
}

func TestActuator_Interface(t *testing.T) {
	var _ Actuator = (*mockActuatorImpl)(nil)

	a := &mockActuatorImpl{
		mockConnector: mockConnector{cmdResult: []byte("done")},
	}

	if err := a.Open(); err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	ctx := context.Background()
	if err := a.Act(ctx, map[string]any{"speed": 100}); err != nil {
		t.Fatalf("Act failed: %v", err)
	}

	if err := a.SafeState(ctx); err != nil {
		t.Fatalf("SafeState failed: %v", err)
	}
	if !a.safed {
		t.Error("SafeState should have been called")
	}
}
