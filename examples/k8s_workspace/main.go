// Package main demonstrates using K8sWorkspace for sandboxed agent execution
// and the kubectl read-only tools for cluster querying.
//
// Prerequisites:
//   - kubectl configured with cluster access
//   - A namespace where you can create Pods
//
// Run:
//
//	export KUBECONFIG=~/.kube/config
//	go run .
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/workspace"
)

func main() {
	ctx := context.Background()

	// --- K8s Workspace: sandboxed agent execution inside a Pod ---
	fmt.Println("=== K8s Workspace Demo ===")
	fmt.Println()
	fmt.Println("K8sWorkspace creates an ephemeral Pod for tool isolation.")
	fmt.Println("Agent file/shell tools execute inside the Pod, not on your machine.")
	fmt.Println()

	// Example config (would normally connect to a real cluster):
	cfg := &workspace.K8sConfig{
		Namespace:             "agent-sandbox",
		PodName:               "lathe-workspace",
		Image:                 "ubuntu:22.04",
		ImagePullPolicy:       "IfNotPresent",
		PodTTLSeconds:         1800, // auto-cleanup after 30 min
		DisableServiceAccount: true, // no SA token in the pod
		SecurityContext: &workspace.PodSecurityContext{
			RunAsNonRoot: boolPtr(true),
			RunAsUser:    int64Ptr(1000),
		},
		Resources: &workspace.ResourceRequirements{
			CPULimit:      "1000m",
			MemoryLimit:   "512Mi",
			CPURequest:    "100m",
			MemoryRequest: "128Mi",
		},
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "agentscope",
			"team":                         "platform",
		},
	}

	fmt.Printf("Config: namespace=%s, pod=%s, image=%s\n", cfg.Namespace, cfg.PodName, cfg.Image)
	fmt.Printf("Security: runAsNonRoot=%v, runAsUser=%d\n", *cfg.SecurityContext.RunAsNonRoot, *cfg.SecurityContext.RunAsUser)
	fmt.Printf("Resources: cpu=%s/%s, mem=%s/%s\n", cfg.Resources.CPURequest, cfg.Resources.CPULimit, cfg.Resources.MemoryRequest, cfg.Resources.MemoryLimit)
	fmt.Printf("TTL: %ds (auto-cleanup)\n", cfg.PodTTLSeconds)
	fmt.Println()

	// In real usage:
	// ws, err := workspace.NewK8sWorkspace(cfg)
	// backend := workspace.NewToolBackend(ws)
	// ctx = tool.WithBackend(ctx, backend)
	// agent := agent.NewUnifiedAgent("bot", "...", model, agent.WithToolkit(tk))
	// // All tool calls now execute inside the K8s Pod!
	// defer ws.Close()

	fmt.Println("(Skipping actual Pod creation — no cluster connected)")
	fmt.Println()

	// --- K8s Read-only Tools: cluster querying ---
	fmt.Println("=== K8s Cluster Tools Demo ===")
	fmt.Println()

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}

	getTool := workspace.NewKubectlGetTool(kubeconfig)
	logTool := workspace.NewKubectlLogTool(kubeconfig)

	fmt.Printf("kubectl_get tool: %s\n", getTool.Name())
	fmt.Printf("  Description: %s\n", getTool.Description())
	fmt.Printf("kubectl_logs tool: %s\n", logTool.Name())
	fmt.Printf("  Description: %s\n", logTool.Description())
	fmt.Println()

	// Example: query pods in kube-system (requires cluster access)
	fmt.Println("Example usage with an agent:")
	fmt.Println(`  agent calls kubectl_get(resource="pods", namespace="kube-system")`)
	fmt.Println(`  agent calls kubectl_logs(pod="coredns-xxx", namespace="kube-system", tail=20)`)
	fmt.Println()
	fmt.Println("These tools are read-only and integrate with the permission engine.")
	fmt.Println("Secrets are explicitly blocked from kubectl_get.")

	_ = ctx
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }
