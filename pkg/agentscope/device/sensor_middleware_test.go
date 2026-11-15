package device

import (
	"context"
	"strings"
	"testing"
	"time"
)

// mockSensorForMiddleware implements Sensor for middleware tests.
type mockSensorForMiddleware struct {
	name  string
	value float64
	unit  string
}

func (s *mockSensorForMiddleware) Open() error  { return nil }
func (s *mockSensorForMiddleware) Close() error { return nil }
func (s *mockSensorForMiddleware) Command(_ context.Context, _ []byte) ([]byte, error) {
	return nil, nil
}
func (s *mockSensorForMiddleware) Read(_ context.Context) (*SensorReading, error) {
	return &SensorReading{
		Name:  s.name,
		Value: s.value,
		Unit:  s.unit,
	}, nil
}

func TestSensorMiddleware_EnrichPrompt_Empty(t *testing.T) {
	sm := NewSensorMiddleware()
	base := "You are a helpful assistant."
	result := sm.EnrichPrompt(context.Background(), base)
	if result != base {
		t.Errorf("expected base prompt unchanged when no sensors, got %q", result)
	}
}

func TestSensorMiddleware_RegisterAndPoll(t *testing.T) {
	sm := NewSensorMiddleware(
		WithMaxTokensBudget(500),
		WithPollTimeout(1*time.Second),
	)

	tempSensor := &mockSensorForMiddleware{name: "temperature", value: 25.5, unit: "°C"}
	humSensor := &mockSensorForMiddleware{name: "humidity", value: 60.0, unit: "%"}

	sm.Register("temp", tempSensor)
	sm.Register("hum", humSensor)

	ctx := context.Background()
	sm.Poll(ctx)

	readings := sm.Readings()
	if len(readings) != 2 {
		t.Fatalf("expected 2 readings, got %d", len(readings))
	}

	if readings["temp"].Value != 25.5 {
		t.Errorf("expected temp 25.5, got %f", readings["temp"].Value)
	}
	if readings["hum"].Value != 60.0 {
		t.Errorf("expected humidity 60.0, got %f", readings["hum"].Value)
	}
}

func TestSensorMiddleware_EnrichPrompt_WithData(t *testing.T) {
	sm := NewSensorMiddleware(WithMaxTokensBudget(500))

	tempSensor := &mockSensorForMiddleware{name: "temperature", value: 22.0, unit: "°C"}
	sm.Register("temp", tempSensor)

	ctx := context.Background()
	sm.Poll(ctx)

	base := "You are an environmental monitor."
	enriched := sm.EnrichPrompt(ctx, base)

	// Should contain SENSOR DATA markers
	if !strings.Contains(enriched, "[SENSOR DATA]") {
		t.Error("enriched prompt should contain [SENSOR DATA] marker")
	}
	if !strings.Contains(enriched, "[/SENSOR DATA]") {
		t.Error("enriched prompt should contain [/SENSOR DATA] marker")
	}

	// Should contain the sensor value
	if !strings.Contains(enriched, "22") {
		t.Error("enriched prompt should contain sensor value")
	}

	// Should end with the base prompt
	if !strings.HasSuffix(enriched, base) {
		t.Error("enriched prompt should end with base prompt")
	}
}

func TestSensorMiddleware_MaxTokensBudget(t *testing.T) {
	// Very small budget
	sm := NewSensorMiddleware(WithMaxTokensBudget(10)) // 10 tokens = ~40 chars

	tempSensor := &mockSensorForMiddleware{name: "temperature_long_name", value: 25.5, unit: "degrees_celsius"}
	sm.Register("sensor_with_a_very_long_identifier", tempSensor)

	ctx := context.Background()
	sm.Poll(ctx)

	base := "Base prompt."
	enriched := sm.EnrichPrompt(ctx, base)

	// The sensor block should be truncated
	sensorPart := strings.TrimSuffix(enriched, base)
	// 10 tokens * 4 chars = 40 chars max for sensor block (approximate)
	// Allow some slack for the closing tag
	if len(sensorPart) > 100 { // generous check
		t.Errorf("sensor block should be truncated, got %d chars", len(sensorPart))
	}
}

func TestSensorMiddleware_Unregister(t *testing.T) {
	sm := NewSensorMiddleware()

	tempSensor := &mockSensorForMiddleware{name: "temperature", value: 25.0, unit: "°C"}
	sm.Register("temp", tempSensor)

	ctx := context.Background()
	sm.Poll(ctx)

	readings := sm.Readings()
	if len(readings) != 1 {
		t.Fatalf("expected 1 reading, got %d", len(readings))
	}

	sm.Unregister("temp")

	readings = sm.Readings()
	if len(readings) != 0 {
		t.Errorf("expected 0 readings after unregister, got %d", len(readings))
	}
}

func TestSensorMiddleware_FormatReadings_Empty(t *testing.T) {
	sm := NewSensorMiddleware()
	result := sm.FormatReadings()
	if result != "No sensor data available." {
		t.Errorf("unexpected format for empty readings: %q", result)
	}
}

func TestSensorMiddleware_FormatReadings_WithData(t *testing.T) {
	sm := NewSensorMiddleware()
	tempSensor := &mockSensorForMiddleware{name: "temperature", value: 23.5, unit: "°C"}
	sm.Register("temp", tempSensor)

	ctx := context.Background()
	sm.Poll(ctx)

	result := sm.FormatReadings()
	if !strings.Contains(result, "23.50") {
		t.Errorf("format should contain value, got %q", result)
	}
	if !strings.Contains(result, "°C") {
		t.Errorf("format should contain unit, got %q", result)
	}
}

func TestSensorMiddleware_PollTimeout(t *testing.T) {
	// Sensor that takes too long
	slowSensor := &slowSensorMock{delay: 200 * time.Millisecond}
	sm := NewSensorMiddleware(WithPollTimeout(50 * time.Millisecond))
	sm.Register("slow", slowSensor)

	ctx := context.Background()
	sm.Poll(ctx) // Should not hang — poll timeout is 50ms

	// Reading might not be available since sensor timed out
	readings := sm.Readings()
	// Either empty or has a stale value — both are acceptable
	_ = readings
}

type slowSensorMock struct {
	delay time.Duration
}

func (s *slowSensorMock) Open() error  { return nil }
func (s *slowSensorMock) Close() error { return nil }
func (s *slowSensorMock) Command(ctx context.Context, _ []byte) ([]byte, error) {
	select {
	case <-time.After(s.delay):
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *slowSensorMock) Read(ctx context.Context) (*SensorReading, error) {
	select {
	case <-time.After(s.delay):
		return &SensorReading{Name: "slow", Value: 1.0, Unit: "x"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestSensorReading_String(t *testing.T) {
	r := &SensorReading{Name: "temperature", Value: 24.5, Unit: "°C"}
	s := r.String()
	if s != "temperature: 24.50 °C" {
		t.Errorf("unexpected String(): %q", s)
	}
}
