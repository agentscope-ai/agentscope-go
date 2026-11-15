package middleware

import "context"

type ctxKey struct{}

// MiddleContext is a key-value store for middleware to persist state across
// hook invocations within a single reply. Each middleware uses its Key() as
// the top-level namespace.
type MiddleContext map[string]any

// WithMiddleContext attaches a MiddleContext to a Go context.
func WithMiddleContext(ctx context.Context, mc MiddleContext) context.Context {
	return context.WithValue(ctx, ctxKey{}, mc)
}

// GetMiddleContext retrieves the MiddleContext from a Go context.
// Returns nil if none is attached.
func GetMiddleContext(ctx context.Context) MiddleContext {
	mc, _ := ctx.Value(ctxKey{}).(MiddleContext)
	return mc
}

// Get retrieves a value from MiddleContext by middleware key and field.
func (mc MiddleContext) Get(middlewareKey, field string) (any, bool) {
	if mc == nil {
		return nil, false
	}
	ns, ok := mc[middlewareKey]
	if !ok {
		return nil, false
	}
	m, ok := ns.(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := m[field]
	return v, ok
}

// Set stores a value in MiddleContext under the middleware's namespace.
func (mc MiddleContext) Set(middlewareKey, field string, value any) {
	if mc == nil {
		return
	}
	ns, ok := mc[middlewareKey]
	if !ok {
		ns = map[string]any{}
		mc[middlewareKey] = ns
	}
	m, ok := ns.(map[string]any)
	if !ok {
		m = map[string]any{}
		mc[middlewareKey] = m
	}
	m[field] = value
}
