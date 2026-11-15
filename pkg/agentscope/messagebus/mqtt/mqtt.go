//go:build mqtt

// Package mqtt provides a PubSub implementation backed by MQTT via the
// Eclipse Paho client. Intended for edge/IoT deployments where devices
// communicate via an MQTT broker.
//
// Build with: go build -tags mqtt
package mqtt

import (
	"context"
	"fmt"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/messagebus"
)

// MQTTTransport implements messagebus.PubSub over MQTT.
type MQTTTransport struct {
	client      paho.Client
	topicPrefix string
	subs        map[string]chan messagebus.PubSubMessage
	mu          sync.RWMutex
	closed      bool
}

// Option configures MQTTTransport.
type Option func(*mqttConfig)

type mqttConfig struct {
	clientID        string
	topicPrefix     string
	username        string
	password        string
	keepAlive       time.Duration
	connectTimeout  time.Duration
	cleanSession    bool
	autoReconnect   bool
	tlsSkipVerify   bool
	onConnect       func()
	onDisconnect    func(error)
}

// WithClientID sets the MQTT client identifier.
func WithClientID(id string) Option {
	return func(c *mqttConfig) { c.clientID = id }
}

// WithTopicPrefix sets a prefix applied to all topics.
func WithTopicPrefix(prefix string) Option {
	return func(c *mqttConfig) { c.topicPrefix = prefix }
}

// WithCredentials sets MQTT broker username and password.
func WithCredentials(username, password string) Option {
	return func(c *mqttConfig) {
		c.username = username
		c.password = password
	}
}

// WithKeepAlive sets the MQTT keep-alive interval.
func WithKeepAlive(d time.Duration) Option {
	return func(c *mqttConfig) { c.keepAlive = d }
}

// WithConnectTimeout sets the connection timeout.
func WithConnectTimeout(d time.Duration) Option {
	return func(c *mqttConfig) { c.connectTimeout = d }
}

// WithCleanSession sets the clean session flag.
func WithCleanSession(clean bool) Option {
	return func(c *mqttConfig) { c.cleanSession = clean }
}

// WithAutoReconnect enables automatic reconnection.
func WithAutoReconnect(enable bool) Option {
	return func(c *mqttConfig) { c.autoReconnect = enable }
}

// WithOnConnect sets a callback invoked on successful connection.
func WithOnConnect(fn func()) Option {
	return func(c *mqttConfig) { c.onConnect = fn }
}

// WithOnDisconnect sets a callback invoked on disconnection.
func WithOnDisconnect(fn func(error)) Option {
	return func(c *mqttConfig) { c.onDisconnect = fn }
}

// NewMQTTTransport creates a PubSub transport connected to an MQTT broker.
func NewMQTTTransport(broker string, opts ...Option) (*MQTTTransport, error) {
	cfg := &mqttConfig{
		clientID:       "agentscope-edge",
		keepAlive:      30 * time.Second,
		connectTimeout: 10 * time.Second,
		cleanSession:   true,
		autoReconnect:  true,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	pahoOpts := paho.NewClientOptions().
		AddBroker(broker).
		SetClientID(cfg.clientID).
		SetKeepAlive(cfg.keepAlive).
		SetConnectTimeout(cfg.connectTimeout).
		SetCleanSession(cfg.cleanSession).
		SetAutoReconnect(cfg.autoReconnect)

	if cfg.username != "" {
		pahoOpts.SetUsername(cfg.username)
		pahoOpts.SetPassword(cfg.password)
	}

	transport := &MQTTTransport{
		topicPrefix: cfg.topicPrefix,
		subs:        make(map[string]chan messagebus.PubSubMessage),
	}

	if cfg.onConnect != nil {
		onConnect := cfg.onConnect
		pahoOpts.SetOnConnectHandler(func(_ paho.Client) {
			onConnect()
		})
	}

	if cfg.onDisconnect != nil {
		onDisconnect := cfg.onDisconnect
		pahoOpts.SetConnectionLostHandler(func(_ paho.Client, err error) {
			onDisconnect(err)
		})
	}

	// Set default message handler for subscriptions.
	pahoOpts.SetDefaultPublishHandler(func(_ paho.Client, msg paho.Message) {
		transport.dispatchMessage(msg)
	})

	client := paho.NewClient(pahoOpts)
	token := client.Connect()
	if !token.WaitTimeout(cfg.connectTimeout) {
		return nil, fmt.Errorf("mqtt: connection timeout to %s", broker)
	}
	if err := token.Error(); err != nil {
		return nil, fmt.Errorf("mqtt: connect to %s: %w", broker, err)
	}

	transport.client = client
	return transport, nil
}

// Publish sends data to the given topic with optional QoS/retain settings.
func (t *MQTTTransport) Publish(ctx context.Context, topic string, data []byte, opts ...messagebus.PubOption) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	t.mu.RLock()
	if t.closed {
		t.mu.RUnlock()
		return fmt.Errorf("mqtt: transport closed")
	}
	t.mu.RUnlock()

	pcfg := messagebus.ApplyPubOptions(opts)
	fullTopic := t.topicPrefix + topic

	token := t.client.Publish(fullTopic, pcfg.QoS(), pcfg.Retain(), data)
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("mqtt: publish to %s: %w", fullTopic, err)
	}
	return nil
}

// Subscribe registers a subscription on the given topic and returns a channel.
func (t *MQTTTransport) Subscribe(ctx context.Context, topic string, opts ...messagebus.SubOption) (<-chan messagebus.PubSubMessage, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil, fmt.Errorf("mqtt: transport closed")
	}

	scfg := messagebus.ApplySubOptions(opts)
	fullTopic := t.topicPrefix + topic

	ch := make(chan messagebus.PubSubMessage, scfg.BufferSize())
	t.subs[fullTopic] = ch

	token := t.client.Subscribe(fullTopic, scfg.QoS(), func(_ paho.Client, msg paho.Message) {
		t.mu.RLock()
		sub, ok := t.subs[msg.Topic()]
		t.mu.RUnlock()
		if ok {
			select {
			case sub <- messagebus.PubSubMessage{
				Topic:   msg.Topic(),
				Payload: msg.Payload(),
				QoS:     msg.Qos(),
				Retain:  msg.Retained(),
			}:
			default:
				// Subscriber too slow, drop message.
			}
		}
	})
	token.Wait()
	if err := token.Error(); err != nil {
		delete(t.subs, fullTopic)
		close(ch)
		return nil, fmt.Errorf("mqtt: subscribe to %s: %w", fullTopic, err)
	}

	return ch, nil
}

// Unsubscribe removes a topic subscription and closes the channel.
func (t *MQTTTransport) Unsubscribe(topic string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	fullTopic := t.topicPrefix + topic
	ch, ok := t.subs[fullTopic]
	if !ok {
		return nil
	}

	token := t.client.Unsubscribe(fullTopic)
	token.Wait()

	delete(t.subs, fullTopic)
	close(ch)

	if err := token.Error(); err != nil {
		return fmt.Errorf("mqtt: unsubscribe from %s: %w", fullTopic, err)
	}
	return nil
}

// Close disconnects from the broker and releases resources.
func (t *MQTTTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true

	for topic, ch := range t.subs {
		close(ch)
		delete(t.subs, topic)
	}

	t.client.Disconnect(250) // 250ms quiesce
	return nil
}

// dispatchMessage routes incoming MQTT messages to the appropriate subscription channel.
func (t *MQTTTransport) dispatchMessage(msg paho.Message) {
	t.mu.RLock()
	ch, ok := t.subs[msg.Topic()]
	t.mu.RUnlock()

	if !ok {
		return
	}

	select {
	case ch <- messagebus.PubSubMessage{
		Topic:   msg.Topic(),
		Payload: msg.Payload(),
		QoS:     msg.Qos(),
		Retain:  msg.Retained(),
	}:
	default:
		// Drop if subscriber cannot keep up.
	}
}

// Compile-time interface check.
var _ messagebus.PubSub = (*MQTTTransport)(nil)
