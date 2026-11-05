package systemoneconfig

import (
	"testing"
)

func TestLocal(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, model, key string
		wantErr                    bool
	}{
		{name: "disabled"},
		{name: "ipv4", endpoint: "http://127.0.0.1:8080/v1/systemone", model: "Piccolo-4B"},
		{name: "ipv6", endpoint: "http://[::1]:8080/v1/systemone", model: "org/Piccolo", key: "fixture-key"},
		{name: "localhost TLS", endpoint: "https://localhost:8080/v1/systemone", model: "served-name"},
		{name: "missing model", endpoint: "http://localhost/v1/systemone", wantErr: true},
		{name: "missing endpoint", model: "Piccolo", wantErr: true},
		{name: "key alone", key: "fixture-key", wantErr: true},
		{name: "remote", endpoint: "http://example.com/v1/systemone", model: "Piccolo", wantErr: true},
		{name: "remote TLS", endpoint: "https://example.com/v1/systemone", model: "Piccolo", wantErr: true},
		{name: "lookalike", endpoint: "http://localhost.example.com/v1/systemone", model: "Piccolo", wantErr: true},
		{name: "userinfo", endpoint: "http://user@localhost/v1/systemone", model: "Piccolo", wantErr: true},
		{name: "query", endpoint: "http://localhost/v1/systemone?x=y", model: "Piccolo", wantErr: true},
		{name: "empty query", endpoint: "http://localhost/v1/systemone?", model: "Piccolo", wantErr: true},
		{name: "fragment", endpoint: "http://localhost/v1/systemone#x", model: "Piccolo", wantErr: true},
		{name: "bad port", endpoint: "http://localhost:bad/v1/systemone", model: "Piccolo", wantErr: true},
		{name: "scheme", endpoint: "ftp://localhost/v1/systemone", model: "Piccolo", wantErr: true},
		{name: "blank model", endpoint: "http://localhost/v1/systemone", model: " ", wantErr: true},
		{name: "invalid key", endpoint: "http://localhost/v1/systemone", model: "Piccolo", key: "bad\nkey", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"SYSTEMONE_LOCAL_ENDPOINT": tc.endpoint, "SYSTEMONE_LOCAL_MODEL": tc.model, "SYSTEMONE_LOCAL_API_KEY": tc.key}
			cfg, err := Local(func(k string) string { return env[k] })
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if tc.endpoint == "" {
				if cfg != nil {
					t.Fatal("override enabled by default")
				}
				return
			}
			if cfg == nil || cfg.Endpoint != tc.endpoint || cfg.Model != tc.model {
				t.Fatal("lost endpoint or exact served model")
			}
			key := tc.key
			if key == "" {
				key = "local-systemone"
			}
			if cfg.APIKey != key {
				t.Fatal("wrong local credential")
			}
		})
	}
}
