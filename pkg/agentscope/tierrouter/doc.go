// Package tierrouter is an experimental reference implementation of the
// three-tier (device/edge/cloud) routing-policy harness described in
// DESIGN.md (issue #67).
//
// The package is a routing-contract testbed: it models operations, an
// operation-level routing policy, a persisted decision log with a commit
// boundary (the attempt record is persisted before the provider call), and
// restart reconciliation. It is the routing-side sibling of the taskenvelope
// package: selecting another model endpoint does not create a remote-Agent
// subtask, so nothing here carries scope, allowance, or root/parent
// identity. Grading reads the decision log and the providers' request
// records, never a provider's self-report.
//
// ChatRouter separately adapts three-tier policies to plain-text, buffered
// ChatModel calls with fake deployments. It keeps chat records and completed
// responses in memory, guards dispatch and result admission with the original
// operation epoch/TTL, and never creates a taskenvelope subtask.
//
// Experimental: the API is not covered by the Stable tier in STABILITY.md
// and may change between releases.
package tierrouter
