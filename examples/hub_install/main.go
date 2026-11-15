package main

import (
	"fmt"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/hub"
)

// This example demonstrates the Hub System: creating a Registry and
// registering MCP and Skill hubs. Since we cannot connect to real
// hub servers, we show the types and Registry operations only.

func main() {
	// Create a hub registry.
	reg := hub.NewRegistry()

	// Register an MCP hub (pointing to a fake URL for demonstration).
	mcpHub := hub.NewMCPHub(hub.MCPHubConfig{
		BaseURL:     "https://hub.example.com",
		APIKey:      "demo-key",
		HubID:       "mcp-community",
		DisplayName: "Community MCP Servers",
	})
	if err := reg.Register(mcpHub); err != nil {
		fmt.Println("register mcp hub:", err)
		return
	}

	// Register a skill hub.
	skillHub := hub.NewSkillHub(hub.SkillHubConfig{
		BaseURL:     "https://skills.example.com",
		APIKey:      "demo-key",
		HubID:       "skill-marketplace",
		DisplayName: "Skill Marketplace",
	})
	if err := reg.Register(skillHub); err != nil {
		fmt.Println("register skill hub:", err)
		return
	}

	// List all registered hubs.
	fmt.Println("=== Registered Hubs ===")
	for _, h := range reg.List() {
		fmt.Printf("  ID: %-20s  Name: %s\n", h.ID(), h.DisplayName())
	}

	// Look up a hub by ID.
	fmt.Println("\n=== Lookup by ID ===")
	if h, ok := reg.Get("mcp-community"); ok {
		fmt.Printf("  Found hub: %s (%s)\n", h.ID(), h.DisplayName())
	}
	if _, ok := reg.Get("nonexistent"); !ok {
		fmt.Println("  Hub 'nonexistent' not found (expected)")
	}

	// Demonstrate that duplicate registration is rejected.
	fmt.Println("\n=== Duplicate Registration ===")
	err := reg.Register(mcpHub)
	if err != nil {
		fmt.Printf("  Correctly rejected: %v\n", err)
	}

	// Explain Hub interface methods.
	fmt.Println("\n=== Hub Interface ===")
	fmt.Println("  Each Hub implements:")
	methods := []string{
		"ID()          — unique identifier for the hub",
		"DisplayName() — user-facing name",
		"List(opts)    — paginated search for installable cards",
		"Get(cardID)   — retrieve a specific card by ID",
		"Install(id, dir) — download and install a card's resources",
		"Close()       — release resources held by the hub",
	}
	for _, m := range methods {
		fmt.Println("   ", m)
	}

	// Show the Card type fields.
	fmt.Println("\n=== Card Structure ===")
	fmt.Println("  A Card contains:")
	fields := []string{
		"ID, Owner, Kind (mcp|skill), Name, Description",
		"Version, IconURL, Tags, Config, Meta",
	}
	for _, f := range fields {
		fmt.Println("   ", f)
	}

	// Demonstrate ListOptions.
	fmt.Println("\n=== ListOptions ===")
	opts := &hub.ListOptions{
		Query: "code-review",
		Tags:  []string{"productivity", "dev-tools"},
		Kind:  hub.CardKindMCP,
		Limit: 10,
	}
	fmt.Printf("  Query=%q  Tags=%s  Kind=%s  Limit=%d\n",
		opts.Query, strings.Join(opts.Tags, ","), opts.Kind, opts.Limit)

	// Clean up.
	if err := reg.Close(); err != nil {
		fmt.Println("close:", err)
	}
	fmt.Println("\n=== Done ===")
	fmt.Println("  Registry closed. All hub connections released.")
}
