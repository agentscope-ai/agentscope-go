// Package retentioncopy provides the internal bounded-copy foundation for W4-A.
// It is not wired into agent execution or a public snapshot API. Inputs must
// remain immutable during Copy; returned mutable storage is independently owned.
package retentioncopy

import (
	"context"
	"errors"
)

var (
	ErrInvalidLimits  = errors.New("retention copy: invalid limits")
	ErrInvalidContext = errors.New("retention copy: nil context")
	ErrLimit          = errors.New("retention copy: resource limit exceeded")
	ErrUnsupported    = errors.New("retention copy: unsupported value")
	ErrInvalidNumber  = errors.New("retention copy: invalid number")
)

// Error contains only a fixed resource name, never input data or a type's text.
type Error struct {
	Kind     error
	Resource string
}

func (e *Error) Error() string { return e.Kind.Error() + " (" + e.Resource + ")" }
func (e *Error) Unwrap() error { return e.Kind }

// Limits are immutable after New. All fields must be positive. Depth is capped
// at 128 to bound the recursion stack even when the input contains a cycle.
// AllocBytes is a logical allocation charge, not a Go allocator/RSS bound.
type Limits struct {
	MaxMessages, MaxBlocks, MaxNodes int64
	MaxPayloadBytes, MaxAllocBytes   int64
	MaxDepth                         int
}

// Stats describes work charged before an error or successful completion. A
// rejected reservation is not charged; allocated-but-discarded work is charged.
// Nodes count values (including nil), map keys, and each scalar struct field.
// Depth counts root=0. Message/block pointer wrappers count nodes but only their
// pointee counts as a message/block. Repeated references are copied and charged
// each time; graph identity is not retained. Strings/raw bytes count as payload.
type Stats struct {
	Messages, Blocks, Nodes, PayloadBytes, AllocBytes int64
	Depth                                             int
}

// Copier has no mutable shared state; concurrent Copy calls use independent
// budgets. The zero value is invalid. It never calls input-defined methods.
type Copier struct {
	limits Limits
	valid  bool
}

func New(limits Limits) (*Copier, error) {
	if limits.MaxMessages <= 0 || limits.MaxBlocks <= 0 || limits.MaxNodes <= 0 || limits.MaxPayloadBytes <= 0 || limits.MaxAllocBytes <= 0 || limits.MaxDepth <= 0 || limits.MaxDepth > 128 {
		return nil, ErrInvalidLimits
	}
	return &Copier{limits: limits, valid: true}, nil
}

type budget struct {
	ctx    context.Context
	limits Limits
	stats  Stats
}

func reserve(used *int64, max, count int64, resource string) error {
	if count < 0 || count > max-*used {
		return &Error{ErrLimit, resource}
	}
	*used += count
	return nil
}
func (b *budget) nodes(count int64) error {
	return reserve(&b.stats.Nodes, b.limits.MaxNodes, count, "nodes")
}
func (b *budget) alloc(count int64) error {
	return reserve(&b.stats.AllocBytes, b.limits.MaxAllocBytes, count, "allocation")
}
func (b *budget) payload(count int64) error {
	return reserve(&b.stats.PayloadBytes, b.limits.MaxPayloadBytes, count, "payload")
}
func (b *budget) enter(depth int) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if depth > b.limits.MaxDepth {
		return &Error{ErrLimit, "depth"}
	}
	if depth > b.stats.Depth {
		b.stats.Depth = depth
	}
	return b.nodes(1)
}

// children checks direct child capacity without charging it twice. Actual
// child visits charge nodes; nested children may exhaust the remaining budget.
func (b *budget) children(count int, per int64) error {
	if int64(count) > (b.limits.MaxNodes-b.stats.Nodes)/per {
		return &Error{ErrLimit, "nodes"}
	}
	return nil
}

// slots checks multiplication before allocation. Logical costs: 16 per value,
// actual Go header/pointee/element sizes, plus 128 per map and 128 per map entry.
// Map reserves conservatively account for runtime metadata but are not a heap
// measurement. Stack/runtime/GC overhead and allocator rounding are not measured.
func (b *budget) slots(count int, width uintptr) error {
	remaining := b.limits.MaxAllocBytes - b.stats.AllocBytes
	if width > 0 && uint64(count) > uint64(remaining)/uint64(width) {
		return &Error{ErrLimit, "allocation"}
	}
	return b.alloc(int64(uint64(count) * uint64(width)))
}
