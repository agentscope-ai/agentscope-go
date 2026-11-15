// Package main demonstrates a multi-agent Werewolves game.
//
// This example showcases:
//   - Multi-agent orchestration with role-based behavior
//   - Secret information management (werewolves know each other)
//   - Voting and elimination mechanics
//   - Deception and deduction through agent prompts
//   - Game state management across rounds
//
// The game uses mock model responses to run without API keys.
// Replace the mock model with a real ChatModel for LLM-driven gameplay.
package main

import (
	"context"
	"fmt"
	"math/rand"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/agent"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// Role represents a player's role in the game.
type Role string

const (
	RoleWerewolf Role = "Werewolf"
	RoleVillager Role = "Villager"
	RoleSeer     Role = "Seer"
	RoleGuard    Role = "Guard"
)

// Player holds game state for one participant.
type Player struct {
	Name  string
	Role  Role
	Alive bool
	Agent *agent.UnifiedAgent
}

// Game manages the werewolves game state.
type Game struct {
	Players []*Player
	Round   int
}

// mockModel returns scripted responses based on game context.
// Replace with a real model.ChatModel for LLM-driven play:
//
//	cm, _ := model.NewDashScopeChatModel(model.DashScopeConfig{...})
type mockModel struct {
	player *Player
	game   *Game
}

func (m *mockModel) Chat(_ context.Context, msgs []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	lastMsg := msgs[len(msgs)-1]
	input := ""
	if txt := lastMsg.GetTextContent("\n"); txt != nil {
		input = *txt
	}

	var response string

	switch {
	case strings.Contains(input, "eliminate tonight"):
		targets := m.livingWithout(RoleWerewolf)
		if len(targets) > 0 {
			response = fmt.Sprintf("I vote to eliminate %s tonight.", targets[rand.Intn(len(targets))])
		} else {
			response = "No target available."
		}

	case strings.Contains(input, "investigate"):
		targets := m.livingOthers()
		if len(targets) > 0 {
			response = fmt.Sprintf("I want to investigate %s.", targets[rand.Intn(len(targets))])
		} else {
			response = "No one to investigate."
		}

	case strings.Contains(input, "protect"):
		targets := m.livingOthers()
		if len(targets) > 0 {
			response = fmt.Sprintf("I will protect %s tonight.", targets[rand.Intn(len(targets))])
		} else {
			response = "I protect myself."
		}

	case strings.Contains(input, "Discuss"):
		response = m.generateDiscussion()

	case strings.Contains(input, "Vote"):
		targets := m.livingOthers()
		if len(targets) > 0 {
			response = fmt.Sprintf("I vote to eliminate %s.", targets[rand.Intn(len(targets))])
		} else {
			response = "I abstain."
		}

	default:
		response = "I have nothing to add."
	}

	return &model.ChatResponse{
		Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: response}},
		IsLast:  true,
	}, nil
}

func (m *mockModel) ChatStream(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	ch := make(chan model.ChatResponse, 1)
	ch <- model.ChatResponse{IsLast: true}
	close(ch)
	return ch, nil
}

func (m *mockModel) CountTokens(_ []*message.Msg, _ []model.ToolSchema) int {
	return 0
}

func (m *mockModel) livingWithout(excludeRole Role) []string {
	var names []string
	for _, p := range m.game.Players {
		if p.Alive && p.Role != excludeRole && p.Name != m.player.Name {
			names = append(names, p.Name)
		}
	}
	return names
}

func (m *mockModel) livingOthers() []string {
	var names []string
	for _, p := range m.game.Players {
		if p.Alive && p.Name != m.player.Name {
			names = append(names, p.Name)
		}
	}
	return names
}

func (m *mockModel) generateDiscussion() string {
	switch m.player.Role {
	case RoleWerewolf:
		targets := m.livingWithout(RoleWerewolf)
		if len(targets) > 0 {
			return fmt.Sprintf("I think %s has been acting suspiciously.", targets[rand.Intn(len(targets))])
		}
	case RoleSeer:
		return "Based on my observations, I have some suspicions but need more evidence."
	}
	return "I don't have strong evidence yet. Let's be careful with our vote."
}

func newGame() *Game {
	g := &Game{Round: 0}

	// 6-player setup: 2 Werewolves, 1 Seer, 1 Guard, 2 Villagers
	setup := []struct {
		name string
		role Role
	}{
		{"Alice", RoleWerewolf},
		{"Bob", RoleWerewolf},
		{"Charlie", RoleSeer},
		{"Diana", RoleGuard},
		{"Eve", RoleVillager},
		{"Frank", RoleVillager},
	}

	for _, s := range setup {
		p := &Player{Name: s.name, Role: s.role, Alive: true}
		sysPrompt := buildSystemPrompt(p, setup)
		mm := &mockModel{player: p, game: g}
		p.Agent = agent.NewUnifiedAgent(s.name, sysPrompt, mm)
		g.Players = append(g.Players, p)
	}

	return g
}

func buildSystemPrompt(p *Player, setup []struct {
	name string
	role Role
}) string {
	base := fmt.Sprintf("You are %s in a Werewolves game. Your role: %s.\n", p.Name, p.Role)
	switch p.Role {
	case RoleWerewolf:
		var partners []string
		for _, s := range setup {
			if s.role == RoleWerewolf && s.name != p.Name {
				partners = append(partners, s.name)
			}
		}
		base += fmt.Sprintf("Your werewolf partner: %s. Eliminate villagers at night; deflect suspicion by day.\n", strings.Join(partners, ", "))
	case RoleSeer:
		base += "Each night investigate one player to learn their role. Use knowledge wisely.\n"
	case RoleGuard:
		base += "Each night protect one player. Cannot protect the same player twice in a row.\n"
	case RoleVillager:
		base += "No special abilities. Observe, deduce, and vote wisely.\n"
	}
	base += "Respond concisely. When voting, name exactly one player."
	return base
}

func (g *Game) livingPlayers() []*Player {
	var alive []*Player
	for _, p := range g.Players {
		if p.Alive {
			alive = append(alive, p)
		}
	}
	return alive
}

func (g *Game) checkWin() (bool, string) {
	wolves, others := 0, 0
	for _, p := range g.livingPlayers() {
		if p.Role == RoleWerewolf {
			wolves++
		} else {
			others++
		}
	}
	if wolves == 0 {
		return true, "Villagers"
	}
	if wolves >= others {
		return true, "Werewolves"
	}
	return false, ""
}

func (g *Game) nightPhase(ctx context.Context) {
	g.Round++
	fmt.Printf("\n=== Night %d ===\n", g.Round)

	// Werewolves vote to kill
	votes := make(map[string]int)
	for _, p := range g.Players {
		if !p.Alive || p.Role != RoleWerewolf {
			continue
		}
		resp, err := p.Agent.Reply(ctx, "Who do you want to eliminate tonight? Name one player.")
		if err != nil {
			continue
		}
		if target := extractName(resp, g.livingPlayers(), p.Name); target != "" {
			votes[target]++
		}
	}
	killTarget := majorityVote(votes)

	// Guard protects
	var protectedPlayer string
	for _, p := range g.Players {
		if !p.Alive || p.Role != RoleGuard {
			continue
		}
		resp, err := p.Agent.Reply(ctx, "Who do you want to protect tonight?")
		if err != nil {
			continue
		}
		protectedPlayer = extractName(resp, g.livingPlayers(), "")
	}

	// Seer investigates
	for _, p := range g.Players {
		if !p.Alive || p.Role != RoleSeer {
			continue
		}
		resp, err := p.Agent.Reply(ctx, "Who do you want to investigate tonight?")
		if err != nil {
			continue
		}
		if target := extractName(resp, g.livingPlayers(), p.Name); target != "" {
			for _, tp := range g.Players {
				if tp.Name == target {
					fmt.Printf("  [Seer %s] investigated %s → %s\n", p.Name, target, tp.Role)
					break
				}
			}
		}
	}

	// Resolve kill
	if killTarget != "" && killTarget != protectedPlayer {
		for _, p := range g.Players {
			if p.Name == killTarget {
				p.Alive = false
				fmt.Printf("  ☠ %s was eliminated by werewolves! (was %s)\n", p.Name, p.Role)
				break
			}
		}
	} else if killTarget == protectedPlayer && killTarget != "" {
		fmt.Printf("  🛡 The guard saved %s!\n", protectedPlayer)
	}
}

func (g *Game) dayPhase(ctx context.Context) {
	fmt.Printf("\n=== Day %d — Discussion ===\n", g.Round)

	// Discussion round
	for _, p := range g.livingPlayers() {
		resp, err := p.Agent.Reply(ctx, fmt.Sprintf("Discuss who you think is a werewolf. Round %d.", g.Round))
		if err != nil {
			continue
		}
		if txt := resp.GetTextContent("\n"); txt != nil {
			fmt.Printf("  [%s] %s\n", p.Name, *txt)
		}
	}

	// Voting
	fmt.Printf("\n=== Day %d — Vote ===\n", g.Round)
	votes := make(map[string]int)
	for _, p := range g.livingPlayers() {
		resp, err := p.Agent.Reply(ctx, "Vote to eliminate one player. Name them clearly.")
		if err != nil {
			continue
		}
		if target := extractName(resp, g.livingPlayers(), p.Name); target != "" {
			votes[target]++
			fmt.Printf("  [%s] → %s\n", p.Name, target)
		}
	}

	eliminated := majorityVote(votes)
	if eliminated != "" {
		for _, p := range g.Players {
			if p.Name == eliminated {
				p.Alive = false
				fmt.Printf("  ⚖ %s was voted out! (was %s)\n", p.Name, p.Role)
				break
			}
		}
	} else {
		fmt.Println("  No majority — no one is eliminated.")
	}
}

func extractName(resp *message.Msg, players []*Player, exclude string) string {
	if resp == nil {
		return ""
	}
	txt := resp.GetTextContent("\n")
	if txt == nil {
		return ""
	}
	lower := strings.ToLower(*txt)
	for _, p := range players {
		if p.Name != exclude && strings.Contains(lower, strings.ToLower(p.Name)) {
			return p.Name
		}
	}
	return ""
}

func majorityVote(votes map[string]int) string {
	var best string
	var bestCount int
	for name, count := range votes {
		if count > bestCount {
			best = name
			bestCount = count
		}
	}
	if bestCount >= 2 {
		return best
	}
	return ""
}

func main() {
	ctx := context.Background()
	g := newGame()

	fmt.Println("╔══════════════════════════════════════╗")
	fmt.Println("║   Werewolves of Miller's Hollow     ║")
	fmt.Println("╠══════════════════════════════════════╣")
	fmt.Println("║ Players: Alice Bob Charlie Diana     ║")
	fmt.Println("║          Eve Frank                   ║")
	fmt.Println("║ Roles: 2 Werewolves, 1 Seer,        ║")
	fmt.Println("║        1 Guard, 2 Villagers          ║")
	fmt.Println("╚══════════════════════════════════════╝")

	const maxRounds = 5
	for i := 0; i < maxRounds; i++ {
		g.nightPhase(ctx)
		if done, winner := g.checkWin(); done {
			fmt.Printf("\n*** GAME OVER: %s win! ***\n", winner)
			return
		}

		g.dayPhase(ctx)
		if done, winner := g.checkWin(); done {
			fmt.Printf("\n*** GAME OVER: %s win! ***\n", winner)
			return
		}
	}

	fmt.Println("\n*** GAME OVER: Maximum rounds reached ***")
}
