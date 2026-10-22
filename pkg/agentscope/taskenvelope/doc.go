// Package taskenvelope is an experimental reference implementation of the
// subtask envelope state machine specified in DESIGN.md (issue #67).
//
// The package is a delegation-state contract testbed: it models subtask
// records, their state transitions (table T1–T12 + T2b in DESIGN.md), the
// commit boundary (single-writer decision point; attempt reservation
// persisted before any external call), the allowance accounting model
// (settle, don't refund), and crash-window reconciliation. It is
// self-contained: it shares no state with the agent, storage or managedtask
// packages, and the device (caller) remains the action authority — the record
// never authorizes work by itself.
//
// Experimental: the API is not covered by the Stable tier in STABILITY.md and
// may change between releases.
package taskenvelope
