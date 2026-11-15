//go:build linux

package device

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// CAN protocol constants.
const (
	afCAN        = 29 // AF_CAN
	pfCAN        = afCAN
	canRaw       = 1 // CAN_RAW protocol
	canMTU       = 16
	canMaxDLC    = 8
	canRawFilter = 1 // CAN_RAW_FILTER
)

// canFrame matches struct can_frame from linux/can.h.
type canFrame struct {
	ID   uint32
	DLC  uint8
	Pad  uint8
	Res0 uint8
	Res1 uint8
	Data [canMaxDLC]byte
}

// sockaddrCAN matches struct sockaddr_can.
type sockaddrCAN struct {
	Family  uint16
	Ifindex int32
	Addr    [8]byte // tp/j1939 addr union — unused for raw
}

// CANConnector provides SocketCAN communication via AF_CAN raw sockets.
// Pure Go implementation — no CGO required.
type CANConnector struct {
	ifaceName string
	timeout   time.Duration

	mu     sync.Mutex
	fd     int
	opened bool
}

// CANOption configures a CANConnector.
type CANOption func(*CANConnector)

// WithCANTimeout sets the read timeout for CAN frames. Default: 1s.
func WithCANTimeout(d time.Duration) CANOption {
	return func(c *CANConnector) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// NewCANConnector creates a CAN bus connector for the given interface (e.g. "can0").
func NewCANConnector(ifaceName string, opts ...CANOption) *CANConnector {
	c := &CANConnector{
		ifaceName: ifaceName,
		timeout:   1 * time.Second,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Open creates a raw CAN socket and binds to the interface.
func (c *CANConnector) Open() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.opened {
		return fmt.Errorf("can: already open")
	}

	// Resolve interface index.
	iface, err := net.InterfaceByName(c.ifaceName)
	if err != nil {
		return fmt.Errorf("can: interface %s: %w", c.ifaceName, err)
	}

	// Create raw CAN socket.
	fd, err := unix.Socket(pfCAN, unix.SOCK_RAW, canRaw)
	if err != nil {
		return fmt.Errorf("can: socket: %w", err)
	}

	// Bind to the interface.
	addr := sockaddrCAN{
		Family:  afCAN,
		Ifindex: int32(iface.Index),
	}
	_, _, errno := unix.Syscall(
		unix.SYS_BIND,
		uintptr(fd),
		uintptr(unsafe.Pointer(&addr)),
		unsafe.Sizeof(addr),
	)
	if errno != 0 {
		_ = unix.Close(fd)
		return fmt.Errorf("can: bind to %s: %w", c.ifaceName, errno)
	}

	// Set read timeout.
	tv := unix.NsecToTimeval(c.timeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("can: set timeout: %w", err)
	}

	c.fd = fd
	c.opened = true
	return nil
}

// Close closes the CAN socket.
func (c *CANConnector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.opened {
		return nil
	}
	err := unix.Close(c.fd)
	c.fd = 0
	c.opened = false
	return err
}

// Command sends a CAN frame and waits for a response frame.
// cmd format: first 4 bytes = CAN ID (little-endian), remaining = data (up to 8 bytes).
// Response format: same layout.
func (c *CANConnector) Command(ctx context.Context, cmd []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.opened {
		return nil, fmt.Errorf("can: not open")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(cmd) < 4 {
		return nil, fmt.Errorf("can: command too short (need at least 4 bytes for CAN ID)")
	}

	// Parse command: [4-byte ID][data...]
	var frame canFrame
	frame.ID = binary.LittleEndian.Uint32(cmd[:4])
	data := cmd[4:]
	if len(data) > canMaxDLC {
		data = data[:canMaxDLC]
	}
	frame.DLC = uint8(len(data))
	copy(frame.Data[:], data)

	// Send frame.
	frameBytes := (*[canMTU]byte)(unsafe.Pointer(&frame))
	_, err := unix.Write(c.fd, frameBytes[:canMTU])
	if err != nil {
		return nil, fmt.Errorf("can: write: %w", err)
	}

	// Read response frame.
	var respFrame canFrame
	respBytes := (*[canMTU]byte)(unsafe.Pointer(&respFrame))
	_, err = unix.Read(c.fd, respBytes[:canMTU])
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("can: read: %w", err)
	}

	// Encode response: [4-byte ID][data...]
	resp := make([]byte, 4+int(respFrame.DLC))
	binary.LittleEndian.PutUint32(resp[:4], respFrame.ID)
	copy(resp[4:], respFrame.Data[:respFrame.DLC])
	return resp, nil
}

// Compile-time interface check.
var _ Connector = (*CANConnector)(nil)
