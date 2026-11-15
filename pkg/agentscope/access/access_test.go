package access

import (
	"context"
	"testing"
)

// ---------- PermissionSatisfies Tests ----------

func TestPermissionSatisfies(t *testing.T) {
	tests := []struct {
		held     PermissionLevel
		required PermissionLevel
		want     bool
	}{
		{PermissionAdmin, PermissionRead, true},
		{PermissionAdmin, PermissionWrite, true},
		{PermissionAdmin, PermissionAdmin, true},
		{PermissionWrite, PermissionRead, true},
		{PermissionWrite, PermissionWrite, true},
		{PermissionWrite, PermissionAdmin, false},
		{PermissionRead, PermissionRead, true},
		{PermissionRead, PermissionWrite, false},
		{PermissionRead, PermissionAdmin, false},
		{PermissionNone, PermissionRead, false},
		{PermissionNone, PermissionNone, true},
	}

	for _, tt := range tests {
		t.Run(string(tt.held)+"_satisfies_"+string(tt.required), func(t *testing.T) {
			got := PermissionSatisfies(tt.held, tt.required)
			if got != tt.want {
				t.Errorf("PermissionSatisfies(%s, %s) = %v, want %v", tt.held, tt.required, got, tt.want)
			}
		})
	}
}

// ---------- InMemoryStore Tests ----------

func TestInMemoryStore_SetAndGetPolicy(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	policy := &Policy{
		ResourceKind: ResourceAgent,
		ResourceID:   "agent-1",
		OwnerID:      "user-alice",
		Grants: []Grant{
			{ResourceKind: ResourceAgent, ResourceID: "agent-1", Principal: Principal{ID: "user-bob", Type: PrincipalUser}, Permission: PermissionRead},
		},
	}

	if err := store.SetPolicy(ctx, policy); err != nil {
		t.Fatalf("SetPolicy failed: %v", err)
	}

	got, err := store.GetPolicy(ctx, ResourceAgent, "agent-1")
	if err != nil {
		t.Fatalf("GetPolicy failed: %v", err)
	}
	if got.OwnerID != "user-alice" {
		t.Errorf("expected owner user-alice, got %s", got.OwnerID)
	}
	if len(got.Grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(got.Grants))
	}
	if got.Grants[0].Principal.ID != "user-bob" {
		t.Errorf("expected grant to user-bob, got %s", got.Grants[0].Principal.ID)
	}
}

func TestInMemoryStore_GetPolicy_NotFound(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	_, err := store.GetPolicy(ctx, ResourceAgent, "nonexistent")
	if err == nil {
		t.Fatal("expected error for non-existent policy")
	}
}

func TestInMemoryStore_DeletePolicy(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	policy := &Policy{
		ResourceKind: ResourceSession,
		ResourceID:   "sess-1",
		OwnerID:      "user-x",
	}
	store.SetPolicy(ctx, policy)

	if err := store.DeletePolicy(ctx, ResourceSession, "sess-1"); err != nil {
		t.Fatalf("DeletePolicy failed: %v", err)
	}

	_, err := store.GetPolicy(ctx, ResourceSession, "sess-1")
	if err == nil {
		t.Fatal("expected error after deletion")
	}
}

func TestInMemoryStore_DeletePolicy_NotFound(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	err := store.DeletePolicy(ctx, ResourceAgent, "nonexistent")
	if err == nil {
		t.Fatal("expected error deleting non-existent policy")
	}
}

func TestInMemoryStore_ListPolicies(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	store.SetPolicy(ctx, &Policy{ResourceKind: ResourceAgent, ResourceID: "a1", OwnerID: "alice"})
	store.SetPolicy(ctx, &Policy{ResourceKind: ResourceAgent, ResourceID: "a2", OwnerID: "bob"})
	store.SetPolicy(ctx, &Policy{ResourceKind: ResourceSession, ResourceID: "s1", OwnerID: "alice"})

	// Filter by kind.
	policies, err := store.ListPolicies(ctx, ListPoliciesOptions{ResourceKind: ResourceAgent})
	if err != nil {
		t.Fatalf("ListPolicies failed: %v", err)
	}
	if len(policies) != 2 {
		t.Errorf("expected 2 agent policies, got %d", len(policies))
	}

	// Filter by owner.
	policies, err = store.ListPolicies(ctx, ListPoliciesOptions{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("ListPolicies failed: %v", err)
	}
	if len(policies) != 2 {
		t.Errorf("expected 2 policies for alice, got %d", len(policies))
	}
}

// ---------- Checker Tests ----------

func TestChecker_Check_OwnerAlwaysPasses(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	store.SetPolicy(ctx, &Policy{
		ResourceKind: ResourceAgent,
		ResourceID:   "agent-1",
		OwnerID:      "owner-1",
	})

	checker := NewChecker(store)

	ok, err := checker.Check(ctx, "owner-1", ResourceAgent, "agent-1", PermissionAdmin)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if !ok {
		t.Error("expected owner to always pass")
	}
}

func TestChecker_Check_DirectGrantSufficient(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	store.SetPolicy(ctx, &Policy{
		ResourceKind: ResourceAgent,
		ResourceID:   "agent-1",
		OwnerID:      "owner-1",
		Grants: []Grant{
			{Principal: Principal{ID: "bob"}, Permission: PermissionWrite},
		},
	})

	checker := NewChecker(store)

	ok, err := checker.Check(ctx, "bob", ResourceAgent, "agent-1", PermissionRead)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if !ok {
		t.Error("expected write grant to satisfy read requirement")
	}
}

func TestChecker_Check_InsufficientLevel(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	store.SetPolicy(ctx, &Policy{
		ResourceKind: ResourceAgent,
		ResourceID:   "agent-1",
		OwnerID:      "owner-1",
		Grants: []Grant{
			{Principal: Principal{ID: "bob"}, Permission: PermissionRead},
		},
	})

	checker := NewChecker(store)

	ok, err := checker.Check(ctx, "bob", ResourceAgent, "agent-1", PermissionAdmin)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if ok {
		t.Error("expected read grant to NOT satisfy admin requirement")
	}
}

func TestChecker_Check_NoPolicyReturnsError(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	checker := NewChecker(store)

	_, err := checker.Check(ctx, "bob", ResourceAgent, "nonexistent", PermissionRead)
	if err == nil {
		t.Fatal("expected error when no policy exists")
	}
}

func TestChecker_ListAccessible(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	store.SetPolicy(ctx, &Policy{
		ResourceKind: ResourceAgent,
		ResourceID:   "agent-1",
		OwnerID:      "alice",
	})
	store.SetPolicy(ctx, &Policy{
		ResourceKind: ResourceAgent,
		ResourceID:   "agent-2",
		OwnerID:      "bob",
		Grants: []Grant{
			{Principal: Principal{ID: "alice"}, Permission: PermissionRead},
		},
	})
	store.SetPolicy(ctx, &Policy{
		ResourceKind: ResourceAgent,
		ResourceID:   "agent-3",
		OwnerID:      "bob",
	})

	checker := NewChecker(store)

	ids, err := checker.ListAccessible(ctx, "alice", ResourceAgent, PermissionRead)
	if err != nil {
		t.Fatalf("ListAccessible failed: %v", err)
	}

	idSet := map[string]bool{}
	for _, id := range ids {
		idSet[id] = true
	}

	if !idSet["agent-1"] {
		t.Error("expected alice to have access to owned agent-1")
	}
	if !idSet["agent-2"] {
		t.Error("expected alice to have access to granted agent-2")
	}
	if idSet["agent-3"] {
		t.Error("expected alice to NOT have access to agent-3")
	}
}

// ---------- Manager Tests ----------

func TestManager_Share_CreatesPolicy(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	mgr := NewManager(store)

	target := Principal{ID: "bob", Type: PrincipalUser, Name: "Bob"}
	err := mgr.Share(ctx, "alice", ResourceAgent, "agent-new", target, PermissionRead)
	if err != nil {
		t.Fatalf("Share failed: %v", err)
	}

	policy, err := store.GetPolicy(ctx, ResourceAgent, "agent-new")
	if err != nil {
		t.Fatalf("GetPolicy failed: %v", err)
	}
	if policy.OwnerID != "alice" {
		t.Errorf("expected owner alice, got %s", policy.OwnerID)
	}
	if len(policy.Grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(policy.Grants))
	}
	if policy.Grants[0].Principal.ID != "bob" || policy.Grants[0].Permission != PermissionRead {
		t.Errorf("unexpected grant: %+v", policy.Grants[0])
	}
}

func TestManager_Share_UpdatesExistingGrant(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	mgr := NewManager(store)

	target := Principal{ID: "bob", Type: PrincipalUser, Name: "Bob"}
	mgr.Share(ctx, "alice", ResourceAgent, "agent-1", target, PermissionRead)

	err := mgr.Share(ctx, "alice", ResourceAgent, "agent-1", target, PermissionWrite)
	if err != nil {
		t.Fatalf("Share update failed: %v", err)
	}

	policy, _ := store.GetPolicy(ctx, ResourceAgent, "agent-1")
	if len(policy.Grants) != 1 {
		t.Fatalf("expected 1 grant after update, got %d", len(policy.Grants))
	}
	if policy.Grants[0].Permission != PermissionWrite {
		t.Errorf("expected permission write, got %s", policy.Grants[0].Permission)
	}
}

func TestManager_Share_NonOwnerRejected(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	mgr := NewManager(store)

	store.SetPolicy(ctx, &Policy{
		ResourceKind: ResourceAgent,
		ResourceID:   "agent-1",
		OwnerID:      "alice",
	})

	target := Principal{ID: "charlie", Type: PrincipalUser}
	err := mgr.Share(ctx, "bob", ResourceAgent, "agent-1", target, PermissionRead)
	if err == nil {
		t.Fatal("expected error when non-owner tries to share")
	}
}

func TestManager_Revoke(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	mgr := NewManager(store)

	store.SetPolicy(ctx, &Policy{
		ResourceKind: ResourceAgent,
		ResourceID:   "agent-1",
		OwnerID:      "alice",
		Grants: []Grant{
			{Principal: Principal{ID: "bob"}, Permission: PermissionRead},
			{Principal: Principal{ID: "charlie"}, Permission: PermissionWrite},
		},
	})

	err := mgr.Revoke(ctx, "alice", ResourceAgent, "agent-1", "bob")
	if err != nil {
		t.Fatalf("Revoke failed: %v", err)
	}

	policy, _ := store.GetPolicy(ctx, ResourceAgent, "agent-1")
	if len(policy.Grants) != 1 {
		t.Fatalf("expected 1 grant after revoke, got %d", len(policy.Grants))
	}
	if policy.Grants[0].Principal.ID != "charlie" {
		t.Errorf("expected remaining grant for charlie, got %s", policy.Grants[0].Principal.ID)
	}
}

func TestManager_Revoke_NonOwnerRejected(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	mgr := NewManager(store)

	store.SetPolicy(ctx, &Policy{
		ResourceKind: ResourceAgent,
		ResourceID:   "agent-1",
		OwnerID:      "alice",
		Grants: []Grant{
			{Principal: Principal{ID: "bob"}, Permission: PermissionRead},
		},
	})

	err := mgr.Revoke(ctx, "bob", ResourceAgent, "agent-1", "bob")
	if err == nil {
		t.Fatal("expected error when non-owner tries to revoke")
	}
}

func TestManager_TransferOwnership(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	mgr := NewManager(store)

	store.SetPolicy(ctx, &Policy{
		ResourceKind: ResourceAgent,
		ResourceID:   "agent-1",
		OwnerID:      "alice",
		Grants: []Grant{
			{Principal: Principal{ID: "bob"}, Permission: PermissionWrite},
			{Principal: Principal{ID: "charlie"}, Permission: PermissionRead},
		},
	})

	err := mgr.TransferOwnership(ctx, "alice", ResourceAgent, "agent-1", "bob")
	if err != nil {
		t.Fatalf("TransferOwnership failed: %v", err)
	}

	policy, _ := store.GetPolicy(ctx, ResourceAgent, "agent-1")
	if policy.OwnerID != "bob" {
		t.Errorf("expected new owner bob, got %s", policy.OwnerID)
	}
	if len(policy.Grants) != 1 {
		t.Fatalf("expected 1 grant (charlie only), got %d", len(policy.Grants))
	}
	if policy.Grants[0].Principal.ID != "charlie" {
		t.Errorf("expected remaining grant for charlie, got %s", policy.Grants[0].Principal.ID)
	}
}

func TestManager_TransferOwnership_NonOwnerRejected(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	mgr := NewManager(store)

	store.SetPolicy(ctx, &Policy{
		ResourceKind: ResourceAgent,
		ResourceID:   "agent-1",
		OwnerID:      "alice",
	})

	err := mgr.TransferOwnership(ctx, "bob", ResourceAgent, "agent-1", "charlie")
	if err == nil {
		t.Fatal("expected error when non-owner tries to transfer ownership")
	}
}

func TestManager_GetSharedWith(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	mgr := NewManager(store)

	grants := []Grant{
		{Principal: Principal{ID: "bob"}, Permission: PermissionRead},
		{Principal: Principal{ID: "charlie"}, Permission: PermissionWrite},
	}
	store.SetPolicy(ctx, &Policy{
		ResourceKind: ResourceAgent,
		ResourceID:   "agent-1",
		OwnerID:      "alice",
		Grants:       grants,
	})

	result, err := mgr.GetSharedWith(ctx, ResourceAgent, "agent-1")
	if err != nil {
		t.Fatalf("GetSharedWith failed: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 grants, got %d", len(result))
	}

	// Verify it is a copy (modifying result does not affect store).
	result[0].Permission = PermissionAdmin
	original, _ := store.GetPolicy(ctx, ResourceAgent, "agent-1")
	if original.Grants[0].Permission == PermissionAdmin {
		t.Error("GetSharedWith should return a copy, not a reference")
	}
}
