//go:build linux

package device

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// GPIO ioctl constants for the Linux chardev interface (/dev/gpiochipN).
// See: include/uapi/linux/gpio.h in the kernel source.
const (
	gpioGetChipInfoIoctl    = 0xB401
	gpioGetLineHandleIoctl  = 0xC250B408
	gpioHandleGetLineValues = 0xC040B408
	gpioHandleSetLineValues = 0xC040B409

	gpioHandleRequestOutput = 0x02
	gpioHandleRequestInput  = 0x01

	gpioMaxNameSize = 32
	gpioMaxLines    = 64
)

// gpioChipInfo matches struct gpiochip_info from linux/gpio.h
type gpioChipInfo struct {
	Name  [gpioMaxNameSize]byte
	Label [gpioMaxNameSize]byte
	Lines uint32
}

// gpioHandleRequest matches struct gpiohandle_request from linux/gpio.h
type gpioHandleRequest struct {
	LineOffsets   [gpioMaxLines]uint32
	Flags         uint32
	DefaultValues [gpioMaxLines]uint8
	ConsumerLabel [gpioMaxNameSize]byte
	Lines         uint32
	Fd            int32
}

// gpioHandleData matches struct gpiohandle_data from linux/gpio.h
type gpioHandleData struct {
	Values [gpioMaxLines]uint8
}

// GPIOConnector provides access to Linux GPIO pins via the chardev interface.
// Pure Go implementation using ioctl syscalls — no CGO required.
type GPIOConnector struct {
	chipPath string
	pin      uint32
	output   bool // true for output, false for input
	label    string

	mu       sync.Mutex
	chipFile *os.File
	lineFd   int
	opened   bool
}

// GPIOOption configures a GPIOConnector.
type GPIOOption func(*GPIOConnector)

// WithGPIOOutput configures the pin as output. Default is input.
func WithGPIOOutput() GPIOOption {
	return func(g *GPIOConnector) { g.output = true }
}

// WithGPIOLabel sets the consumer label for the GPIO line request.
func WithGPIOLabel(label string) GPIOOption {
	return func(g *GPIOConnector) { g.label = label }
}

// NewGPIOConnector creates a GPIO connector for a specific pin on a chip.
func NewGPIOConnector(chipPath string, pin uint32, opts ...GPIOOption) *GPIOConnector {
	g := &GPIOConnector{
		chipPath: chipPath,
		pin:      pin,
		label:    "agentscope",
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Open requests the GPIO line from the kernel.
func (g *GPIOConnector) Open() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.opened {
		return fmt.Errorf("gpio: already open")
	}

	f, err := os.OpenFile(g.chipPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("gpio: open chip %s: %w", g.chipPath, err)
	}

	// Verify chip is accessible and pin is valid.
	var info gpioChipInfo
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		f.Fd(),
		uintptr(gpioGetChipInfoIoctl),
		uintptr(unsafe.Pointer(&info)),
	)
	if errno != 0 {
		_ = f.Close()
		return fmt.Errorf("gpio: get chip info: %w", errno)
	}
	if g.pin >= info.Lines {
		_ = f.Close()
		return fmt.Errorf("gpio: pin %d out of range (chip has %d lines)", g.pin, info.Lines)
	}

	// Request the line handle.
	var req gpioHandleRequest
	req.LineOffsets[0] = g.pin
	req.Lines = 1
	copy(req.ConsumerLabel[:], g.label)

	if g.output {
		req.Flags = gpioHandleRequestOutput
	} else {
		req.Flags = gpioHandleRequestInput
	}

	_, _, errno = unix.Syscall(
		unix.SYS_IOCTL,
		f.Fd(),
		uintptr(gpioGetLineHandleIoctl),
		uintptr(unsafe.Pointer(&req)),
	)
	if errno != 0 {
		_ = f.Close()
		return fmt.Errorf("gpio: request line %d: %w", g.pin, errno)
	}

	g.chipFile = f
	g.lineFd = int(req.Fd)
	g.opened = true
	return nil
}

// Close releases the GPIO line.
func (g *GPIOConnector) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.opened {
		return nil
	}

	_ = unix.Close(g.lineFd)
	g.lineFd = 0
	err := g.chipFile.Close()
	g.chipFile = nil
	g.opened = false
	return err
}

// Command interprets cmd as a GPIO operation:
//   - Read (cmd[0] == 'R' or empty): returns 1 byte (0 or 1) with pin value
//   - Write (cmd[0] == 'W'): sets pin to cmd[1] (0 or 1)
func (g *GPIOConnector) Command(ctx context.Context, cmd []byte) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.opened {
		return nil, fmt.Errorf("gpio: not open")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	op := byte('R')
	if len(cmd) > 0 {
		op = cmd[0]
	}

	switch op {
	case 'R', 'r':
		return g.readPin()
	case 'W', 'w':
		if len(cmd) < 2 {
			return nil, fmt.Errorf("gpio: write requires value byte")
		}
		return nil, g.writePin(cmd[1])
	default:
		return nil, fmt.Errorf("gpio: unknown command %q", op)
	}
}

func (g *GPIOConnector) readPin() ([]byte, error) {
	var data gpioHandleData
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(g.lineFd),
		uintptr(gpioHandleGetLineValues),
		uintptr(unsafe.Pointer(&data)),
	)
	if errno != 0 {
		return nil, fmt.Errorf("gpio: read pin %d: %w", g.pin, errno)
	}
	return []byte{data.Values[0]}, nil
}

func (g *GPIOConnector) writePin(value byte) error {
	if !g.output {
		return fmt.Errorf("gpio: pin %d is not configured as output", g.pin)
	}

	var data gpioHandleData
	if value != 0 {
		data.Values[0] = 1
	}
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(g.lineFd),
		uintptr(gpioHandleSetLineValues),
		uintptr(unsafe.Pointer(&data)),
	)
	if errno != 0 {
		return fmt.Errorf("gpio: write pin %d: %w", g.pin, errno)
	}
	return nil
}

// ChipInfo returns information about the GPIO chip.
func (g *GPIOConnector) ChipInfo() (*GPIOChipInfo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.opened {
		return nil, fmt.Errorf("gpio: not open")
	}

	var info gpioChipInfo
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		g.chipFile.Fd(),
		uintptr(gpioGetChipInfoIoctl),
		uintptr(unsafe.Pointer(&info)),
	)
	if errno != 0 {
		return nil, fmt.Errorf("gpio: get chip info: %w", errno)
	}

	return &GPIOChipInfo{
		Name:  cString(info.Name[:]),
		Label: cString(info.Label[:]),
		Lines: info.Lines,
	}, nil
}

// GPIOChipInfo holds GPIO chip metadata.
type GPIOChipInfo struct {
	Name  string
	Label string
	Lines uint32
}

// cString extracts a null-terminated C string from a byte slice.
func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// Ensure binary is imported (used for potential frame encoding).
var _ = binary.LittleEndian

// Compile-time interface check.
var _ Connector = (*GPIOConnector)(nil)
