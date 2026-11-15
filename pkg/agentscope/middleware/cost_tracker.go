package middleware

import (
	"context"
	"fmt"
	"sync"

	agenterrors "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// ModelPrice defines the per-million-token pricing for a model. All fields are
// in USD per one million tokens.
type ModelPrice struct {
	InputPerMillion  float64
	OutputPerMillion float64
	CacheReadPerM    float64
	CacheWritePerM   float64
}

// ModelCost is the accumulated usage and cost attributed to a single model.
type ModelCost struct {
	InputTokens  int
	OutputTokens int
	CacheTokens  int
	CostUSD      float64
}

// TurnCost is the accumulated usage and cost for a single logical turn.
type TurnCost struct {
	Turn         int
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

// SessionCost is a snapshot of all usage and cost tracked across a session.
type SessionCost struct {
	TotalInputTokens  int
	TotalOutputTokens int
	TotalCacheTokens  int
	TotalCostUSD      float64
	ByModel           map[string]ModelCost
	ByTurn            []TurnCost

	// ConvertedCosts maps currency codes to the total cost expressed in
	// that currency, using the exchange rates supplied via WithExchangeRate.
	// For example, if WithExchangeRate("CNY", 7.2) was set and TotalCostUSD
	// is 1.0, ConvertedCosts["CNY"] will be 7.2.
	ConvertedCosts map[string]float64 `json:"converted_costs,omitempty"`
}

// ExchangeRate maps a currency code to its USD conversion factor.
// For example, {"CNY": 7.2} means 1 USD = 7.2 CNY.
type ExchangeRate struct {
	Currency string
	Rate     float64 // units of this currency per 1 USD
}

// CostTrackerMiddleware accumulates token usage and cost across every model call
// for the lifetime of the middleware instance (i.e. across turns), in contrast
// to ReplyBudgetControlMiddleware which resets per reply. It is safe for
// concurrent model calls.
//
// When maxCostUSD > 0, the middleware returns errors.ErrBudgetExceeded and
// stops all further model calls once the accumulated cost reaches the limit.
type CostTrackerMiddleware struct {
	BaseMiddleware

	mu          sync.Mutex
	prices      map[string]ModelPrice
	totalInput  int
	totalOutput int
	totalCache  int
	totalCost   float64
	byModel     map[string]*ModelCost
	byTurn      []*TurnCost
	currentTurn *TurnCost

	// maxCostUSD guards already-accounted cost. When > 0, subsequent calls
	// are rejected once that cost reaches the limit. In-flight calls are
	// not reserved and may overshoot it.
	maxCostUSD float64

	// exchangeRates provides optional currency conversion for display via
	// GetSessionCost(). Internally all cost is tracked in USD.
	exchangeRates []ExchangeRate
}

// CostTrackerOption configures a CostTrackerMiddleware.
type CostTrackerOption func(*CostTrackerMiddleware)

// WithMaxCostUSD sets a threshold for already-accounted cost in US dollars.
// Subsequent calls return ErrBudgetExceeded once that cost reaches the limit.
// Individual or concurrent calls can overshoot; no cost is reserved in advance.
// Missing usage or prices prevent complete cost accounting.
func WithMaxCostUSD(maxUSD float64) CostTrackerOption {
	return func(m *CostTrackerMiddleware) { m.maxCostUSD = maxUSD }
}

// WithExchangeRate adds a currency conversion rate for display purposes.
// The rate is expressed as units-of-currency per 1 USD.
// For example: WithExchangeRate("CNY", 7.2) means 1 USD = 7.2 CNY.
func WithExchangeRate(currency string, rate float64) CostTrackerOption {
	return func(m *CostTrackerMiddleware) {
		m.exchangeRates = append(m.exchangeRates, ExchangeRate{Currency: currency, Rate: rate})
	}
}

// NewCostTrackerMiddleware creates a cost tracker with the given per-model
// pricing table, keyed by model name. Model calls whose model name is absent
// from the table still contribute to token totals but incur zero cost.
func NewCostTrackerMiddleware(prices map[string]ModelPrice, opts ...CostTrackerOption) *CostTrackerMiddleware {
	p := make(map[string]ModelPrice, len(prices))
	for k, v := range prices {
		p[k] = v
	}
	ct := &CostTrackerMiddleware{
		BaseMiddleware: BaseMiddleware{MiddlewareKey: "cost_tracker"},
		prices:         p,
		byModel:        make(map[string]*ModelCost),
	}
	for _, o := range opts {
		o(ct)
	}
	return ct
}

// NewTurn starts a new turn for per-turn tracking. Subsequent model calls are
// attributed to this turn until NewTurn is called again.
func (m *CostTrackerMiddleware) NewTurn() {
	m.mu.Lock()
	defer m.mu.Unlock()
	turn := &TurnCost{Turn: len(m.byTurn) + 1}
	m.byTurn = append(m.byTurn, turn)
	m.currentTurn = turn
}

// OnModelCall records the token usage of each model call and accumulates cost.
// If maxCostUSD > 0 and the accumulated cost has already reached the limit,
// the call is rejected immediately with ErrBudgetExceeded.
func (m *CostTrackerMiddleware) OnModelCall(
	ctx context.Context,
	input *ModelCallInput,
	next ModelCallHandler,
) (*model.ChatResponse, error) {
	// Pre-flight budget check: reject before calling the model.
	m.mu.Lock()
	maxCost := m.maxCostUSD
	m.mu.Unlock()
	if maxCost > 0 {
		m.mu.Lock()
		exceeded := m.totalCost >= maxCost
		current := m.totalCost
		m.mu.Unlock()
		if exceeded {
			return nil, &agenterrors.AgentError{
				Category:  agenterrors.CategoryResource,
				Code:      "budget.exceeded",
				Message:   fmt.Sprintf("spend cap reached: $%.4f >= $%.4f", current, maxCost),
				Retryable: false,
			}
		}
	}

	resp, err := next(ctx, input)
	if err != nil {
		return resp, err
	}
	if resp == nil || resp.Usage == nil {
		return resp, nil
	}

	modelName := resp.ModelName
	if modelName == "" {
		modelName = input.ModelName
	}

	m.record(modelName, resp.Usage)
	return resp, nil
}

func (m *CostTrackerMiddleware) record(modelName string, usage *model.ChatUsage) {
	cacheTokens := usage.CacheInputTokens + usage.CacheCreationInputTokens

	price := m.prices[modelName]
	cost := float64(usage.InputTokens)*price.InputPerMillion/1e6 +
		float64(usage.OutputTokens)*price.OutputPerMillion/1e6 +
		float64(usage.CacheInputTokens)*price.CacheReadPerM/1e6 +
		float64(usage.CacheCreationInputTokens)*price.CacheWritePerM/1e6

	m.mu.Lock()
	defer m.mu.Unlock()

	m.totalInput += usage.InputTokens
	m.totalOutput += usage.OutputTokens
	m.totalCache += cacheTokens
	m.totalCost += cost

	mc := m.byModel[modelName]
	if mc == nil {
		mc = &ModelCost{}
		m.byModel[modelName] = mc
	}
	mc.InputTokens += usage.InputTokens
	mc.OutputTokens += usage.OutputTokens
	mc.CacheTokens += cacheTokens
	mc.CostUSD += cost

	if m.currentTurn != nil {
		m.currentTurn.InputTokens += usage.InputTokens
		m.currentTurn.OutputTokens += usage.OutputTokens
		m.currentTurn.CostUSD += cost
	}
}

// GetSessionCost returns a snapshot of all accumulated usage and cost. The
// returned maps and slices are copies, safe to read without holding the lock.
func (m *CostTrackerMiddleware) GetSessionCost() SessionCost {
	m.mu.Lock()
	defer m.mu.Unlock()

	byModel := make(map[string]ModelCost, len(m.byModel))
	for name, c := range m.byModel {
		byModel[name] = *c
	}

	byTurn := make([]TurnCost, len(m.byTurn))
	for i, t := range m.byTurn {
		byTurn[i] = *t
	}

	sc := SessionCost{
		TotalInputTokens:  m.totalInput,
		TotalOutputTokens: m.totalOutput,
		TotalCacheTokens:  m.totalCache,
		TotalCostUSD:      m.totalCost,
		ByModel:           byModel,
		ByTurn:            byTurn,
	}

	if len(m.exchangeRates) > 0 {
		sc.ConvertedCosts = make(map[string]float64, len(m.exchangeRates))
		for _, er := range m.exchangeRates {
			sc.ConvertedCosts[er.Currency] = m.totalCost * er.Rate
		}
	}

	return sc
}
