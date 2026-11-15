// Example edge_fleet demonstrates multi-device coordination using the PubSub
// interface for inter-agent communication.
//
// It simulates 3 edge agents (sensor, processor, actuator) coordinating via
// an in-memory PubSub bus. In production, replace with MQTT for cross-device
// communication.
//
// Run:
//
//	go run ./examples/edge_fleet/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/messagebus"
)

// InMemoryPubSub is a minimal in-process PubSub for the fleet demo.
// In production, use messagebus/mqtt.MQTTTransport.
type InMemoryPubSub struct {
	mu   sync.RWMutex
	subs map[string][]chan messagebus.PubSubMessage
}

func NewInMemoryPubSub() *InMemoryPubSub {
	return &InMemoryPubSub{
		subs: make(map[string][]chan messagebus.PubSubMessage),
	}
}

func (ps *InMemoryPubSub) Publish(_ context.Context, topic string, data []byte, opts ...messagebus.PubOption) error {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	for _, ch := range ps.subs[topic] {
		select {
		case ch <- messagebus.PubSubMessage{Topic: topic, Payload: data}:
		default:
		}
	}
	return nil
}

func (ps *InMemoryPubSub) Subscribe(_ context.Context, topic string, opts ...messagebus.SubOption) (<-chan messagebus.PubSubMessage, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ch := make(chan messagebus.PubSubMessage, 16)
	ps.subs[topic] = append(ps.subs[topic], ch)
	return ch, nil
}

func (ps *InMemoryPubSub) Unsubscribe(topic string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, ch := range ps.subs[topic] {
		close(ch)
	}
	delete(ps.subs, topic)
	return nil
}

func (ps *InMemoryPubSub) Close() error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for topic, chs := range ps.subs {
		for _, ch := range chs {
			close(ch)
		}
		delete(ps.subs, topic)
	}
	return nil
}

// SensorReading is the message format published by the sensor agent.
type SensorReading struct {
	Device string  `json:"device"`
	Temp   float64 `json:"temp"`
	Time   string  `json:"time"`
}

// Command is the message format published by the processor agent.
type Command struct {
	Target string `json:"target"`
	Action string `json:"action"`
	Reason string `json:"reason"`
}

func main() {
	bus := NewInMemoryPubSub()
	defer bus.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup

	// Agent 1: Sensor agent — publishes temperature readings.
	wg.Add(1)
	go func() {
		defer wg.Done()
		sensorAgent(ctx, bus)
	}()

	// Agent 2: Processor agent — subscribes to sensor data, publishes commands.
	wg.Add(1)
	go func() {
		defer wg.Done()
		processorAgent(ctx, bus)
	}()

	// Agent 3: Actuator agent — subscribes to commands, executes them.
	wg.Add(1)
	go func() {
		defer wg.Done()
		actuatorAgent(ctx, bus)
	}()

	wg.Wait()
	fmt.Println("\n--- Fleet coordination demo complete ---")
}

func sensorAgent(ctx context.Context, bus messagebus.PubSub) {
	fmt.Println("[Sensor] Starting temperature monitoring...")
	ticker := time.NewTicker(800 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reading := SensorReading{
				Device: "sensor-01",
				Temp:   20 + rand.Float64()*15, // 20-35°C
				Time:   time.Now().Format(time.RFC3339),
			}
			data, _ := json.Marshal(reading)
			if err := bus.Publish(ctx, "sensors/temperature", data); err == nil {
				fmt.Printf("[Sensor] Published: %.1f°C\n", reading.Temp)
			}
		}
	}
}

func processorAgent(ctx context.Context, bus messagebus.PubSub) {
	ch, err := bus.Subscribe(ctx, "sensors/temperature")
	if err != nil {
		fmt.Printf("[Processor] Subscribe error: %v\n", err)
		return
	}

	fmt.Println("[Processor] Listening for sensor data...")
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			var reading SensorReading
			if err := json.Unmarshal(msg.Payload, &reading); err != nil {
				continue
			}

			// Simple decision logic: if temp > 30, command cooling.
			if reading.Temp > 30 {
				cmd := Command{
					Target: "actuator-fan",
					Action: "ON",
					Reason: fmt.Sprintf("temperature %.1f°C exceeds threshold", reading.Temp),
				}
				data, _ := json.Marshal(cmd)
				bus.Publish(ctx, "commands/actuator", data)
				fmt.Printf("[Processor] HIGH TEMP! Commanding fan ON (%.1f°C)\n", reading.Temp)
			} else if reading.Temp < 22 {
				cmd := Command{
					Target: "actuator-fan",
					Action: "OFF",
					Reason: fmt.Sprintf("temperature %.1f°C below comfort zone", reading.Temp),
				}
				data, _ := json.Marshal(cmd)
				bus.Publish(ctx, "commands/actuator", data)
				fmt.Printf("[Processor] LOW TEMP. Commanding fan OFF (%.1f°C)\n", reading.Temp)
			}
		}
	}
}

func actuatorAgent(ctx context.Context, bus messagebus.PubSub) {
	ch, err := bus.Subscribe(ctx, "commands/actuator")
	if err != nil {
		fmt.Printf("[Actuator] Subscribe error: %v\n", err)
		return
	}

	fmt.Println("[Actuator] Listening for commands...")
	fanState := "OFF"

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			var cmd Command
			if err := json.Unmarshal(msg.Payload, &cmd); err != nil {
				continue
			}

			if cmd.Action != fanState {
				fanState = cmd.Action
				fmt.Printf("[Actuator] Fan -> %s (reason: %s)\n", fanState, cmd.Reason)
			}
		}
	}
}
