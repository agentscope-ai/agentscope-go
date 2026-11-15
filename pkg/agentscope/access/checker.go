package access

import "context"

// Checker evaluates whether a principal has the required permission on a resource.
type Checker struct {
	store Store
}

// NewChecker creates a Checker backed by the given store.
func NewChecker(store Store) *Checker {
	return &Checker{store: store}
}

// Check returns whether the given principal has at least the required permission level.
func (c *Checker) Check(ctx context.Context, principalID string, kind ResourceKind, resourceID string, required PermissionLevel) (bool, error) {
	policy, err := c.store.GetPolicy(ctx, kind, resourceID)
	if err != nil {
		return false, err
	}

	// Owner always has full access.
	if policy.OwnerID == principalID {
		return true, nil
	}

	// Check direct grants to the principal.
	for _, g := range policy.Grants {
		if g.Principal.ID == principalID {
			if PermissionSatisfies(g.Permission, required) {
				return true, nil
			}
		}
	}

	return false, nil
}

// ListAccessible returns all resource IDs of the given kind that the principal
// can access with at least the required permission level.
func (c *Checker) ListAccessible(ctx context.Context, principalID string, kind ResourceKind, required PermissionLevel) ([]string, error) {
	policies, err := c.store.ListPolicies(ctx, ListPoliciesOptions{
		ResourceKind: kind,
	})
	if err != nil {
		return nil, err
	}

	var ids []string
	for _, p := range policies {
		if p.OwnerID == principalID {
			ids = append(ids, p.ResourceID)
			continue
		}
		for _, g := range p.Grants {
			if g.Principal.ID == principalID && PermissionSatisfies(g.Permission, required) {
				ids = append(ids, p.ResourceID)
				break
			}
		}
	}
	return ids, nil
}
