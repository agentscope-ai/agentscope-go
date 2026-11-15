package access

import (
	"context"
	"fmt"
	"sync"
)

// InMemoryStore is a simple thread-safe in-memory policy store.
type InMemoryStore struct {
	mu       sync.RWMutex
	policies map[string]*Policy // key: "kind:resourceID"
}

// NewInMemoryStore creates a new InMemoryStore.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		policies: make(map[string]*Policy),
	}
}

func policyKey(kind ResourceKind, resourceID string) string {
	return fmt.Sprintf("%s:%s", kind, resourceID)
}

// GetPolicy retrieves the policy for the given resource.
func (s *InMemoryStore) GetPolicy(_ context.Context, kind ResourceKind, resourceID string) (*Policy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := policyKey(kind, resourceID)
	p, ok := s.policies[key]
	if !ok {
		return nil, fmt.Errorf("policy not found for %s", key)
	}
	// Return a copy to avoid races.
	cp := *p
	cp.Grants = make([]Grant, len(p.Grants))
	copy(cp.Grants, p.Grants)
	return &cp, nil
}

// SetPolicy stores (creates or updates) a policy.
func (s *InMemoryStore) SetPolicy(_ context.Context, policy *Policy) error {
	if policy == nil {
		return fmt.Errorf("policy must not be nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := policyKey(policy.ResourceKind, policy.ResourceID)
	cp := *policy
	cp.Grants = make([]Grant, len(policy.Grants))
	copy(cp.Grants, policy.Grants)
	s.policies[key] = &cp
	return nil
}

// DeletePolicy removes the policy for the given resource.
func (s *InMemoryStore) DeletePolicy(_ context.Context, kind ResourceKind, resourceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := policyKey(kind, resourceID)
	if _, ok := s.policies[key]; !ok {
		return fmt.Errorf("policy not found for %s", key)
	}
	delete(s.policies, key)
	return nil
}

// ListPolicies returns policies matching the given filter options.
func (s *InMemoryStore) ListPolicies(_ context.Context, opts ListPoliciesOptions) ([]Policy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []Policy
	for _, p := range s.policies {
		if opts.ResourceKind != "" && p.ResourceKind != opts.ResourceKind {
			continue
		}
		if opts.OwnerID != "" && p.OwnerID != opts.OwnerID {
			continue
		}
		if opts.PrincipalID != "" {
			found := false
			for _, g := range p.Grants {
				if g.Principal.ID == opts.PrincipalID {
					found = true
					break
				}
			}
			if !found && p.OwnerID != opts.PrincipalID {
				continue
			}
		}
		cp := *p
		cp.Grants = make([]Grant, len(p.Grants))
		copy(cp.Grants, p.Grants)
		results = append(results, cp)
	}
	return results, nil
}
