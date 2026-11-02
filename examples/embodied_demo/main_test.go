package main

import (
	"context"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/taskenvelope"
)

func TestOfflineCooperationComparison(t *testing.T) {
	for _, cfg := range []scenarioConfig{
		{Dir: t.TempDir(), UseCloud: false, CloudAvailable: false, LocalAlternative: "B", ApprovedAlternatives: []string{"B"}},
		{Dir: t.TempDir(), UseCloud: true, CloudAvailable: true, ApprovedAlternatives: []string{"B"}},
	} {
		s, err := newScenario(cfg)
		if err != nil {
			t.Fatal(err)
		}
		report, err := s.run(context.Background())
		if err != nil || report.Completed != 1 || report.Interventions != 0 || report.ActionEffects != 2 {
			t.Fatalf("report=%+v err=%v", report, err)
		}
		if report.DuplicateEffects != 0 || report.RejectedStale != 0 {
			t.Fatalf("fault report: %+v", report)
		}
		if len(s.feedback) != 2 || s.feedback[0].State != taskenvelope.StateCompleted || s.feedback[1].State != taskenvelope.StateCompleted {
			t.Fatalf("execution feedback: %+v", s.feedback)
		}
		if cfg.UseCloud {
			if report.CloudRequests != 1 || report.PlanningRequests != 1 {
				t.Fatalf("cloud requests: %+v", report)
			}
		} else if report.CloudRequests != 0 {
			t.Fatalf("no-cloud mode used cloud: %+v", report)
		}
		for _, fake := range s.models {
			for _, req := range fake.Requests() {
				for _, msg := range req.Messages {
					text := msg.GetTextContent("\n")
					if text != nil && strings.Contains(*text, "opaque://") {
						t.Fatal("media reference leaked into model request")
					}
				}
			}
		}
		for _, req := range s.cloud.Requests() {
			for _, msg := range req.Messages {
				text := msg.GetTextContent("\n")
				if text != nil && strings.Contains(*text, "opaque://") {
					t.Fatal("media reference leaked into cloud request")
				}
			}
		}
	}
}

func TestWANLossHasExplicitDegradedMode(t *testing.T) {
	s, err := newScenario(scenarioConfig{Dir: t.TempDir(), UseCloud: true, CloudAvailable: false, ApprovedAlternatives: []string{"B"}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.run(context.Background())
	if err == nil || report.Completed != 0 || report.Interventions != 1 || len(s.controller.Commands()) != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if len(s.cloud.Requests()) != 0 {
		t.Fatal("cloud called during WAN loss")
	}
}

func TestStaleObservationAndCloudProposalRejected(t *testing.T) {
	for name, cfg := range map[string]scenarioConfig{
		"observation": {Dir: t.TempDir(), StaleObservation: true, LocalAlternative: "B", ApprovedAlternatives: []string{"B"}},
		"cloud site":  {Dir: t.TempDir(), UseCloud: true, CloudAvailable: true, ReviseSiteDuringCloud: true, ApprovedAlternatives: []string{"B"}},
		"destination": {Dir: t.TempDir(), UseCloud: true, CloudAvailable: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := newScenario(cfg)
			if err != nil {
				t.Fatal(err)
			}
			report, err := s.run(context.Background())
			if err == nil || len(s.controller.Commands()) != 0 {
				t.Fatalf("report=%+v err=%v", report, err)
			}
		})
	}
}

func TestDeviceRejectedDestinationStopsWithoutPanic(t *testing.T) {
	s, err := newScenario(scenarioConfig{Dir: t.TempDir(), LocalAlternative: "C", ApprovedAlternatives: []string{"C"}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.run(context.Background())
	if err == nil || report.Completed != 0 || report.RejectedStale != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if len(s.controller.Commands()) != 1 {
		t.Fatalf("unexpected effect count: %d", len(s.controller.Commands()))
	}
}

func TestLostACKRestartDoesNotRepeatEffect(t *testing.T) {
	cfg := scenarioConfig{Dir: t.TempDir(), LocalAlternative: "B", ApprovedAlternatives: []string{"B"}, LoseACK: true}
	s, err := newScenario(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.run(context.Background()); err == nil {
		t.Fatal("lost ACK ignored")
	}
	if len(s.controller.Commands()) != 1 {
		t.Fatalf("commands before restart: %d", len(s.controller.Commands()))
	}
	cfg.Reopen = true
	cfg.LoseACK = false
	reopened, err := newScenario(cfg)
	if err != nil {
		t.Fatal(err)
	}
	report, err := reopened.run(context.Background())
	if err != nil || report.Completed != 1 || report.ActionEffects != 2 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if len(reopened.controller.Commands()) != 2 {
		t.Fatalf("duplicate or missing effects: %d", len(reopened.controller.Commands()))
	}
	first, ok := reopened.gateway.ActionRecord("plan-1/0")
	if !ok || first.State != taskenvelope.StateCompleted {
		t.Fatalf("first action did not reconcile: %+v", first)
	}
}

func TestWANLossUsesApprovedLocalAlternative(t *testing.T) {
	s, err := newScenario(scenarioConfig{Dir: t.TempDir(), UseCloud: true, CloudAvailable: false, LocalAlternative: "B", ApprovedAlternatives: []string{"B"}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.run(context.Background())
	if err != nil || report.Completed != 1 || report.CloudRequests != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestInvalidAgentDecisionStopsBeforeActions(t *testing.T) {
	s, err := newScenario(scenarioConfig{Dir: t.TempDir(), LocalAlternative: "B", ApprovedAlternatives: []string{"B"}, InvalidAgentReply: true})
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.run(context.Background())
	if err == nil || report.Completed != 0 || len(s.controller.Commands()) != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if report.Latency <= 0 || report.PlanningRequests != 1 {
		t.Fatalf("fault evidence missing: %+v", report)
	}
}
