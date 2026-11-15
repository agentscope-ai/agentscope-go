//go:build linux

package device

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// SerialConnector provides serial port communication via Linux termios.
// Pure Go implementation using unix syscalls — no CGO required.
type SerialConnector struct {
	path     string
	baudRate uint32
	dataBits uint8
	stopBits uint8
	parity   byte // 'N', 'E', 'O'
	timeout  time.Duration

	mu   sync.Mutex
	file *os.File
	fd   int
}

// SerialOption configures a SerialConnector.
type SerialOption func(*SerialConnector)

// WithBaudRate sets the baud rate (default: 9600).
func WithBaudRate(rate uint32) SerialOption {
	return func(s *SerialConnector) { s.baudRate = rate }
}

// WithDataBits sets data bits (5-8, default: 8).
func WithDataBits(bits uint8) SerialOption {
	return func(s *SerialConnector) {
		if bits >= 5 && bits <= 8 {
			s.dataBits = bits
		}
	}
}

// WithStopBits sets stop bits (1 or 2, default: 1).
func WithStopBits(bits uint8) SerialOption {
	return func(s *SerialConnector) {
		if bits == 1 || bits == 2 {
			s.stopBits = bits
		}
	}
}

// WithParity sets parity: 'N' (none), 'E' (even), 'O' (odd). Default: 'N'.
func WithParity(p byte) SerialOption {
	return func(s *SerialConnector) {
		if p == 'N' || p == 'E' || p == 'O' {
			s.parity = p
		}
	}
}

// WithTimeout sets the read timeout. Default: 1s.
func WithTimeout(d time.Duration) SerialOption {
	return func(s *SerialConnector) {
		if d > 0 {
			s.timeout = d
		}
	}
}

// NewSerialConnector creates a serial port connector.
func NewSerialConnector(path string, opts ...SerialOption) *SerialConnector {
	s := &SerialConnector{
		path:     path,
		baudRate: 9600,
		dataBits: 8,
		stopBits: 1,
		parity:   'N',
		timeout:  1 * time.Second,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Open initializes the serial port with configured parameters.
func (s *SerialConnector) Open() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file != nil {
		return fmt.Errorf("serial: already open")
	}

	f, err := os.OpenFile(s.path, os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("serial: open %s: %w", s.path, err)
	}

	fd := int(f.Fd())

	// Get current termios settings.
	var tios unix.Termios
	if err := termiosGet(fd, &tios); err != nil {
		_ = f.Close()
		return fmt.Errorf("serial: get termios: %w", err)
	}

	// Configure raw mode.
	tios.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	tios.Oflag &^= unix.OPOST
	tios.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	tios.Cflag &^= unix.CSIZE | unix.PARENB

	// Data bits.
	switch s.dataBits {
	case 5:
		tios.Cflag |= unix.CS5
	case 6:
		tios.Cflag |= unix.CS6
	case 7:
		tios.Cflag |= unix.CS7
	default:
		tios.Cflag |= unix.CS8
	}

	// Stop bits.
	if s.stopBits == 2 {
		tios.Cflag |= unix.CSTOPB
	}

	// Parity.
	switch s.parity {
	case 'E':
		tios.Cflag |= unix.PARENB
	case 'O':
		tios.Cflag |= unix.PARENB | unix.PARODD
	}

	// Enable receiver, local mode.
	tios.Cflag |= unix.CLOCAL | unix.CREAD

	// Baud rate.
	speed, ok := baudRateToSpeed(s.baudRate)
	if !ok {
		_ = f.Close()
		return fmt.Errorf("serial: unsupported baud rate %d", s.baudRate)
	}
	tios.Ispeed = speed
	tios.Ospeed = speed

	// Read timeout: VMIN=0, VTIME in tenths of second.
	tios.Cc[unix.VMIN] = 0
	vtime := uint8(s.timeout.Milliseconds() / 100)
	if vtime == 0 {
		vtime = 1
	}
	tios.Cc[unix.VTIME] = vtime

	if err := termiosSet(fd, &tios); err != nil {
		_ = f.Close()
		return fmt.Errorf("serial: set termios: %w", err)
	}

	// Clear O_NONBLOCK now that configuration is done.
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = f.Close()
		return fmt.Errorf("serial: clear nonblock: %w", err)
	}

	s.file = f
	s.fd = fd
	return nil
}

// Close releases the serial port.
func (s *SerialConnector) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	s.fd = 0
	return err
}

// Command sends cmd bytes and reads the response.
func (s *SerialConnector) Command(ctx context.Context, cmd []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file == nil {
		return nil, fmt.Errorf("serial: not open")
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Write command.
	if _, err := s.file.Write(cmd); err != nil {
		return nil, fmt.Errorf("serial: write: %w", err)
	}

	// Read response with context deadline.
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(s.timeout)
	}

	buf := make([]byte, 4096)
	var response []byte

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return response, ctx.Err()
		}
		n, err := s.file.Read(buf)
		if n > 0 {
			response = append(response, buf[:n]...)
		}
		if err != nil {
			break
		}
		if n == 0 {
			break // VTIME elapsed with no data
		}
	}

	return response, nil
}

// termiosGet reads termios settings via ioctl.
func termiosGet(fd int, tios *unix.Termios) error {
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.TCGETS),
		uintptr(unsafe.Pointer(tios)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// termiosSet writes termios settings via ioctl.
func termiosSet(fd int, tios *unix.Termios) error {
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.TCSETSF),
		uintptr(unsafe.Pointer(tios)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// baudRateToSpeed maps common baud rates to their termios speed constants.
func baudRateToSpeed(rate uint32) (uint32, bool) {
	speeds := map[uint32]uint32{
		300:    unix.B300,
		600:    unix.B600,
		1200:   unix.B1200,
		2400:   unix.B2400,
		4800:   unix.B4800,
		9600:   unix.B9600,
		19200:  unix.B19200,
		38400:  unix.B38400,
		57600:  unix.B57600,
		115200: unix.B115200,
		230400: unix.B230400,
		460800: unix.B460800,
		921600: unix.B921600,
	}
	s, ok := speeds[rate]
	return s, ok
}

// Compile-time interface check.
var _ Connector = (*SerialConnector)(nil)
