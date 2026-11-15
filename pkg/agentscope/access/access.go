package access

import "context"

// ResourceKind identifies types of shareable resources.
type ResourceKind string

const (
	ResourceCredential    ResourceKind = "credential"
	ResourceAgent         ResourceKind = "agent"
	ResourceKnowledgeBase ResourceKind = "knowledge_base"
	ResourceSession       ResourceKind = "session"
)

// PermissionLevel defines levels of access for resource sharing.
type PermissionLevel string

const (
	PermissionNone  PermissionLevel = "none"
	PermissionRead  PermissionLevel = "read"
	PermissionWrite PermissionLevel = "write"
	PermissionAdmin PermissionLevel = "admin"
)

// permissionRank maps permission levels to numeric ranks for comparison.
var permissionRank = map[PermissionLevel]int{
	PermissionNone:  0,
	PermissionRead:  1,
	PermissionWrite: 2,
	PermissionAdmin: 3,
}

// PermissionSatisfies returns true if held >= required.
func PermissionSatisfies(held, required PermissionLevel) bool {
	return permissionRank[held] >= permissionRank[required]
}

// PrincipalType identifies the type of a principal.
type PrincipalType string

const (
	PrincipalUser  PrincipalType = "user"
	PrincipalGroup PrincipalType = "group"
	PrincipalOrg   PrincipalType = "org"
)

// Principal represents a user, group, or organization that can own/access resources.
type Principal struct {
	ID   string        `json:"id"`
	Type PrincipalType `json:"type"`
	Name string        `json:"name"`
}

// Grant represents a permission grant on a resource to a principal.
type Grant struct {
	ResourceKind ResourceKind    `json:"resource_kind"`
	ResourceID   string          `json:"resource_id"`
	Principal    Principal       `json:"principal"`
	Permission   PermissionLevel `json:"permission"`
}

// Policy defines the access control policy for a resource.
type Policy struct {
	ResourceKind ResourceKind `json:"resource_kind"`
	ResourceID   string       `json:"resource_id"`
	OwnerID      string       `json:"owner_id"`
	Grants       []Grant      `json:"grants"`
}

// Store persists access policies.
type Store interface {
	GetPolicy(ctx context.Context, kind ResourceKind, resourceID string) (*Policy, error)
	SetPolicy(ctx context.Context, policy *Policy) error
	DeletePolicy(ctx context.Context, kind ResourceKind, resourceID string) error
	ListPolicies(ctx context.Context, opts ListPoliciesOptions) ([]Policy, error)
}

// ListPoliciesOptions filters policies when listing.
type ListPoliciesOptions struct {
	OwnerID      string
	PrincipalID  string
	ResourceKind ResourceKind
}
