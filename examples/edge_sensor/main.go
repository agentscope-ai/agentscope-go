// Example edge_sensor demonstrates the SensorMiddleware and DeviceTool for
// an edge AI agent that monitors physical sensors.
//
// This example uses a mock sensor for portability — on real hardware, replace
// MockSensor with a real I2C/Serial sensor connector.
//
// Run:
//
//	go run ./examples/edge_sensor/
package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/device"
)

// MockSensor simulates a temperature sensor for demonstration.
type MockSensor struct {
	name   string
	opened bool
}

func (s *MockSensor) Open() error {
	s.opened = true
	return nil
}

func (s *MockSensor) Close() error {
	s.opened = false
	return nil
}

func (s *MockSensor) Command(_ context.Context, cmd []byte) ([]byte, error) {
	reading, _ := s.Read(context.Background())
	return []byte(reading.String()), nil
}

func (s *MockSensor) Read(_ context.Context) (*device.SensorReading, error) {
	// Simulate temperature between 20-30°C with some noise.
	temp := 25.0 + (rand.Float64()-0.5)*10
	return &device.SensorReading{
		Name:  s.name,
		Value: temp,
		Unit:  "°C",
	}, nil
}

func main() {
	// Create sensors.
	tempSensor := &MockSensor{name: "temperature"}
	humiditySensor := &MockHumiditySensor{name: "humidity"}

	// Open sensors.
	if err := tempSensor.Open(); err != nil {
		fmt.Printf("Failed to open temp sensor: %v\n", err)
		return
	}
	defer tempSensor.Close()

	if err := humiditySensor.Open(); err != nil {
		fmt.Printf("Failed to open humidity sensor: %v\n", err)
		return
	}
	defer humiditySensor.Close()

	// Create sensor middleware.
	sm := device.NewSensorMiddleware(
		device.WithMaxTokensBudget(200),
		device.WithPollTimeout(2*time.Second),
	)
	sm.Register("temperature", tempSensor)
	sm.Register("humidity", humiditySensor)

	// Create a watchdog (5-second timeout).
	wd := device.NewWatchdog(5*time.Second, func() {
		fmt.Println("[WATCHDOG] Safe state triggered! Shutting down sensors.")
	})
	wd.Start()
	defer wd.Stop()

	// Simulate agent loop: poll sensors and show enriched prompt.
	ctx := context.Background()
	basePrompt := "You are an environmental monitoring agent on an edge device."

	for i := 0; i < 3; i++ {
		// Poll sensors.
		sm.Poll(ctx)

		// Show enriched prompt.
		enriched := sm.EnrichPrompt(ctx, basePrompt)
		fmt.Printf("--- Iteration %d ---\n", i+1)
		fmt.Printf("Enriched prompt:\n%s\n", enriched)
		fmt.Printf("Readings:\n%s\n", sm.FormatReadings())

		// Kick the watchdog (agent is alive).
		wd.Kick()

		time.Sleep(1 * time.Second)
	}

	fmt.Println("Edge sensor agent demo complete.")
}

// MockHumiditySensor simulates a humidity sensor.
type MockHumiditySensor struct {
	name   string
	opened bool
}

func (s *MockHumiditySensor) Open() error {
	s.opened = true
	return nil
}

func (s *MockHumiditySensor) Close() error {
	s.opened = false
	return nil
}

func (s *MockHumiditySensor) Command(_ context.Context, cmd []byte) ([]byte, error) {
	reading, _ := s.Read(context.Background())
	return []byte(reading.String()), nil
}

func (s *MockHumiditySensor) Read(_ context.Context) (*device.SensorReading, error) {
	humidity := 55.0 + (rand.Float64()-0.5)*20
	return &device.SensorReading{
		Name:  s.name,
		Value: humidity,
		Unit:  "%",
	}, nil
}
