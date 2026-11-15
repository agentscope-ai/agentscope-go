// Compare managed entry points with fixtures by default. Live mode requires an
// explicit experiment plan and credentials; no production qualification is made.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/managedtask"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/replay/evalkit"
)

func main() {
	live := flag.Bool("live", false, "enable billable requests to the explicitly configured destinations")
	config := flag.String("config", "", "live experiment JSON plan (no credential values)")
	split := flag.String("split", "acceptance", "offline dataset split: tuning or acceptance")
	revision := flag.String("revision", "offline-unrecorded", "source revision (record git rev-parse HEAD)")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, *live, *config, *split, *revision, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, live bool, path, split, revision string, w io.Writer) error {
	var p experimentPlan
	if live {
		if path == "" {
			return fmt.Errorf("-live requires -config with an explicit experiment plan")
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("cannot open experiment plan")
		}
		defer f.Close()
		decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&p) != nil {
			return fmt.Errorf("invalid experiment plan")
		}
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			return fmt.Errorf("experiment plan must contain one JSON object")
		}
		split = p.Split
	} else if path != "" {
		return fmt.Errorf("-config requires explicit -live")
	}
	version, samples, err := loadSamples(split)
	if err != nil {
		return err
	}
	var cl classifier.DispatchClassifier
	if live {
		cl, err = liveClassifier(&p, version, samples, os.Getenv)
		if err != nil {
			return err
		}
	} else {
		baseline := fixtureServer(samples, false)
		defer baseline.Close()
		cheap := fixtureServer(samples, true)
		defer cheap.Close()
		rate := 0.000001
		p = experimentPlan{Version: 1, SourceRevision: revision, DatasetVersion: version, RubricVersion: "managed-text-rubric-v1", Split: split, SampleSize: len(samples), MaxQualityLoss: 0.02, MaxP95LatencyMS: 5000, ConfirmEvaluationIsolation: true, FixedID: "baseline", DefaultID: "baseline", CategoryTargets: map[string]string{"extract": "candidate", "classify": "candidate", "rewrite": "baseline"}, Jev: classificationPlan{MinConfidence: 0.8}}
		for _, item := range []struct{ id, base, model string }{{"baseline", baseline.URL, "baseline-fixture-v1"}, {"candidate", cheap.URL, "candidate-fixture-v1"}} {
			p.Candidates = append(p.Candidates, candidate{ID: item.id, BaseURL: item.base, Model: item.model, ContextWindow: 8192, InputTokenUpperBound: 4096, VersionEvidence: "synthetic", BoundEvidence: "synthetic bytes plus 32", Prices: &managedtask.Prices{Currency: "fixture-credits", InputPerToken: &rate, OutputPerToken: &rate}})
		}
		categories := map[string]string{}
		for i := range samples {
			s := &samples[i]
			categories[s.Input] = s.Category
		}
		cl = &fixtureClassifier{categories: categories}
		return compare(ctx, &p, samples, cl, false, w)
	}
	return compare(ctx, &p, samples, cl, true, w)
}

func compare(ctx context.Context, p *experimentPlan, samples []sample, cl classifier.DispatchClassifier, live bool, w io.Writer) error {
	deployments, closeClients, err := buildDeployments(p, samples, live)
	if err != nil {
		return err
	}
	defer closeClients()
	limits := experimentLimits()
	var fixed *managedtask.Executor
	for i := range deployments {
		d := deployments[i]
		if d.Deployment.ID == p.FixedID {
			d.Limits = &limits
			fixed, err = managedtask.New(d)
			if err != nil {
				return err
			}
		}
	}
	if fixed == nil {
		return fmt.Errorf("fixed deployment is not configured")
	}
	rules, err := buildRouter(p, deployments, nil, samples)
	if err != nil {
		return err
	}
	routed, err := buildRouter(p, deployments, cl, samples)
	if err != nil {
		return err
	}
	evidence := "offline-fixtures"
	third := "rules+simulated-classifier"
	if live {
		evidence = "live-quality-experiment"
		third = "rules+Jev"
	}
	output := struct {
		Evidence    string
		Plan        *experimentPlan
		Reports     []*evalkit.ManagedLoadReport
		Comparisons []marginComparison
	}{Evidence: evidence, Plan: p}
	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	for _, group := range []struct {
		name    string
		execute func(context.Context, managedtask.Task) *managedtask.Result
	}{
		{"fixed", func(ctx context.Context, t managedtask.Task) *managedtask.Result {
			t.Category = "general"
			return fixed.Execute(ctx, t)
		}},
		{"rules-only", rules.Execute}, {third, routed.Execute},
	} {
		metadata, marshalErr := json.Marshal(p)
		if marshalErr != nil {
			return marshalErr
		}
		manifest := &evalkit.WorkloadManifest{Version: 1, RunID: stamp + "/" + group.name, Scenario: group.name, SourceRevision: p.SourceRevision, TaskSetVersion: p.DatasetVersion + "/" + p.Split, QualityThreshold: 1, Metadata: map[string]string{"evidence": evidence, "experiment_plan": string(metadata), "rubric": p.RubricVersion, "sampling": "provider defaults; pin server configuration in VersionEvidence"}}
		categories := map[string]string{}
		for i := range samples {
			s := &samples[i]
			gold, _ := json.Marshal(s)
			manifest.Tasks = append(manifest.Tasks, evalkit.TaskSpec{ID: s.ID, Input: s.Input, Tags: []string{s.Language, s.Category}, Scorer: evalkit.ScorerSpec{Ref: "managed-text-rubric-v1", Expect: string(gold)}})
			manifest.Arrivals = append(manifest.Arrivals, evalkit.TaskArrival{TaskID: s.ID, Repeat: 1, Offset: time.Duration(i) * 25 * time.Millisecond})
			categories[s.ID] = s.Category
		}
		report, runErr := new(evalkit.Runner).RunManagedLoad(ctx, context.Background(), manifest, &evalkit.ManagedLoadConfig{MaxInFlight: 4, TaskTimeout: 10 * time.Second, DrainTimeout: 15 * time.Second, MaxPendingScores: len(samples), MaxScorers: 2, ScoreTimeout: time.Second, ScorePhaseTimeout: 5 * time.Second, Execute: group.execute, Scorer: rubricScorer{}, Categories: categories})
		if report != nil {
			output.Reports = append(output.Reports, report)
		}
		if runErr != nil {
			if err := json.NewEncoder(w).Encode(output); err != nil {
				return err
			}
			return runErr
		}
	}
	output.Comparisons = compareMargins(output.Reports, p)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}
