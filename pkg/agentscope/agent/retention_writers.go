package agent

// retentionVersion is transient, agent-local invalidation bookkeeping. It is
// deliberately absent from checkpoints. A version is meaningful only for the
// same agent instance; it neither certifies content ownership nor detects edits
// through caller/plugin aliases. Those require the later owned snapshot seam.
// All access is guarded by a.mu. Exhaustion permanently disables matching.
type retentionVersion struct {
	epoch     uint64
	exhausted bool
}

func (a *UnifiedAgent) invalidateRetentionLocked() {
	if !a.retentionTracking {
		return
	}
	if a.retentionVersion.epoch == ^uint64(0) {
		a.retentionVersion.exhausted = true
		return
	}
	a.retentionVersion.epoch++
}

// beginRetentionWrite tracks an operation across unlocked callbacks and I/O.
// Nested operations are counted independently. Admission and completion both
// invalidate: even a failed operation or a mutate-then-restore cannot revive a
// previous version. It does not change existing writer admission policies.
func (a *UnifiedAgent) beginRetentionWrite() func() {
	a.mu.Lock()
	if !a.retentionTracking {
		a.mu.Unlock()
		return func() {}
	}
	a.invalidateRetentionLocked()
	a.retentionWriters++
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.invalidateRetentionLocked()
		a.retentionWriters--
		a.mu.Unlock()
	}
}

// retentionIdleLocked supplements busyLocked without changing existing APIs'
// concurrency behavior. The snapshot seam must also check configuration,
// ownership and pending work; this predicate alone never grants eligibility.
func (a *UnifiedAgent) retentionIdleLocked() bool {
	return a.retentionTracking && !a.retentionVersion.exhausted && a.retentionWriters == 0 &&
		a.activeReplies == 0 && a.runningCores == 0 && a.activeConv == nil &&
		!a.precompressRunning && a.pendingPrecompress == nil && a.state.SummaryOffer == nil
}

// Writer coverage (all versions/counts are guarded by mu):
//   - Reply/Resume: reply admission -> core/reply completion; live context,
//     tool states/results, plan and recovery replacement invalidate under mu.
//   - Compact/CompactStrict/overflow: operation guard spans callbacks, model
//     calls and persistence, including fallback, block splitting and retries.
//   - ApplySummary/OfferSummary/ApplyThreadUpdate/SaveCheckpoint: operation
//     guard includes token counters and saver I/O; failed attempts invalidate.
//   - Background precompression: precompressRunning is set before spawning,
//     cleared after publication; both transitions invalidate. Pending result
//     publication/drop and SummaryOffer materialization invalidate under mu.
//   - Thread/budget edits, images, tool-result reduction and conversation
//     injection/ownership: locked invalidation at admission/publication.
// ForkSession/ForkSummary read the parent and write detached state, so they do
// not invalidate the parent. Options run only during construction; external
// configuration and caller/plugin aliases are NOT covered by this tracker.
// Read caches, usage-event accounting and confirmation mailboxes are auxiliary
// runtime state, not a content ownership guarantee or a snapshot binding.
