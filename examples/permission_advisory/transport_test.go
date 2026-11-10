package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExplicitLocalTransport(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "malformed"}[broken], func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/systemone" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer local-only" {
					t.Error("wrong endpoint or credential")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if !bytes.Contains(body, []byte("approved synthetic fixture")) || bytes.Contains(body, []byte(demoIntent)) {
					t.Error("unexpected projection")
				}
				w.Header().Set("Content-Type", "application/json")
				if broken {
					_, _ = io.WriteString(w, `{"secret-response":`)
				} else {
					_, _ = io.WriteString(w, fixtureResponse)
				}
			}))
			defer server.Close()
			getenv := func(key string) string {
				switch key {
				case "SYSTEMONE_LOCAL_ENDPOINT":
					return server.URL + "/v1/systemone"
				case "SYSTEMONE_LOCAL_MODEL":
					return "advisory-fixture"
				case "SYSTEMONE_LOCAL_API_KEY":
					return "local-only"
				default:
					t.Errorf("unexpected env lookup: %s", key)
					return ""
				}
			}
			var output bytes.Buffer
			if err := run(context.Background(), true, true, getenv, &output); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatalf("calls=%d, want one per turn without retries", calls.Load())
			}
			for _, secret := range []string{server.URL, "local-only", "secret-response"} {
				if strings.Contains(output.String(), secret) {
					t.Fatal("audit leaked transport data")
				}
			}
			if broken && strings.Contains(output.String(), `"status":"advice"`) {
				t.Fatal("malformed provider response produced advice")
			}
			if !broken && !strings.Contains(output.String(), `"classifier_attempts":1`) {
				t.Fatal("missing attempt report")
			}
		})
	}
}
