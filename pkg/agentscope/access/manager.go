package access

import (
	"context"
	"fmt"
)

// Manager provides high-level resource sharing operations.
type Manager struct {
	store   Store
	checker *Checker
}

// NewManager creates a Manager backed by the given store.
func NewManager(store Store) *Manager {
	return &Manager{
		store:   store,
		checker: NewChecker(store),
	}
}

// Checker returns the underlying permission checker.
func (m *Manager) Checker() *Checker {
	return m.checker
}

// Share grants a permission on a resource to a principal.
// Only the resource owner can share it.
func (m *Manager) Share(ctx context.Context, ownerID string, kind ResourceKind, resourceID string, target Principal, perm PermissionLevel) error {
	policy, err := m.store.GetPolicy(ctx, kind, resourceID)
	if err != nil {
		// If no policy exists, create one with the caller as owner.
		policy = &Policy{
			ResourceKind: kind,
			ResourceID:   resourceID,
			OwnerID:      ownerID,
		}
	}

	if policy.OwnerID != ownerID {
		return fmt.Errorf("only the owner can share this resource")
	}

	// Update existing grant or append a new one.
	found := false
	for i, g := range policy.Grants {
		if g.Principal.ID == target.ID {
			policy.Grants[i].Permission = perm
			found = true
			break
		}
	}
	if !found {
		policy.Grants = append(policy.Grants, Grant{
			ResourceKind: kind,
			ResourceID:   resourceID,
			Principal:    target,
			Permission:   perm,
		})
	}

	return m.store.SetPolicy(ctx, policy)
}

// Revoke removes a principal's access to a resource.
// Only the resource owner can revoke access.
func (m *Manager) Revoke(ctx context.Context, ownerID string, kind ResourceKind, resourceID string, targetID string) error {
	policy, err := m.store.GetPolicy(ctx, kind, resourceID)
	if err != nil {
		return err
	}

	if policy.OwnerID != ownerID {
		return fmt.Errorf("only the owner can revoke access to this resource")
	}

	grants := make([]Grant, 0, len(policy.Grants))
	for _, g := range policy.Grants {
		if g.Principal.ID != targetID {
			grants = append(grants, g)
		}
	}
	policy.Grants = grants

	return m.store.SetPolicy(ctx, policy)
}

// TransferOwnership changes the owner of a resource.
// Only the current owner can transfer ownership.
func (m *Manager) TransferOwnership(ctx context.Context, currentOwnerID string, kind ResourceKind, resourceID string, newOwnerID string) error {
	policy, err := m.store.GetPolicy(ctx, kind, resourceID)
	if err != nil {
		return err
	}

	if policy.OwnerID != currentOwnerID {
		return fmt.Errorf("only the current owner can transfer ownership")
	}

	policy.OwnerID = newOwnerID

	// Remove any existing grant to the new owner (they now have full access as owner).
	grants := make([]Grant, 0, len(policy.Grants))
	for _, g := range policy.Grants {
		if g.Principal.ID != newOwnerID {
			grants = append(grants, g)
		}
	}
	policy.Grants = grants

	return m.store.SetPolicy(ctx, policy)
}

// GetSharedWith returns all principals that have access to a resource.
func (m *Manager) GetSharedWith(ctx context.Context, kind ResourceKind, resourceID string) ([]Grant, error) {
	policy, err := m.store.GetPolicy(ctx, kind, resourceID)
	if err != nil {
		return nil, err
	}

	result := make([]Grant, len(policy.Grants))
	copy(result, policy.Grants)
	return result, nil
}
