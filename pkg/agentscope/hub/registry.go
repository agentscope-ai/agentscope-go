package hub

import (
	"context"
	"fmt"
	"sync"
)

// Registry manages multiple Hub instances and provides unified search.
type Registry struct {
	mu   sync.RWMutex
	hubs map[string]Hub
}

// NewRegistry creates a new empty hub registry.
func NewRegistry() *Registry {
	return &Registry{
		hubs: make(map[string]Hub),
	}
}

// Register adds a hub to the registry.
func (r *Registry) Register(h Hub) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id := h.ID()
	if _, exists := r.hubs[id]; exists {
		return fmt.Errorf("hub: registry already contains hub %q", id)
	}
	r.hubs[id] = h
	return nil
}

// Unregister removes a hub from the registry by ID.
func (r *Registry) Unregister(hubID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.hubs[hubID]; !exists {
		return fmt.Errorf("hub: registry does not contain hub %q", hubID)
	}
	delete(r.hubs, hubID)
	return nil
}

// Get retrieves a hub by ID.
func (r *Registry) Get(hubID string) (Hub, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	h, ok := r.hubs[hubID]
	return h, ok
}

// List returns all registered hubs.
func (r *Registry) List() []Hub {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]Hub, 0, len(r.hubs))
	for _, h := range r.hubs {
		result = append(result, h)
	}
	return result
}

// SearchAll queries all registered hubs and returns results keyed by hub ID.
func (r *Registry) SearchAll(ctx context.Context, opts *ListOptions) (map[string]*ListResult, error) {
	r.mu.RLock()
	hubs := make([]Hub, 0, len(r.hubs))
	for _, h := range r.hubs {
		hubs = append(hubs, h)
	}
	r.mu.RUnlock()

	type searchResult struct {
		hubID  string
		result *ListResult
		err    error
	}

	ch := make(chan searchResult, len(hubs))
	var wg sync.WaitGroup

	for _, h := range hubs {
		wg.Add(1)
		go func(hub Hub) {
			defer wg.Done()
			res, err := hub.List(ctx, opts)
			ch <- searchResult{hubID: hub.ID(), result: res, err: err}
		}(h)
	}

	wg.Wait()
	close(ch)

	results := make(map[string]*ListResult, len(hubs))
	var firstErr error
	for sr := range ch {
		if sr.err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("hub %q: %w", sr.hubID, sr.err)
			}
			continue
		}
		results[sr.hubID] = sr.result
	}

	if len(results) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

// Close closes all registered hubs and clears the registry.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var firstErr error
	for id, h := range r.hubs {
		if err := h.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("hub %q: %w", id, err)
		}
	}
	r.hubs = make(map[string]Hub)
	return firstErr
}
