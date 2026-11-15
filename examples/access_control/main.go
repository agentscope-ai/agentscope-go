package main

import (
	"context"
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/access"
)

// This example demonstrates the access control system: creating policies,
// sharing resources, checking permissions, and revoking access.

func main() {
	ctx := context.Background()

	// Create an in-memory policy store and manager.
	store := access.NewInMemoryStore()
	mgr := access.NewManager(store)

	// Alice owns a credential resource "api-key-1".
	// Share creates the policy with alice as the owner.
	bob := access.Principal{ID: "bob", Type: access.PrincipalUser, Name: "Bob"}
	devTeam := access.Principal{ID: "dev-team", Type: access.PrincipalGroup, Name: "Dev Team"}

	fmt.Println("=== Setup: alice owns 'api-key-1' ===")

	// Share with bob at Read level.
	err := mgr.Share(ctx, "alice", access.ResourceCredential, "api-key-1", bob, access.PermissionRead)
	if err != nil {
		fmt.Println("share with bob:", err)
		return
	}
	fmt.Println("  Shared with bob at Read level")

	// Share with dev-team at Write level.
	err = mgr.Share(ctx, "alice", access.ResourceCredential, "api-key-1", devTeam, access.PermissionWrite)
	if err != nil {
		fmt.Println("share with dev-team:", err)
		return
	}
	fmt.Println("  Shared with dev-team at Write level")

	// Permission checks.
	checker := mgr.Checker()

	fmt.Println("\n=== Permission Checks ===")
	checks := []struct {
		who      string
		level    access.PermissionLevel
		expected bool
	}{
		{"bob", access.PermissionRead, true},
		{"bob", access.PermissionWrite, false},
		{"alice", access.PermissionAdmin, true},
		{"dev-team", access.PermissionWrite, true},
		{"dev-team", access.PermissionAdmin, false},
	}
	for _, c := range checks {
		ok, err := checker.Check(ctx, c.who, access.ResourceCredential, "api-key-1", c.level)
		if err != nil {
			fmt.Printf("  check error: %v\n", err)
			continue
		}
		status := "NO"
		if ok {
			status = "YES"
		}
		fmt.Printf("  Can %-10s %-6s api-key-1?  %s\n", c.who, c.level, status)
	}

	// List accessible resources for bob.
	fmt.Println("\n=== Accessible Resources for bob (Read+) ===")
	ids, err := checker.ListAccessible(ctx, "bob", access.ResourceCredential, access.PermissionRead)
	if err != nil {
		fmt.Println("  list error:", err)
	} else if len(ids) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, id := range ids {
			fmt.Printf("  - %s\n", id)
		}
	}

	// Show who has access.
	fmt.Println("\n=== Shared With ===")
	grants, err := mgr.GetSharedWith(ctx, access.ResourceCredential, "api-key-1")
	if err != nil {
		fmt.Println("  error:", err)
	} else {
		for _, g := range grants {
			fmt.Printf("  %s (%s) -> %s\n", g.Principal.Name, g.Principal.Type, g.Permission)
		}
	}

	// Revoke bob's access.
	fmt.Println("\n=== Revoke bob's access ===")
	err = mgr.Revoke(ctx, "alice", access.ResourceCredential, "api-key-1", "bob")
	if err != nil {
		fmt.Println("  revoke error:", err)
		return
	}
	fmt.Println("  Revoked bob's access")

	// Re-check bob.
	fmt.Println("\n=== After Revocation ===")
	ok, err := checker.Check(ctx, "bob", access.ResourceCredential, "api-key-1", access.PermissionRead)
	if err != nil {
		fmt.Println("  check error:", err)
	} else {
		status := "NO"
		if ok {
			status = "YES"
		}
		fmt.Printf("  Can bob read api-key-1?  %s\n", status)
	}

	// Verify bob no longer appears in accessible list.
	ids, err = checker.ListAccessible(ctx, "bob", access.ResourceCredential, access.PermissionRead)
	if err != nil {
		fmt.Println("  list error:", err)
	} else {
		fmt.Printf("  Bob's accessible credentials: %d\n", len(ids))
	}

	fmt.Println("\n=== Done ===")
}
