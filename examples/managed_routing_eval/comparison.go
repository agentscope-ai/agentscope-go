package main

import (
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/replay/evalkit"
)

// This checks observed pilot margins only. It is not a statistical qualification
// decision and never creates managedtask.Qualification or enables production.
type marginComparison struct {
	Group, Category                         string
	ObservedQualityLoss                     float64
	MaxQualityLoss                          float64
	MaxP95LatencyMS                         int
	CompleteObservation, WithinPilotMargins bool
}

func compareMargins(reports []*evalkit.ManagedLoadReport, p *experimentPlan) []marginComparison {
	if len(reports) < 2 {
		return nil
	}
	var out []marginComparison
	for _, report := range reports[1:] {
		for _, category := range []string{"extract", "classify", "rewrite"} {
			base, bok := reports[0].Categories[category]
			cand, cok := report.Categories[category]
			row := marginComparison{Group: report.Load.Manifest.Scenario, Category: category, MaxQualityLoss: p.MaxQualityLoss, MaxP95LatencyMS: p.MaxP95LatencyMS, ObservedQualityLoss: base.QualityPerPlanned - cand.QualityPerPlanned}
			row.CompleteObservation = bok && cok && base.Planned > 0 && base.Planned == cand.Planned && base.Completed == base.Planned && base.Scored == base.Completed && cand.Completed == cand.Planned && cand.Scored == cand.Completed && cand.CompletionLatencies != nil
			row.WithinPilotMargins = row.CompleteObservation && row.ObservedQualityLoss <= p.MaxQualityLoss && cand.CompletionLatencies.P95 <= time.Duration(p.MaxP95LatencyMS)*time.Millisecond
			out = append(out, row)
		}
	}
	return out
}
