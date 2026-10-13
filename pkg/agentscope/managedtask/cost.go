package managedtask

func (e *Executor) cost(attempts []Attempt) Cost {
	total := Cost{}
	p := e.cfg.Prices
	if p != nil {
		total.Currency = p.Currency
	}
	for i := range attempts {
		a := &attempts[i]
		if p == nil || a.Usage == nil || a.Model != e.cfg.Deployment.Model || !a.Completed {
			total.UnknownAttempts++
			continue
		}
		u := a.Usage
		unknown := false
		subtotal := 0.0
		add := func(tokens *int64, rate *float64) {
			if tokens == nil || rate == nil {
				unknown = true
				return
			}
			value := float64(*tokens) * *rate
			if !finite(value) || !finite(subtotal+value) {
				unknown = true
				return
			}
			subtotal += value
		}
		if p.CachedInputPerToken != nil {
			if u.InputTokens == nil || u.CachedInputTokens == nil {
				unknown = true
			} else {
				uncached := *u.InputTokens - *u.CachedInputTokens
				add(&uncached, p.InputPerToken)
				add(u.CachedInputTokens, p.CachedInputPerToken)
			}
		} else {
			add(u.InputTokens, p.InputPerToken)
		}
		add(u.OutputTokens, p.OutputPerToken)
		if finite(total.KnownSubtotal + subtotal) {
			total.KnownSubtotal += subtotal
		} else {
			unknown = true
		}
		if unknown {
			total.UnknownAttempts++
		}
	}
	return total
}
