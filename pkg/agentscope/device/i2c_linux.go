//go:build linux

package device

import (
	"context"
	"fmt"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// I2C ioctl constants.
const (
	i2cSlave = 0x0703 // I2C_SLAVE
	i2cRDWR  = 0x0707 // I2C_RDWR
)

// I2CConnector provides I2C communication via Linux i2c-dev.
// Pure Go implementation using ioctl — no CGO required.
type I2CConnector struct {
	busPath string
	addr    uint8

	mu     sync.Mutex
	file   *os.File
	opened bool
}

// I2COption configures an I2CConnector.
type I2COption func(*I2CConnector)

// NewI2CConnector creates an I2C connector for the given bus and device address.
// busPath is typically "/dev/i2c-0" or "/dev/i2c-1".
func NewI2CConnector(busPath string, addr uint8, opts ...I2COption) *I2CConnector {
	c := &I2CConnector{
		busPath: busPath,
		addr:    addr,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Open opens the I2C bus and selects the device address.
func (c *I2CConnector) Open() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.opened {
		return fmt.Errorf("i2c: already open")
	}

	f, err := os.OpenFile(c.busPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("i2c: open %s: %w", c.busPath, err)
	}

	// Set slave address.
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		f.Fd(),
		uintptr(i2cSlave),
		uintptr(c.addr),
	)
	if errno != 0 {
		_ = f.Close()
		return fmt.Errorf("i2c: set slave addr 0x%02x: %w", c.addr, errno)
	}

	c.file = f
	c.opened = true
	return nil
}

// Close releases the I2C bus file.
func (c *I2CConnector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.opened {
		return nil
	}
	err := c.file.Close()
	c.file = nil
	c.opened = false
	return err
}

// Command sends bytes to the I2C device and reads back a response.
// cmd format:
//   - cmd[0] = register address (or first data byte for raw write)
//   - cmd[1:] = data to write (if any)
//
// If cmd is a single byte (register address), it performs a read of up to 32 bytes
// from that register. If cmd has multiple bytes, it writes them all.
func (c *I2CConnector) Command(ctx context.Context, cmd []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.opened {
		return nil, fmt.Errorf("i2c: not open")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(cmd) == 0 {
		return nil, fmt.Errorf("i2c: empty command")
	}

	if len(cmd) == 1 {
		// Register read: write register address, then read.
		return c.readRegister(cmd[0])
	}

	// Write operation.
	_, err := c.file.Write(cmd)
	if err != nil {
		return nil, fmt.Errorf("i2c: write: %w", err)
	}
	return nil, nil
}

// readRegister writes the register address and reads back data.
func (c *I2CConnector) readRegister(reg byte) ([]byte, error) {
	// Write register address.
	if _, err := c.file.Write([]byte{reg}); err != nil {
		return nil, fmt.Errorf("i2c: write reg 0x%02x: %w", reg, err)
	}

	// Read response.
	buf := make([]byte, 32)
	n, err := c.file.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("i2c: read: %w", err)
	}
	return buf[:n], nil
}

// ReadRegisterN reads exactly n bytes from the given register.
func (c *I2CConnector) ReadRegisterN(ctx context.Context, reg byte, n int) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.opened {
		return nil, fmt.Errorf("i2c: not open")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if _, err := c.file.Write([]byte{reg}); err != nil {
		return nil, fmt.Errorf("i2c: write reg 0x%02x: %w", reg, err)
	}

	buf := make([]byte, n)
	_, err := c.file.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("i2c: read %d bytes: %w", n, err)
	}
	return buf, nil
}

// Ensure unsafe is used (for I2C_RDWR in future advanced ops).
var _ = unsafe.Pointer(nil)

// Compile-time interface check.
var _ Connector = (*I2CConnector)(nil)
