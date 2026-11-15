package device

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// SensorMiddleware injects sensor readings into the system prompt.
// It polls registered sensors periodically and formats their readings
// as structured data that gets prepended to the system prompt, giving
// the agent real-time awareness of its physical environment.
//
// Usage:
//
//	sm := NewSensorMiddleware(WithMaxTokensBudget(200))
//	sm.Register("temp", tempSensor)
//	sm.Register("humidity", humiditySensor)
//	systemPrompt := sm.EnrichPrompt(ctx, basePrompt)
type SensorMiddleware struct {
	sensors     map[string]Sensor
	readings    map[string]*SensorReading
	maxTokens   int
	pollTimeout time.Duration
	mu          sync.RWMutex
}

// SensorMiddlewareOption configures SensorMiddleware.
type SensorMiddlewareOption func(*SensorMiddleware)

// WithMaxTokensBudget sets the maximum token budget for sensor data in the
// system prompt. Readings are truncated to fit within this budget.
// Default is 256 tokens (approximated as chars/4).
func WithMaxTokensBudget(tokens int) SensorMiddlewareOption {
	return func(sm *SensorMiddleware) {
		if tokens > 0 {
			sm.maxTokens = tokens
		}
	}
}

// WithPollTimeout sets the timeout for individual sensor reads during polling.
// Default is 2 seconds.
func WithPollTimeout(d time.Duration) SensorMiddlewareOption {
	return func(sm *SensorMiddleware) {
		if d > 0 {
			sm.pollTimeout = d
		}
	}
}

// NewSensorMiddleware creates a middleware that enriches system prompts with
// sensor data.
func NewSensorMiddleware(opts ...SensorMiddlewareOption) *SensorMiddleware {
	sm := &SensorMiddleware{
		sensors:     make(map[string]Sensor),
		readings:    make(map[string]*SensorReading),
		maxTokens:   256,
		pollTimeout: 2 * time.Second,
	}
	for _, opt := range opts {
		opt(sm)
	}
	return sm
}

// Register adds a named sensor to the middleware.
func (sm *SensorMiddleware) Register(name string, sensor Sensor) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.sensors[name] = sensor
}

// Unregister removes a sensor from the middleware.
func (sm *SensorMiddleware) Unregister(name string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sensors, name)
	delete(sm.readings, name)
}

// Poll reads all registered sensors and caches their values.
// Should be called before EnrichPrompt to get fresh data.
func (sm *SensorMiddleware) Poll(ctx context.Context) {
	sm.mu.Lock()
	sensors := make(map[string]Sensor, len(sm.sensors))
	for k, v := range sm.sensors {
		sensors[k] = v
	}
	sm.mu.Unlock()

	for name, sensor := range sensors {
		readCtx, cancel := context.WithTimeout(ctx, sm.pollTimeout)
		reading, err := sensor.Read(readCtx)
		cancel()

		sm.mu.Lock()
		if err != nil {
			logrus.WithError(err).Debugf("sensor %s: read failed", name)
			// Keep stale reading if available
		} else {
			sm.readings[name] = reading
		}
		sm.mu.Unlock()
	}
}

// EnrichPrompt prepends current sensor readings to the base system prompt.
// It respects the maxTokens budget to prevent context bloat.
func (sm *SensorMiddleware) EnrichPrompt(ctx context.Context, basePrompt string) string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if len(sm.readings) == 0 {
		return basePrompt
	}

	// Build sensor context block.
	var readings []map[string]any
	for name, r := range sm.readings {
		readings = append(readings, map[string]any{
			"sensor": name,
			"name":   r.Name,
			"value":  r.Value,
			"unit":   r.Unit,
		})
	}

	data, err := json.Marshal(readings)
	if err != nil {
		return basePrompt
	}

	sensorBlock := fmt.Sprintf("[SENSOR DATA]\n%s\n[/SENSOR DATA]\n\n", string(data))

	// Check token budget (approximate: 1 token ≈ 4 chars).
	estimatedTokens := len(sensorBlock) / 4
	if estimatedTokens > sm.maxTokens {
		// Truncate to fit budget.
		maxChars := sm.maxTokens * 4
		if maxChars < len(sensorBlock) {
			sensorBlock = sensorBlock[:maxChars] + "...\n[/SENSOR DATA]\n\n"
		}
	}

	return sensorBlock + basePrompt
}

// Readings returns the current cached sensor readings.
func (sm *SensorMiddleware) Readings() map[string]*SensorReading {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	result := make(map[string]*SensorReading, len(sm.readings))
	for k, v := range sm.readings {
		result[k] = v
	}
	return result
}

// FormatReadings returns a human-readable string of all current readings.
func (sm *SensorMiddleware) FormatReadings() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if len(sm.readings) == 0 {
		return "No sensor data available."
	}

	var sb strings.Builder
	for name, r := range sm.readings {
		sb.WriteString(fmt.Sprintf("  %s: %s\n", name, r.String()))
	}
	return sb.String()
}
