package middleware

import (
	"fmt"
	"math"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"
)

// NewCostLedgerFromAttempts creates a query projection of the canonical managed
// attempt ledger. Logical Record/CostTracking entries do not contribute in this
// mode. TotalCostUSD is a known subtotal; inspect unknown/incomplete fields.
func NewCostLedgerFromAttempts(source *inference.Ledger) (*CostLedger, error) {
	if source == nil {
		return nil, fmt.Errorf("middleware: nil attempt ledger")
	}
	return &CostLedger{attempts: source}, nil
}
func (l *CostLedger) inferenceSummary(filter CostFilter) CostSummary {
	snapshot := l.attempts.Snapshot()
	result := CostSummary{ByModel: make(map[string]float64), ByAgent: make(map[string]float64), Dropped: snapshot.Dropped, Incomplete: snapshot.Dropped > 0}
	// Queued operations have no physical attempt yet, but their filtered cost
	// observation is still unfinished. Do not label them settled zero-cost work.
	for i := range snapshot.Operations {
		op := &snapshot.Operations[i]
		if !op.FinishedAt.IsZero() || filter.SessionID != "" && op.Identity.SessionID != filter.SessionID || filter.AgentName != "" && op.Identity.AgentName != filter.AgentName || filter.ModelName != "" && op.Model != filter.ModelName || !filter.Since.IsZero() && op.StartedAt.Before(filter.Since) {
			continue
		}
		result.Incomplete = true
	}
	addCost := func(current, amount float64) float64 {
		next := current + amount
		if math.IsInf(next, 0) || math.IsNaN(next) {
			result.CostOverflow = true
			result.Incomplete = true
			return current
		}
		return next
	}
	addTokens := func(current int, amount int64) int {
		if amount < 0 || amount > int64(math.MaxInt-current) {
			result.TokenOverflow = true
			result.Incomplete = true
			return current
		}
		return current + int(amount)
	}
	for i := range snapshot.Attempts {
		a := &snapshot.Attempts[i]
		if filter.SessionID != "" && a.Identity.SessionID != filter.SessionID || filter.AgentName != "" && a.Identity.AgentName != filter.AgentName || filter.ModelName != "" && a.Model != filter.ModelName || !filter.Since.IsZero() && a.StartedAt.Before(filter.Since) {
			continue
		}
		result.Calls++
		if a.FinishedAt.IsZero() || a.UsageKnown && !a.UsageComplete {
			result.Incomplete = true
		}
		if !a.UsageKnown || !a.UsageComplete {
			result.UnknownUsage++
		}
		if !a.CostKnown || !a.UsageComplete {
			result.UnknownCost++
		}
		if a.Usage.InputKnown {
			result.TotalInTokens = addTokens(result.TotalInTokens, a.Usage.InputTokens)
			result.TotalCacheReadTokens = addTokens(result.TotalCacheReadTokens, a.Usage.CacheReadTokens)
			result.TotalCacheWriteTokens = addTokens(result.TotalCacheWriteTokens, a.Usage.CacheWriteTokens)
		}
		if a.Usage.OutputKnown {
			result.TotalOutTokens = addTokens(result.TotalOutTokens, a.Usage.OutputTokens)
		}
		if a.CostKnown {
			result.TotalCostUSD = addCost(result.TotalCostUSD, a.CostUSD)
			result.ByModel[a.Model] = addCost(result.ByModel[a.Model], a.CostUSD)
			result.ByAgent[a.Identity.AgentName] = addCost(result.ByAgent[a.Identity.AgentName], a.CostUSD)
		}
	}
	return result
}
