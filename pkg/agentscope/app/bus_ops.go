package app

import (
	"context"
	"encoding/json"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/messagebus"
)

// PublishSessionEvent appends an event to the session's replay log and
// publishes it for live subscribers. This is the central bus operation
// used by ChatService and SessionProjection.
func PublishSessionEvent(bus messagebus.MessageBus, sessionID string, evt event.Event) error {
	topic := "session:" + sessionID + ":events"

	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}

	// Append to replay log (queue)
	_ = bus.QueuePush(context.TODO(), topic+":log", data)

	// Publish live
	return bus.Publish(context.TODO(), topic, data)
}

// EnqueueRunTrigger queues a run trigger event for a session.
// Dispatchers (e.g., WakeupDispatcher) consume these triggers.
func EnqueueRunTrigger(bus messagebus.MessageBus, sessionID string, triggerType string, payload any) error {
	data, err := json.Marshal(map[string]any{
		"type":       triggerType,
		"session_id": sessionID,
		"payload":    payload,
	})
	if err != nil {
		return err
	}
	topic := "session:" + sessionID + ":triggers"
	_ = bus.QueuePush(context.TODO(), topic, data)
	return nil
}
