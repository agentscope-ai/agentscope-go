package grpc

import "sync"

// clientStream owns channel delivery and closure. Sending never blocks while
// holding mu; cancellation can safely close the channel at any time.
type clientStream struct {
	mu       sync.Mutex
	messages chan *Message
	closed   bool
	stop     func() bool
}

// deliver reports whether the stream remains active. Overflow closes the
// stream without a synthetic StreamEnd, preserving an observable distinction
// between successful completion and interruption.
func (s *clientStream) deliver(msg *Message) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case s.messages <- msg:
		if !msg.StreamEnd {
			return true
		}
	default:
	}
	s.closeLocked()
	return false
}

func (s *clientStream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
}

func (s *clientStream) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	if s.stop != nil {
		s.stop()
	}
	close(s.messages)
}
