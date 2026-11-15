package messagebus

import "context"

// PubSub is a minimal publish/subscribe interface for IoT and edge messaging
// protocols (MQTT, AMQP, etc.). It is a narrower contract than the full
// MessageBus, focusing solely on topic-based pub/sub with QoS and retain
// semantics commonly used in embedded/device communication.
type PubSub interface {
	// Publish sends data to a topic with optional QoS/retain settings.
	Publish(ctx context.Context, topic string, data []byte, opts ...PubOption) error

	// Subscribe registers interest in a topic and returns a channel of messages.
	// The caller should call Unsubscribe when done.
	Subscribe(ctx context.Context, topic string, opts ...SubOption) (<-chan PubSubMessage, error)

	// Unsubscribe removes a subscription and closes the associated channel.
	Unsubscribe(topic string) error

	// Close shuts down the transport and releases resources.
	Close() error
}

// PubSubMessage represents a message received from a PubSub subscription.
type PubSubMessage struct {
	// Topic is the topic the message was received on.
	Topic string

	// Payload is the message data.
	Payload []byte

	// QoS is the quality-of-service level (0, 1, or 2).
	QoS byte

	// Retain indicates whether this was a retained message.
	Retain bool
}

// pubConfig holds options for a Publish call.
type pubConfig struct {
	qos    byte
	retain bool
}

// PubOption configures a Publish call.
type PubOption func(*pubConfig)

// WithQoS sets the quality-of-service level for a publish (0=at most once,
// 1=at least once, 2=exactly once).
func WithQoS(qos byte) PubOption {
	return func(c *pubConfig) {
		if qos <= 2 {
			c.qos = qos
		}
	}
}

// WithRetain sets the retain flag on a published message.
func WithRetain(retain bool) PubOption {
	return func(c *pubConfig) {
		c.retain = retain
	}
}

// subConfig holds options for a Subscribe call.
type subConfig struct {
	qos        byte
	bufferSize int
}

// SubOption configures a Subscribe call.
type SubOption func(*subConfig)

// WithSubQoS sets the maximum QoS level for received messages.
func WithSubQoS(qos byte) SubOption {
	return func(c *subConfig) {
		if qos <= 2 {
			c.qos = qos
		}
	}
}

// WithBufferSize sets the channel buffer size for received messages.
// Default is 64.
func WithBufferSize(size int) SubOption {
	return func(c *subConfig) {
		if size > 0 {
			c.bufferSize = size
		}
	}
}

// PubConfig holds resolved publish options. Exported for use by PubSub implementations.
type PubConfig struct {
	qos    byte
	retain bool
}

// QoS returns the quality-of-service level.
func (c PubConfig) QoS() byte { return c.qos }

// Retain returns the retain flag.
func (c PubConfig) Retain() bool { return c.retain }

// ApplyPubOptions applies PubOption functions and returns the resulting config.
func ApplyPubOptions(opts []PubOption) PubConfig {
	cfg := PubConfig{qos: 0, retain: false}
	pc := &pubConfig{}
	for _, opt := range opts {
		opt(pc)
	}
	cfg.qos = pc.qos
	cfg.retain = pc.retain
	return cfg
}

// SubConfig holds resolved subscribe options. Exported for use by PubSub implementations.
type SubConfig struct {
	qos        byte
	bufferSize int
}

// QoS returns the maximum QoS level.
func (c SubConfig) QoS() byte { return c.qos }

// BufferSize returns the channel buffer size.
func (c SubConfig) BufferSize() int { return c.bufferSize }

// ApplySubOptions applies SubOption functions and returns the resulting config.
func ApplySubOptions(opts []SubOption) SubConfig {
	cfg := SubConfig{qos: 0, bufferSize: 64}
	sc := &subConfig{bufferSize: 64}
	for _, opt := range opts {
		opt(sc)
	}
	cfg.qos = sc.qos
	cfg.bufferSize = sc.bufferSize
	return cfg
}
