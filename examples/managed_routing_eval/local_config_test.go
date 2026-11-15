package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/jev"
)

func localTestPlan() (*experimentPlan, string, []sample) {
	version, samples, _ := loadSamples("acceptance")
	p := &experimentPlan{Version: 1, SourceRevision: "test", DatasetVersion: version, RubricVersion: "managed-text-rubric-v1", SampleSize: len(samples), MaxQualityLoss: 0.02, MaxP95LatencyMS: 5000, ConfirmEvaluationIsolation: true, FixedID: "baseline", DefaultID: "baseline", CategoryTargets: map[string]string{"extract": "candidate", "classify": "candidate", "rewrite": "baseline"}, Jev: classificationPlan{Endpoint: jev.DefaultEndpoint, Model: "jev-1.13.0", MinConfidence: 0.8}}
	for _, id := range []string{"baseline", "candidate"} {
		p.Candidates = append(p.Candidates, candidate{ID: id, BaseURL: "https://example.com", Model: "pinned-v1", VersionEvidence: "fixture", APIKeyEnv: "TEST_MODEL_API_KEY", ContextWindow: 8192, InputTokenUpperBound: 4096, BoundEvidence: "fixture proof"})
	}
	return p, version, samples
}

func TestLocalClassifierConfiguration(t *testing.T) {
	for _, key := range []string{"", "local-fixture-key"} {
		t.Run("key="+key, func(t *testing.T) {
			served := "org/Piccolo-fixture"
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body struct{ Model string }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				expectedKey := key
				if expectedKey == "" {
					expectedKey = "local-systemone"
				}
				if body.Model != served || r.URL.Path != "/v1/systemone" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+expectedKey {
					t.Error("wrong local classifier request")
				}
				fmt.Fprintf(w, `{"model":%q,"answers":{"ok":{"type":"noul","noul":0.8}}}`, served)
			}))
			defer s.Close()
			env := map[string]string{"SYSTEMONE_LOCAL_ENDPOINT": s.URL + "/v1/systemone", "SYSTEMONE_LOCAL_MODEL": served, "SYSTEMONE_LOCAL_API_KEY": key, "TYPESAFE_API_KEY": "hosted-must-not-leak"}
			p, version, samples := localTestPlan()
			cl, err := liveClassifier(p, version, samples, func(k string) string { return env[k] })
			if err != nil {
				t.Fatal(err)
			}
			if p.Jev.Endpoint != env["SYSTEMONE_LOCAL_ENDPOINT"] || p.Jev.Model != served {
				t.Fatal("report lost effective classifier binding")
			}
			out, err := cl.Classify(context.Background(), classifier.Request{OperationID: "local-fixture", State: json.RawMessage(`"fixture"`), Questions: map[string]classifier.Question{"ok": &classifier.BinaryQuestion{Instructions: "True?"}}})
			if err != nil || out.Model != served || calls != 1 {
				t.Fatalf("fixture call failed: %v", err)
			}
			p.Candidates[0].BaseURL = "http://127.0.0.1:8080"
			if _, err = liveClassifier(p, version, samples, func(k string) string { return env[k] }); err == nil {
				t.Fatal("local classifier weakened generation HTTPS check")
			}
			p, version, samples = localTestPlan()
			p.Candidates = p.Candidates[:1]
			if _, err = liveClassifier(p, version, samples, func(k string) string { return env[k] }); err == nil {
				t.Fatal("local classifier weakened candidate count")
			}
		})
	}
}

func TestHostedClassifierConfiguration(t *testing.T) {
	empty := func(string) string { return "" }
	p, version, samples := localTestPlan()
	if _, err := liveClassifier(p, version, samples, empty); err == nil || err.Error() != "TYPESAFE_API_KEY is required for live execution" {
		t.Fatalf("changed missing key behavior: %v", err)
	}
	cl, err := liveClassifier(p, version, samples, func(k string) string {
		if k == "TYPESAFE_API_KEY" {
			return "fixture-key"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, model := cl.Target()
	if endpoint != jev.DefaultEndpoint || model != "jev-1.13.0" {
		t.Fatal("changed hosted binding")
	}
	for _, mutate := range []func(*experimentPlan){func(p *experimentPlan) { p.Jev.Endpoint = "http://localhost/v1/systemone" }, func(p *experimentPlan) { p.Jev.Model = "Piccolo" }} {
		p, version, samples = localTestPlan()
		mutate(p)
		if _, err := liveClassifier(p, version, samples, empty); err == nil || !strings.Contains(err.Error(), "explicit HTTPS Jev destination/pinned version") {
			t.Fatalf("changed hosted validation: %v", err)
		}
	}
	p, version, samples = localTestPlan()
	if _, err := liveClassifier(p, version, samples, func(k string) string {
		if k == "SYSTEMONE_LOCAL_MODEL" {
			return "Piccolo"
		}
		return ""
	}); err == nil {
		t.Fatal("partial override accepted")
	}
}

func TestOfflineIgnoresLocalOverride(t *testing.T) {
	t.Setenv("SYSTEMONE_LOCAL_ENDPOINT", "invalid")
	t.Setenv("SYSTEMONE_LOCAL_MODEL", "")
	var output bytes.Buffer
	if err := run(context.Background(), false, "", "acceptance", "fixture", &output); err != nil {
		t.Fatal(err)
	}
	if output.Len() == 0 {
		t.Fatal("no offline report")
	}
}
