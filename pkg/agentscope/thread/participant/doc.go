// Package participant wraps agents as Main Thread participants (EXPERIMENTAL).
//
// A participant is what the thread Coordinator drives: it imports thread
// entries into its agent's context, runs one reply per activation, forwards
// the reply's events, and reports what the user saw, which tools ran and what
// the model calls cost. Two kinds exist:
//
//   - AgentParticipant wraps one UnifiedAgent. Build it in two steps: Prepare
//     returns the agent options and thread tools, the host builds the agent
//     with them, and Prepared.Participant binds the agent.
//   - TeamParticipant wraps a master agent with sub-agents (members) through
//     pipeline.TeamPipeline. Members never see the thread.
//
// The thread tools let the model hand the foreground to another participant
// (transfer), start background work (assign_task), publish results (report,
// publish_artifact) and read the shared thread and peers' tool outputs
// (thread_read, event_read). The four writing tools are external tools: the
// participant answers them itself, at their exact place in the event stream,
// so the text sealed by a transfer is exact. A reply started outside Activate gets a
// RequireExternalExecutionEvent for them that its caller must answer.
//
// Everything here may change without notice. See docs/main-thread.md.
package participant
