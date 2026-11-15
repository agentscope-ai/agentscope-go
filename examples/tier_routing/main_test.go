package main

import "testing"

func TestOfflineComparison(t *testing.T) {
	reports, err := run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(reports) != 4 {
		t.Fatalf("groups: %d", len(reports))
	}
	byName := make(map[string]groupReport, len(reports))
	for _, report := range reports {
		byName[report.Group] = report
		if report.Planned != len(tasks) || len(report.Results) != len(tasks) {
			t.Fatalf("%s lost arrivals: %+v", report.Group, report)
		}
		if report.Completed+report.Failed != report.Planned {
			t.Fatalf("%s outcomes: %+v", report.Group, report)
		}
	}
	for _, name := range []string{"fixed-device", "fixed-edge", "routed"} {
		if byName[name].Completed != len(tasks) {
			t.Fatalf("%s completed=%d", name, byName[name].Completed)
		}
	}
	if byName["fixed-cloud"].Failed != 1 {
		t.Fatalf("fixed cloud report: %+v", byName["fixed-cloud"])
	}
	routed := byName["routed"]
	if routed.Requests != len(tasks)+1 || routed.TierRequests["cloud"] != 2 || routed.TierRequests["edge"] != 2 || routed.TierRequests["device"] != 1 {
		t.Fatalf("routed requests: %+v", routed)
	}
	if routed.UnknownUsageAttempts != 1 {
		t.Fatalf("unknown usage attempts: %d", routed.UnknownUsageAttempts)
	}
	status := routed.Results[0]
	if status.ID != "status" || status.Attempts != 1 || status.Status != "completed" ||
		len(status.Tiers) != 1 || status.Tiers[0] != "device" {
		t.Fatalf("device-only control: %+v", status)
	}
	plan := routed.Results[2]
	if plan.ID != "plan" || plan.Attempts != 2 || plan.Status != "completed" {
		t.Fatalf("fallback result: %+v", plan)
	}
}
