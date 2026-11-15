// Package systemoneconfig shares opt-in local configuration between verification
// entry points. It does not change the classifier client's defaults.
package systemoneconfig

import (
	"fmt"
	"net"
	"net/url"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/jev"
)

// Local returns nil when no override is requested. Callers must check their live
// opt-in before using it. Only loopback destinations are accepted; this is URL
// validation, not process or network isolation.
func Local(getenv func(string) string) (*jev.Config, error) {
	endpoint := getenv("SYSTEMONE_LOCAL_ENDPOINT")
	model := getenv("SYSTEMONE_LOCAL_MODEL")
	key := getenv("SYSTEMONE_LOCAL_API_KEY")
	if endpoint == "" && model == "" && key == "" {
		return nil, nil
	}
	if endpoint == "" || model == "" {
		return nil, fmt.Errorf("local System One requires both SYSTEMONE_LOCAL_ENDPOINT and SYSTEMONE_LOCAL_MODEL (exact served model name)")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u == nil {
		return nil, fmt.Errorf("invalid local System One endpoint")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("local System One endpoint must use localhost or a loopback IP")
	}
	// jev.New requires a non-empty key even for unauthenticated local servers.
	// Never forward the hosted TypeSafe credential implicitly.
	if key == "" {
		key = "local-systemone"
	}
	cfg := &jev.Config{Endpoint: endpoint, Model: model, APIKey: key}
	if _, err := jev.New(*cfg); err != nil {
		return nil, fmt.Errorf("invalid local System One endpoint, served model name or API key")
	}
	return cfg, nil
}
