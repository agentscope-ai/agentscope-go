// Package embodied provides experimental observation, plan and simulated
// action-admission contracts for issue #67 milestone 2A.
//
// Gateway composes taskenvelope for durable subtask lifecycle and recovery.
// It does not schedule plan steps, fetch media, control hardware, or provide
// real-time, exactly-once or physical-safety guarantees. The device host owns
// plan ordering and current world state; SimController is a deterministic
// fixture with an independent durable command and effect log.
package embodied
