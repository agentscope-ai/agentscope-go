package evalkit

import (
	"math"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/bench"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/managedtask"
)

func (r *ManagedLoadReport) summarize(deadline time.Duration) {
	groups := map[string][]int{}
	all := make([]int, len(r.Results))
	for i, row := range r.Results {
		all[i] = i
		groups[row.Category] = append(groups[row.Category], i)
	}
	r.Overall = r.summary(all, deadline)
	for category, indices := range groups {
		r.Categories[category] = r.summary(indices, deadline)
	}
}

func (r *ManagedLoadReport) summary(indices []int, deadline time.Duration) ManagedSummary {
	s := ManagedSummary{Planned: len(indices), Statuses: map[managedtask.Status]int{}, ArrivalStatuses: map[bench.OpenLoopStatus]int{}, KnownCostByCurrency: map[string]float64{}, KnownCostPerQualified: map[string]float64{}}
	scoreSum := 0.0
	subset := &LoadReport{Execution: r.Load.Execution}
	for _, i := range indices {
		row := &r.Load.Results[i]
		result := r.Results[i].Result
		subset.Results = append(subset.Results, *row)
		s.ArrivalStatuses[row.Arrival.Status]++
		if result == nil {
			if !row.Arrival.StartedAt.IsZero() {
				s.UnobservedExecutions++
			}
			continue
		}
		s.Statuses[result.Status]++
		if result.Status == managedtask.Succeeded && result.Err == nil && row.Arrival.Status == bench.OpenLoopSucceeded {
			s.Completed++
			if row.QualityStatus == QualityScored {
				s.Scored++
				scoreSum += row.Task.Score
				if row.Task.Pass && row.Arrival.Latency < deadline {
					s.Qualified++
				}
			}
		}
		s.UnknownCostAttempts += result.Cost.UnknownAttempts
		if result.Cost.Currency != "" {
			sum := s.KnownCostByCurrency[result.Cost.Currency] + result.Cost.KnownSubtotal
			if math.IsNaN(sum) || math.IsInf(sum, 0) || sum < 0 {
				s.UnknownCostAttempts++
			} else {
				s.KnownCostByCurrency[result.Cost.Currency] = sum
			}
		} else if result.Cost.KnownSubtotal != 0 {
			s.UnknownCostAttempts++
		}
		generations := 0
		for j := range result.Attempts {
			a := &result.Attempts[j]
			if !a.Completed {
				s.UnfinishedAttempts++
			}
			if a.Kind == managedtask.SimulatedAttempt {
				s.SimulatedAttempts++
				continue
			}
			s.PhysicalRequests++
			if a.OperationID != "" {
				s.ClassificationRequests++
				s.ClassificationAttemptTime += a.Duration
			} else {
				s.GenerationRequests++
				generations++
			}
		}
		s.AdditionalGenerationRequests += max(0, generations-1)
	}
	if s.Planned > 0 {
		s.CompletionRate = float64(s.Completed) / float64(s.Planned)
		s.QualityPerPlanned = scoreSum / float64(s.Planned)
	}
	if s.Scored > 0 {
		mean := scoreSum / float64(s.Scored)
		s.MeanScoredQuality = &mean
	}
	if s.Qualified > 0 {
		for currency, cost := range s.KnownCostByCurrency {
			s.KnownCostPerQualified[currency] = cost / float64(s.Qualified)
		}
		if len(s.KnownCostPerQualified) == 1 && s.UnknownCostAttempts == 0 && s.UnobservedExecutions == 0 && s.UnfinishedAttempts == 0 {
			for _, v := range s.KnownCostPerQualified {
				value := v
				s.CostPerQualified = &value
			}
		}
	}
	summarizeLoad(subset, deadline)
	s.LatencySamples = subset.CompletionSamples
	s.CompletionLatencies = subset.Latencies
	return s
}
