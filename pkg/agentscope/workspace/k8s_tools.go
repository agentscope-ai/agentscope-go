package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tool"
)

// allowedResources is the set of Kubernetes resource types that can be queried
// via KubectlGetTool. Secrets are explicitly excluded for security.
var allowedResources = map[string]bool{
	"pods":         true,
	"deployments":  true,
	"services":     true,
	"configmaps":   true,
	"events":       true,
	"nodes":        true,
	"namespaces":   true,
	"ingresses":    true,
	"jobs":         true,
	"cronjobs":     true,
	"statefulsets": true,
	"daemonsets":   true,
	"replicasets":  true,
	"pvc":          true,
	"hpa":          true,
}

// NewKubectlGetTool creates a read-only tool for querying Kubernetes resources.
func NewKubectlGetTool(kubeconfigPath string) *tool.FunctionTool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"resource": {
				"type": "string",
				"description": "Kubernetes resource type (e.g. pods, deployments, services)",
				"enum": ["pods","deployments","services","configmaps","events","nodes","namespaces","ingresses","jobs","cronjobs","statefulsets","daemonsets","replicasets","pvc","hpa"]
			},
			"namespace": {
				"type": "string",
				"description": "Kubernetes namespace (default: 'default')",
				"default": "default"
			},
			"name": {
				"type": "string",
				"description": "Specific resource name (optional)"
			},
			"selector": {
				"type": "string",
				"description": "Label selector (e.g. 'app=nginx')"
			},
			"output": {
				"type": "string",
				"description": "Output format",
				"enum": ["yaml","json","wide"],
				"default": "yaml"
			}
		},
		"required": ["resource"]
	}`)

	fn := func(ctx context.Context, input map[string]any) (any, error) {
		resource, _ := input["resource"].(string)
		if resource == "" {
			return nil, fmt.Errorf("resource is required")
		}

		// Block secrets explicitly.
		if strings.EqualFold(resource, "secrets") || strings.EqualFold(resource, "secret") {
			return nil, fmt.Errorf("access to secrets is not allowed")
		}

		if !allowedResources[strings.ToLower(resource)] {
			return nil, fmt.Errorf("resource %q is not in the allowed list", resource)
		}

		namespace := "default"
		if ns, ok := input["namespace"].(string); ok && ns != "" {
			namespace = ns
		}

		output := "yaml"
		if o, ok := input["output"].(string); ok && o != "" {
			output = o
		}

		// Build kubectl args.
		args := []string{"get", resource}

		if name, ok := input["name"].(string); ok && name != "" {
			args = append(args, name)
		}

		args = append(args, "-n", namespace, "-o", output)

		if selector, ok := input["selector"].(string); ok && selector != "" {
			args = append(args, "-l", selector)
		}

		if kubeconfigPath != "" {
			args = append(args, "--kubeconfig="+kubeconfigPath)
		}

		// Execute with timeout.
		execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		cmd := exec.CommandContext(execCtx, "kubectl", args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			if execCtx.Err() != nil {
				return "command timed out after 30s", nil
			}
			return fmt.Sprintf("Error: %s\n%s", err, stderr.String()), nil
		}

		return stdout.String(), nil
	}

	return tool.NewFunctionTool("kubectl_get", "Query Kubernetes resources (read-only)", schema, fn)
}

// NewKubectlLogTool creates a read-only tool for retrieving pod logs.
func NewKubectlLogTool(kubeconfigPath string) *tool.FunctionTool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"pod": {
				"type": "string",
				"description": "Pod name"
			},
			"namespace": {
				"type": "string",
				"description": "Kubernetes namespace (default: 'default')",
				"default": "default"
			},
			"container": {
				"type": "string",
				"description": "Container name (for multi-container pods)"
			},
			"tail": {
				"type": "integer",
				"description": "Number of lines from the end to show (default: 100)",
				"default": 100
			},
			"since": {
				"type": "string",
				"description": "Show logs since duration (e.g. '5m', '1h')"
			}
		},
		"required": ["pod"]
	}`)

	fn := func(ctx context.Context, input map[string]any) (any, error) {
		pod, _ := input["pod"].(string)
		if pod == "" {
			return nil, fmt.Errorf("pod name is required")
		}

		namespace := "default"
		if ns, ok := input["namespace"].(string); ok && ns != "" {
			namespace = ns
		}

		tail := 100
		if t, ok := input["tail"].(float64); ok && t > 0 {
			tail = int(t)
		}

		// Build kubectl args.
		args := []string{"logs", pod, "-n", namespace, "--tail=" + strconv.Itoa(tail)}

		if container, ok := input["container"].(string); ok && container != "" {
			args = append(args, "-c", container)
		}

		if since, ok := input["since"].(string); ok && since != "" {
			args = append(args, "--since="+since)
		}

		if kubeconfigPath != "" {
			args = append(args, "--kubeconfig="+kubeconfigPath)
		}

		// Execute with timeout.
		execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		cmd := exec.CommandContext(execCtx, "kubectl", args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			if execCtx.Err() != nil {
				return "command timed out after 30s", nil
			}
			return fmt.Sprintf("Error: %s\n%s", err, stderr.String()), nil
		}

		return stdout.String(), nil
	}

	return tool.NewFunctionTool("kubectl_logs", "Read pod logs from Kubernetes (read-only)", schema, fn)
}
