package middleware

import (
	"context"
	"sync"
	"time"

	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// CostLedger aggregates model-call cost across sessions/agents/models/days
// (HARNESS_DESIGN D2). It is the cross-session query surface; per-session
// observed-cost guards are provided by CostTrackerMiddleware / ReplyCostBudget.
// These guards do not reserve cost for in-flight calls.
//
// Prometheus exports must only use the low-cardinality dimensions
// (agent/model/day). SessionID must never become a metric label — session
// IDs are unbounded generated values and would explode cardinality in
// embedded multi-tenant deployments.
// DefaultLedgerRetention bounds the raw entry buffer (HARNESS review M12):
// embedded-production data structures must stay memory-bounded. Oldest
// entries drop first; aggregates queried before eviction remain accurate.
const DefaultLedgerRetention = 100_000

type CostLedger struct {
	attempts  *inference.Ledger
	mu        sync.Mutex
	entries   []LedgerEntry
	retention int
}

// LedgerEntry is one recorded model call's cost attribution.
type LedgerEntry struct {
	Timestamp    time.Time
	SessionID    string
	AgentName    string
	ModelName    string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

// CostFilter selects ledger entries. Empty fields match everything.
type CostFilter struct {
	SessionID string
	AgentName string
	ModelName string
	Since     time.Time
}

// CostSummary is the aggregated answer to a CostFilter query.
type CostSummary struct {
	UnknownUsage          int
	UnknownCost           int
	Dropped               uint64
	Incomplete            bool
	CostOverflow          bool
	TokenOverflow         bool
	TotalCacheReadTokens  int
	TotalCacheWriteTokens int
	TotalCostUSD          float64
	TotalInTokens         int
	TotalOutTokens        int
	Calls                 int
	ByModel               map[string]float64
	ByAgent               map[string]float64
}

// NewCostLedger creates an empty ledger with DefaultLedgerRetention.
func NewCostLedger() *CostLedger {
	return &CostLedger{retention: DefaultLedgerRetention}
}

// NewCostLedgerWithRetention creates a ledger with a custom entry cap.
func NewCostLedgerWithRetention(retention int) *CostLedger {
	if retention <= 0 {
		retention = DefaultLedgerRetention
	}
	return &CostLedger{retention: retention}
}

// Record appends one cost attribution.
func (l *CostLedger) Record(e *LedgerEntry) {
	if l.attempts != nil {
		return
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	l.mu.Lock()
	l.entries = append(l.entries, *e)
	if over := len(l.entries) - l.retention; over > 0 {
		l.entries = l.entries[over:]
	}
	l.mu.Unlock()
}

// Summary aggregates entries matching the filter.
func (l *CostLedger) Summary(f CostFilter) CostSummary {
	if l.attempts != nil {
		return l.inferenceSummary(f)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := CostSummary{ByModel: map[string]float64{}, ByAgent: map[string]float64{}}
	for i := range l.entries {
		e := &l.entries[i]
		if f.SessionID != "" && e.SessionID != f.SessionID {
			continue
		}
		if f.AgentName != "" && e.AgentName != f.AgentName {
			continue
		}
		if f.ModelName != "" && e.ModelName != f.ModelName {
			continue
		}
		if !f.Since.IsZero() && e.Timestamp.Before(f.Since) {
			continue
		}
		out.TotalCostUSD += e.CostUSD
		out.TotalInTokens += e.InputTokens
		out.TotalOutTokens += e.OutputTokens
		out.Calls++
		out.ByModel[e.ModelName] += e.CostUSD
		out.ByAgent[e.AgentName] += e.CostUSD
	}
	return out
}

// CostTrackingMiddleware records every model call into a shared CostLedger
// (D2). Attach one per agent/session; the ledger itself is shared.
type CostTrackingMiddleware struct {
	BaseMiddleware
	ledger    *CostLedger
	sessionID string
	agentName string
	prices    map[string]model.Price // optional manual prices; falls back to model.ResolvePrice
}

// NewCostTracking creates the ledger-recording middleware.
func NewCostTracking(ledger *CostLedger, sessionID, agentName string, prices map[string]model.Price) *CostTrackingMiddleware {
	return &CostTrackingMiddleware{
		BaseMiddleware: BaseMiddleware{MiddlewareKey: "cost-tracking"},
		ledger:         ledger,
		sessionID:      sessionID,
		agentName:      agentName,
		prices:         prices,
	}
}

// OnModelCall records usage after each successful call.
func (m *CostTrackingMiddleware) OnModelCall(ctx context.Context, input *ModelCallInput, next ModelCallHandler) (*model.ChatResponse, error) {
	resp, err := next(ctx, input)
	if err != nil || resp == nil || resp.Usage == nil || m.ledger == nil || m.ledger.attempts != nil {
		return resp, err
	}
	price, ok := m.priceFor(input.ModelName)
	if !ok {
		return resp, nil // unknown price: record nothing rather than zero
	}
	m.ledger.Record(&LedgerEntry{
		SessionID:    m.sessionID,
		AgentName:    firstNonEmpty(m.agentName, input.AgentName),
		ModelName:    input.ModelName,
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		CostUSD:      price.CostUSD(resp.Usage),
	})
	return resp, nil
}

func (m *CostTrackingMiddleware) priceFor(modelName string) (model.Price, bool) {
	if p, ok := m.prices[modelName]; ok {
		return p, true
	}
	return model.ResolvePrice(modelName)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ReplyCostBudget enforces a USD budget per reply (HARNESS_DESIGN D3):
// a soft warning hint at WarnRatio (default 0.8) of the cap, and a hard
// stop (typed ErrBudgetExceeded) once the cap is exceeded. Complements the
// token-weighted ReplyBudgetControlMiddleware with cost-based accounting.
type ReplyCostBudgetMiddleware struct {
	BaseMiddleware
	maxUSD    float64
	warnRatio float64
	prices    map[string]model.Price
	hint      string
}

const defaultCostBudgetHint = "<system-reminder>You have used most of the cost budget for this reply. Finish concisely; avoid further tool calls unless strictly necessary.</system-reminder>"

// ReplyCostBudgetOption configures NewReplyCostBudget.
type ReplyCostBudgetOption func(*ReplyCostBudgetMiddleware)

// WithCostBudgetWarnRatio sets when the soft warning fires (default 0.8).
func WithCostBudgetWarnRatio(r float64) ReplyCostBudgetOption {
	return func(m *ReplyCostBudgetMiddleware) {
		if r > 0 && r < 1 {
			m.warnRatio = r
		}
	}
}

// WithCostBudgetHint overrides the warning hint text.
func WithCostBudgetHint(h string) ReplyCostBudgetOption {
	return func(m *ReplyCostBudgetMiddleware) {
		if h != "" {
			m.hint = h
		}
	}
}

// WithCostBudgetPrices supplies manual prices (fallback: model.ResolvePrice).
func WithCostBudgetPrices(p map[string]model.Price) ReplyCostBudgetOption {
	return func(m *ReplyCostBudgetMiddleware) { m.prices = p }
}

// NewReplyCostBudget creates the per-reply cost budget middleware.
func NewReplyCostBudget(maxUSD float64, opts ...ReplyCostBudgetOption) *ReplyCostBudgetMiddleware {
	m := &ReplyCostBudgetMiddleware{
		BaseMiddleware: BaseMiddleware{MiddlewareKey: "reply-cost-budget"},
		maxUSD:         maxUSD,
		warnRatio:      0.8,
		hint:           defaultCostBudgetHint,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// OnReply resets per-reply cost state.
func (m *ReplyCostBudgetMiddleware) OnReply(ctx context.Context, input ReplyInput, next ReplyHandler) <-chan event.Event {
	if mc := GetMiddleContext(ctx); mc != nil && budgetState(ctx) == nil {
		mc.Set(m.Key(), "spent", 0.0)
		mc.Set(m.Key(), "warned", false)
	}
	return next(ctx, input)
}

// OnModelCall checks recorded cost before a call, then accounts for returned
// usage. It does not reserve the call's cost or reject an overshooting response.
func (m *ReplyCostBudgetMiddleware) OnModelCall(ctx context.Context, input *ModelCallInput, next ModelCallHandler) (*model.ChatResponse, error) {
	mc := GetMiddleContext(ctx)
	if mc == nil || m.maxUSD <= 0 {
		return next(ctx, input)
	}
	spent := m.spentInContext(ctx, mc)
	if spent >= m.maxUSD {
		return nil, agenterrors.ErrBudgetExceeded
	}

	resp, err := next(ctx, input)
	if err != nil || resp == nil || resp.Usage == nil {
		return resp, err
	}
	if price, ok := m.priceFor(input.ModelName); ok {
		m.recordInContext(ctx, mc, price.CostUSD(resp.Usage))
	}
	return resp, nil
}

// OnSystemPrompt injects the warning hint once the soft threshold passes.
func (m *ReplyCostBudgetMiddleware) OnSystemPrompt(ctx context.Context, _ string, currentPrompt string) string {
	mc := GetMiddleContext(ctx)
	if mc == nil {
		return currentPrompt
	}
	if m.warnedInContext(ctx, mc) {
		return currentPrompt + "\n\n" + m.hint
	}
	return currentPrompt
}

func (m *ReplyCostBudgetMiddleware) spent(mc MiddleContext) float64 {
	if v, ok := mc.Get(m.Key(), "spent"); ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return 0
}

func (m *ReplyCostBudgetMiddleware) priceFor(modelName string) (model.Price, bool) {
	if p, ok := m.prices[modelName]; ok {
		return p, true
	}
	return model.ResolvePrice(modelName)
}
