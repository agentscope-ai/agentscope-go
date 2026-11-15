// Package thread lets a cloud participant and an edge participant share one
// continuous session through an append-only Main Thread log.
//
// EXPERIMENTAL: every exported identifier in this package may change without
// notice. See docs/main-thread.md for the design and its limits.
//
// Each participant keeps its own agent context, tools and summaries. The log
// holds only published content: user input, the text the user actually saw,
// tool digests (names and arguments cut at 200 runes, never outputs),
// progress of running work, summaries a cloud fork
// wrote for another participant's window, ownership changes, background
// assignments, reports, artifacts and markers. A Coordinator is the single
// writer. It routes each query, keeps the foreground owner and its epoch,
// forwards only the owner's output to the user, commits Transfer, Assign and
// Report, wakes participants, schedules summary forks and records the shared
// ledger.
//
// Invariants:
//
//  1. One sequencer, total order.
//  2. Entries are immutable; corrections are new entries.
//  3. The current owner is the target of the last owner_change entry.
//  4. Only the current owner's foreground activation reaches the user. Writes
//     the user can see carry a Guard with the owner and epoch.
//  5. A participant's cursor and the content imported up to it are updated in
//     one state change and one checkpoint.
//  6. Entries are never stranded: after every activation the Coordinator
//     checks for entries addressed to the participant past its cursor.
package thread
