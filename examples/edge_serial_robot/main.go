// Example edge_serial_robot demonstrates using DeviceTool with a serial-connected
// robot arm controlled by an AI agent.
//
// This example uses a mock serial device for portability. On real hardware,
// replace MockSerialDevice with device.NewSerialConnector("/dev/ttyUSB0", ...).
//
// Run:
//
//	go run ./examples/edge_serial_robot/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/device"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// MockSerialDevice simulates a serial-connected robot arm.
type MockSerialDevice struct {
	opened   bool
	position [3]int // X, Y, Z
}

func (d *MockSerialDevice) Open() error {
	d.opened = true
	d.position = [3]int{0, 0, 0}
	fmt.Println("[Robot] Connected. Home position: X=0 Y=0 Z=0")
	return nil
}

func (d *MockSerialDevice) Close() error {
	d.opened = false
	fmt.Println("[Robot] Disconnected.")
	return nil
}

func (d *MockSerialDevice) Command(ctx context.Context, cmd []byte) ([]byte, error) {
	if !d.opened {
		return nil, fmt.Errorf("robot: not connected")
	}

	command := strings.TrimSpace(string(cmd))

	switch {
	case command == "STATUS":
		return []byte(fmt.Sprintf("OK X=%d Y=%d Z=%d", d.position[0], d.position[1], d.position[2])), nil
	case command == "HOME":
		d.position = [3]int{0, 0, 0}
		return []byte("OK HOMED"), nil
	case strings.HasPrefix(command, "MOVE"):
		// Parse: MOVE X10 Y20 Z5
		var x, y, z int
		fmt.Sscanf(command, "MOVE X%d Y%d Z%d", &x, &y, &z)
		d.position = [3]int{x, y, z}
		return []byte(fmt.Sprintf("OK MOVED X=%d Y=%d Z=%d", x, y, z)), nil
	case command == "GRIP":
		return []byte("OK GRIPPED"), nil
	case command == "RELEASE":
		return []byte("OK RELEASED"), nil
	default:
		return []byte("ERR UNKNOWN_COMMAND"), nil
	}
}

func main() {
	// Create mock robot device.
	robot := &MockSerialDevice{}

	// Open the device.
	if err := robot.Open(); err != nil {
		fmt.Printf("Failed to connect to robot: %v\n", err)
		return
	}
	defer robot.Close()

	// Create a watchdog that homes the robot on timeout.
	wd := device.NewWatchdog(10*time.Second, func() {
		fmt.Println("[WATCHDOG] Agent timeout! Homing robot for safety.")
		robot.Command(context.Background(), []byte("HOME"))
	})

	// Create the device tool.
	robotTool := device.NewDeviceTool(
		"robot_arm",
		"Control the robot arm. Commands: STATUS, HOME, MOVE X<n> Y<n> Z<n>, GRIP, RELEASE",
		robot,
		false, // not a sensor — it's an actuator
		device.WithDeviceTimeout(5*time.Second),
		device.WithWatchdog(wd),
	)

	// Start watchdog.
	wd.Start()
	defer wd.Stop()

	// Create a toolkit with the robot tool.
	tk := tool.NewToolkit(robotTool)

	// Simulate agent issuing commands.
	ctx := context.Background()
	commands := []string{"STATUS", "MOVE X100 Y50 Z25", "GRIP", "STATUS", "HOME"}

	fmt.Println("\n--- Agent commanding robot ---")
	for _, cmd := range commands {
		input := map[string]any{"command": cmd}
		resp, err := tk.CallTool(ctx, "robot_arm", input)
		if err != nil {
			fmt.Printf("[Agent] Error: %v\n", err)
			continue
		}

		// Extract text from response.
		if len(resp.Content) > 0 {
			data, _ := json.Marshal(resp.Content[0])
			fmt.Printf("[Agent] Sent: %-25s Got: %s\n", cmd, string(data))
		}

		time.Sleep(500 * time.Millisecond)
	}

	fmt.Println("\n--- Demo complete ---")
	fmt.Printf("Tool schemas: %d tool(s) registered\n", len(tk.GetToolSchemas()))
}
