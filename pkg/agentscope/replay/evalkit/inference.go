package evalkit

import "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/inference"

func joinInference(report *LoadReport, ledger *inference.Ledger, unmanaged []bool) {
	if ledger == nil {
		return
	}
	snapshot := ledger.SnapshotFor(report.Manifest.RunID, report.Manifest.Scenario)
	for i, value := range unmanaged {
		if value {
			snapshot.Incomplete = true
			report.Results[i].InferenceUnmanaged = true
		}
	}
	for i := range snapshot.Attempts {
		attempt := &snapshot.Attempts[i]
		index := attempt.Attribution.Iteration - 1
		if index < 0 || index >= len(report.Results) {
			report.InferenceUnmatched++
			snapshot.Incomplete = true
			continue
		}
		arrival := report.Manifest.Arrivals[index]
		if attempt.Attribution.TaskID != arrival.TaskID || attempt.Attribution.Repeat != arrival.Repeat {
			report.InferenceUnmatched++
			snapshot.Incomplete = true
			continue
		}
		report.Results[index].InferenceAttemptIDs = append(report.Results[index].InferenceAttemptIDs, attempt.ID)
	}
	report.Inference = &snapshot
}
