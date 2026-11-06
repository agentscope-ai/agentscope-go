package jev_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLocalLiveContractFixture(t *testing.T) {
	var calls atomic.Int32
	served := "org/Piccolo-fixture"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" || body.Model != served || r.Header.Get("Authorization") != "Bearer local-systemone" {
			t.Error("lost local endpoint, model or dummy credential")
		}
		fmt.Fprint(w, strings.Replace(fixture, "jev-fixture-v1", served, 1))
	}))
	defer s.Close()
	t.Setenv("JEV_LIVE_TEST", "1")
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("SYSTEMONE_LOCAL_ENDPOINT", s.URL+"/v1/systemone")
	t.Setenv("SYSTEMONE_LOCAL_MODEL", served)
	t.Setenv("SYSTEMONE_LOCAL_API_KEY", "")
	t.Run("contract", TestLiveContract)
	if calls.Load() != 1 {
		t.Fatalf("local contract made %d requests, want 1", calls.Load())
	}
	calls.Store(0)
	t.Setenv("JEV_LIVE_TEST", "")
	t.Run("disabled", TestLiveContract)
	if calls.Load() != 0 {
		t.Fatal("live opt-in was bypassed")
	}
}

func TestHostedLiveContractConfiguration(t *testing.T) {
	for _, tc := range []struct {
		key, model string
		skip, fail bool
	}{
		{skip: true}, {model: "bad", skip: true}, {key: "fixture-key", fail: true},
		{key: "fixture-key", model: "Piccolo", fail: true},
		{key: "fixture-key", model: "jev-1.13.0"},
	} {
		cfg, err := liveContractConfig(func(k string) string {
			switch k {
			case "TYPESAFE_API_KEY":
				return tc.key
			case "JEV_LIVE_MODEL":
				return tc.model
			}
			return ""
		})
		if (err != nil) != tc.fail {
			t.Fatalf("model %q: %v", tc.model, err)
		}
		if !tc.fail && (cfg == nil) != tc.skip {
			t.Fatal("changed hosted skip behavior")
		}
		if cfg != nil && (cfg.Endpoint != "" || cfg.Model != tc.model || cfg.APIKey != tc.key) {
			t.Fatal("changed hosted defaults")
		}
	}
}
