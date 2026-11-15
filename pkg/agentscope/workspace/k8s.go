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

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// PodSecurityContext configures the pod-level security context.
type PodSecurityContext struct {
	RunAsNonRoot *bool // pointer to allow explicit false
	RunAsUser    *int64
	RunAsGroup   *int64
	FSGroup      *int64
}

// ResourceRequirements configures container resource requests and limits.
type ResourceRequirements struct {
	CPULimit      string // e.g. "1000m"
	MemoryLimit   string // e.g. "512Mi"
	CPURequest    string // e.g. "100m"
	MemoryRequest string // e.g. "128Mi"
}

// K8sConfig configures a Kubernetes Pod-based workspace.
type K8sConfig struct {
	APIServer      string          // e.g. "https://kubernetes.default.svc"
	Token          string          // bearer token (plain text, deprecated: prefer SecretToken)
	SecretToken    model.SecretStr // preferred over Token; use model.NewSecretStr(tok)
	CACert         string          // path to CA cert file (optional)
	Namespace      string          // default "default"
	PodName        string
	Image          string        // default "ubuntu:22.04"
	PVCName        string        // optional, for persistent storage
	CommandTimeout time.Duration // default: 60s
	TLSInsecure    bool          // skip TLS verification (for testing)

	// Pod hardening
	ImagePullPolicy       string // default "IfNotPresent"
	DisableServiceAccount bool   // when true, sets automountServiceAccountToken: false
	PodTTLSeconds         int64  // activeDeadlineSeconds; default 3600 (1h)

	// Security
	SecurityContext    *PodSecurityContext // optional; when set, applied to pod spec
	ServiceAccountName string              // optional; defaults to "" (use default SA)

	// Resources
	Resources *ResourceRequirements // optional; when set, applied to container spec

	// Metadata
	Labels      map[string]string
	Annotations map[string]string
}

// K8sWorkspace manages a Kubernetes Pod with optional PVC for sandboxed execution.
// It shells out to kubectl for all interactions with the cluster.
type K8sWorkspace struct {
	cfg            *K8sConfig
	namespace      string
	podName        string
	pvcName        string
	commandTimeout time.Duration
	kubectlArgs    []string // base args for kubectl invocations
}

// Compile-time interface check.
var _ Workspace = (*K8sWorkspace)(nil)

// NewK8sWorkspace creates a Pod (and optionally a PVC) for workspace operations.
func NewK8sWorkspace(cfg *K8sConfig) (*K8sWorkspace, error) {
	if cfg.PodName == "" {
		return nil, fmt.Errorf("k8s workspace: pod name is required")
	}

	ns := cfg.Namespace
	if ns == "" {
		ns = "default"
	}
	if cfg.Image == "" {
		cfg.Image = "ubuntu:22.04"
	}
	timeout := cfg.CommandTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	if cfg.ImagePullPolicy == "" {
		cfg.ImagePullPolicy = "IfNotPresent"
	}
	if cfg.PodTTLSeconds == 0 {
		cfg.PodTTLSeconds = 3600
	}

	// Resolve token: prefer SecretToken over plain Token.
	resolvedToken := model.ResolveAPIKey(cfg.Token, cfg.SecretToken)

	// Build base kubectl args.
	var baseArgs []string
	if cfg.APIServer != "" {
		baseArgs = append(baseArgs, "--server="+cfg.APIServer)
	}
	if resolvedToken != "" {
		baseArgs = append(baseArgs, "--token="+resolvedToken)
	}
	if cfg.CACert != "" {
		baseArgs = append(baseArgs, "--certificate-authority="+cfg.CACert)
	}
	if cfg.TLSInsecure {
		baseArgs = append(baseArgs, "--insecure-skip-tls-verify=true")
	}
	baseArgs = append(baseArgs, "-n", ns)

	w := &K8sWorkspace{
		cfg:            cfg,
		namespace:      ns,
		podName:        cfg.PodName,
		pvcName:        cfg.PVCName,
		commandTimeout: timeout,
		kubectlArgs:    baseArgs,
	}

	// Create PVC if specified and doesn't already exist.
	if cfg.PVCName != "" {
		if err := w.ensurePVC(cfg.PVCName); err != nil {
			return nil, fmt.Errorf("k8s workspace: create PVC: %w", err)
		}
	}

	// Create Pod.
	if err := w.createPod(cfg); err != nil {
		return nil, fmt.Errorf("k8s workspace: create pod: %w", err)
	}

	// Wait for Pod to be Running.
	if err := w.waitForPod(cfg.PodName); err != nil {
		return nil, fmt.Errorf("k8s workspace: wait for pod: %w", err)
	}

	return w, nil
}

// BasePath returns the workspace mount path inside the pod.
func (w *K8sWorkspace) BasePath() string { return "/workspace" }

// Execute runs a command inside the pod via kubectl exec.
func (w *K8sWorkspace) Execute(ctx context.Context, command string) (*ExecResult, error) {
	args := w.baseKubectlArgsCopy()
	args = append(args, "exec", w.podName, "--", "sh", "-c", command)
	return w.runKubectl(ctx, args, nil)
}

// WriteFile writes data to a file inside the pod using kubectl exec with tee.
func (w *K8sWorkspace) WriteFile(ctx context.Context, path string, data []byte) error {
	// Ensure parent directory exists.
	if idx := strings.LastIndex(path, "/"); idx > 0 {
		dir := path[:idx]
		mkdirArgs := w.baseKubectlArgsCopy()
		mkdirArgs = append(mkdirArgs, "exec", w.podName, "--", "mkdir", "-p", dir)
		if _, err := w.runKubectl(ctx, mkdirArgs, nil); err != nil {
			return fmt.Errorf("k8s workspace: mkdir: %w", err)
		}
	}

	// Write file content via stdin piped to tee.
	args := w.baseKubectlArgsCopy()
	args = append(args, "exec", "-i", w.podName, "--", "tee", path)
	result, err := w.runKubectl(ctx, args, data)
	if err != nil {
		return fmt.Errorf("k8s workspace: write file: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("k8s workspace: write file failed: %s", result.Stderr)
	}
	return nil
}

// ReadFile reads a file from the pod via kubectl exec cat.
func (w *K8sWorkspace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	args := w.baseKubectlArgsCopy()
	args = append(args, "exec", w.podName, "--", "cat", path)
	result, err := w.runKubectl(ctx, args, nil)
	if err != nil {
		return nil, fmt.Errorf("k8s workspace: read file: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("k8s workspace: read file: %s", result.Stderr)
	}
	return []byte(result.Stdout), nil
}

// ListFiles lists directory contents inside the pod.
func (w *K8sWorkspace) ListFiles(ctx context.Context, dir string) ([]FileInfo, error) {
	// Use portable find (no GNU -printf) to list files with type.
	cmd := fmt.Sprintf("find %s -maxdepth 1 -mindepth 1 -exec sh -c 'for f; do if [ -d \"$f\" ]; then echo \"d\\t0\\t$(basename \"$f\")\"; else size=$(wc -c < \"$f\" 2>/dev/null || echo 0); echo \"f\\t${size}\\t$(basename \"$f\")\"; fi; done' _ {} +", shellQuote(dir))
	args := w.baseKubectlArgsCopy()
	args = append(args, "exec", w.podName, "--", "sh", "-c", cmd)
	result, err := w.runKubectl(ctx, args, nil)
	if err != nil {
		return nil, fmt.Errorf("k8s workspace: list files: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("k8s workspace: list files: %s", result.Stderr)
	}

	var files []FileInfo
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		fileType := parts[0]
		size, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		name := parts[2]
		isDir := fileType == "d"

		entryPath := strings.TrimRight(dir, "/") + "/" + name

		files = append(files, FileInfo{
			Name:  name,
			Path:  entryPath,
			IsDir: isDir,
			Size:  size,
		})
	}
	return files, nil
}

// RemoveFile removes a file from the pod.
func (w *K8sWorkspace) RemoveFile(ctx context.Context, path string) error {
	args := w.baseKubectlArgsCopy()
	args = append(args, "exec", w.podName, "--", "rm", path)
	result, err := w.runKubectl(ctx, args, nil)
	if err != nil {
		return fmt.Errorf("k8s workspace: remove file: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("k8s workspace: remove file: %s", result.Stderr)
	}
	return nil
}

// Close deletes the Pod and optionally the PVC.
func (w *K8sWorkspace) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Delete the pod.
	args := w.baseKubectlArgsCopy()
	args = append(args, "delete", "pod", w.podName, "--grace-period=5")
	if _, err := w.runKubectl(ctx, args, nil); err != nil {
		return fmt.Errorf("k8s workspace: delete pod: %w", err)
	}

	// Delete PVC if one was managed by this workspace.
	if w.pvcName != "" {
		pvcArgs := w.baseKubectlArgsCopy()
		pvcArgs = append(pvcArgs, "delete", "pvc", w.pvcName)
		if _, err := w.runKubectl(ctx, pvcArgs, nil); err != nil {
			return fmt.Errorf("k8s workspace: delete pvc: %w", err)
		}
	}

	return nil
}

// ensurePVC creates a PVC if it does not already exist.
func (w *K8sWorkspace) ensurePVC(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Check if PVC already exists.
	args := w.baseKubectlArgsCopy()
	args = append(args, "get", "pvc", name, "--ignore-not-found", "-o", "name")
	result, err := w.runKubectl(ctx, args, nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(result.Stdout) != "" {
		return nil // PVC already exists.
	}

	// Create PVC via kubectl apply.
	manifest := fmt.Sprintf(`{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":%q,"namespace":%q},"spec":{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"1Gi"}}}}`, name, w.namespace)

	applyArgs := w.baseKubectlArgsCopy()
	applyArgs = append(applyArgs, "apply", "-f", "-")
	result, err = w.runKubectl(ctx, applyArgs, []byte(manifest))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("kubectl apply pvc: %s", result.Stderr)
	}
	return nil
}

// buildPodManifest constructs the JSON manifest for the workspace pod.
// Extracted for testability.
func (w *K8sWorkspace) buildPodManifest(cfg *K8sConfig) ([]byte, error) {
	// Container definition.
	container := map[string]any{
		"name":            "workspace",
		"image":           cfg.Image,
		"command":         []string{"sleep", "infinity"},
		"workingDir":      "/workspace",
		"imagePullPolicy": cfg.ImagePullPolicy,
	}

	// Volume mounts.
	if cfg.PVCName != "" {
		container["volumeMounts"] = []map[string]any{
			{"name": "workspace-vol", "mountPath": "/workspace"},
		}
	}

	// Resource requirements.
	if cfg.Resources != nil {
		resources := map[string]map[string]string{}
		if cfg.Resources.CPULimit != "" || cfg.Resources.MemoryLimit != "" {
			limits := map[string]string{}
			if cfg.Resources.CPULimit != "" {
				limits["cpu"] = cfg.Resources.CPULimit
			}
			if cfg.Resources.MemoryLimit != "" {
				limits["memory"] = cfg.Resources.MemoryLimit
			}
			resources["limits"] = limits
		}
		if cfg.Resources.CPURequest != "" || cfg.Resources.MemoryRequest != "" {
			requests := map[string]string{}
			if cfg.Resources.CPURequest != "" {
				requests["cpu"] = cfg.Resources.CPURequest
			}
			if cfg.Resources.MemoryRequest != "" {
				requests["memory"] = cfg.Resources.MemoryRequest
			}
			resources["requests"] = requests
		}
		if len(resources) > 0 {
			container["resources"] = resources
		}
	}

	// Pod spec.
	spec := map[string]any{
		"containers":            []any{container},
		"restartPolicy":         "Never",
		"activeDeadlineSeconds": cfg.PodTTLSeconds,
	}

	// Volumes.
	if cfg.PVCName != "" {
		spec["volumes"] = []map[string]any{
			{"name": "workspace-vol", "persistentVolumeClaim": map[string]any{"claimName": cfg.PVCName}},
		}
	}

	// Service account.
	if cfg.DisableServiceAccount {
		spec["automountServiceAccountToken"] = false
	}
	if cfg.ServiceAccountName != "" {
		spec["serviceAccountName"] = cfg.ServiceAccountName
	}

	// Pod security context.
	if cfg.SecurityContext != nil {
		sc := map[string]any{}
		if cfg.SecurityContext.RunAsNonRoot != nil {
			sc["runAsNonRoot"] = *cfg.SecurityContext.RunAsNonRoot
		}
		if cfg.SecurityContext.RunAsUser != nil {
			sc["runAsUser"] = *cfg.SecurityContext.RunAsUser
		}
		if cfg.SecurityContext.RunAsGroup != nil {
			sc["runAsGroup"] = *cfg.SecurityContext.RunAsGroup
		}
		if cfg.SecurityContext.FSGroup != nil {
			sc["fsGroup"] = *cfg.SecurityContext.FSGroup
		}
		if len(sc) > 0 {
			spec["securityContext"] = sc
		}
	}

	// Metadata.
	metadata := map[string]any{
		"name":      cfg.PodName,
		"namespace": w.namespace,
	}
	if len(cfg.Labels) > 0 {
		metadata["labels"] = cfg.Labels
	}
	if len(cfg.Annotations) > 0 {
		metadata["annotations"] = cfg.Annotations
	}

	pod := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   metadata,
		"spec":       spec,
	}

	return json.Marshal(pod)
}

// createPod creates the workspace pod if it doesn't already exist.
func (w *K8sWorkspace) createPod(cfg *K8sConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Check if pod already exists and is usable.
	args := w.baseKubectlArgsCopy()
	args = append(args, "get", "pod", cfg.PodName, "--ignore-not-found", "-o", "jsonpath={.status.phase}")
	result, err := w.runKubectl(ctx, args, nil)
	if err != nil {
		return err
	}
	phase := strings.TrimSpace(result.Stdout)
	if phase == "Running" || phase == "Pending" {
		return nil // Pod already exists.
	}

	// Build pod manifest.
	manifest, err := w.buildPodManifest(cfg)
	if err != nil {
		return fmt.Errorf("build manifest: %w", err)
	}

	applyArgs := w.baseKubectlArgsCopy()
	applyArgs = append(applyArgs, "apply", "-f", "-")
	result, err = w.runKubectl(ctx, applyArgs, manifest)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("kubectl apply pod: %s", result.Stderr)
	}
	return nil
}

// waitForPod polls until the pod is Running or times out.
func (w *K8sWorkspace) waitForPod(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	args := w.baseKubectlArgsCopy()
	args = append(args, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=300s")
	result, err := w.runKubectl(ctx, args, nil)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("pod not ready: %s", result.Stderr)
	}
	return nil
}

// baseKubectlArgsCopy returns a fresh copy of the base kubectl arguments.
func (w *K8sWorkspace) baseKubectlArgsCopy() []string {
	args := make([]string, len(w.kubectlArgs))
	copy(args, w.kubectlArgs)
	return args
}

// runKubectl executes a kubectl command and returns the result.
func (w *K8sWorkspace) runKubectl(ctx context.Context, args []string, stdin []byte) (*ExecResult, error) {
	// Only apply commandTimeout if the parent ctx has no deadline already.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && w.commandTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, w.commandTimeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := &ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else if ctx.Err() != nil {
			result.ExitCode = -1
			result.Stderr += "\ncommand timed out"
		} else {
			return nil, fmt.Errorf("kubectl: %w", err)
		}
	}

	return result, nil
}

// shellQuote wraps a string in single quotes for safe shell interpolation.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
