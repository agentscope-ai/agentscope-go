package workspace

import (
	"encoding/json"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func TestK8sManifest_PodSpec(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg := &K8sConfig{
			PodName:       "test-pod",
			Image:         "",
			PodTTLSeconds: 0, // should default
		}
		// Apply defaults as NewK8sWorkspace would.
		if cfg.Image == "" {
			cfg.Image = "ubuntu:22.04"
		}
		if cfg.ImagePullPolicy == "" {
			cfg.ImagePullPolicy = "IfNotPresent"
		}
		if cfg.PodTTLSeconds == 0 {
			cfg.PodTTLSeconds = 3600
		}

		w := &K8sWorkspace{
			cfg:       cfg,
			namespace: "default",
			podName:   cfg.PodName,
		}

		manifest, err := w.buildPodManifest(cfg)
		if err != nil {
			t.Fatalf("buildPodManifest: %v", err)
		}

		var pod map[string]any
		if err := json.Unmarshal(manifest, &pod); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		spec := pod["spec"].(map[string]any)
		containers := spec["containers"].([]any)
		c := containers[0].(map[string]any)

		// Default image.
		if got := c["image"].(string); got != "ubuntu:22.04" {
			t.Errorf("image = %q, want %q", got, "ubuntu:22.04")
		}

		// imagePullPolicy default.
		if got := c["imagePullPolicy"].(string); got != "IfNotPresent" {
			t.Errorf("imagePullPolicy = %q, want %q", got, "IfNotPresent")
		}

		// activeDeadlineSeconds.
		ads := spec["activeDeadlineSeconds"].(float64)
		if ads != 3600 {
			t.Errorf("activeDeadlineSeconds = %v, want 3600", ads)
		}
	})

	t.Run("disable_service_account", func(t *testing.T) {
		cfg := &K8sConfig{
			PodName:               "test-pod",
			Image:                 "alpine:3.18",
			ImagePullPolicy:       "Always",
			PodTTLSeconds:         1800,
			DisableServiceAccount: true,
		}

		w := &K8sWorkspace{
			cfg:       cfg,
			namespace: "default",
			podName:   cfg.PodName,
		}

		manifest, err := w.buildPodManifest(cfg)
		if err != nil {
			t.Fatalf("buildPodManifest: %v", err)
		}

		var pod map[string]any
		if err := json.Unmarshal(manifest, &pod); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		spec := pod["spec"].(map[string]any)

		// automountServiceAccountToken should be false.
		val, ok := spec["automountServiceAccountToken"]
		if !ok {
			t.Fatal("automountServiceAccountToken not set")
		}
		if val.(bool) != false {
			t.Errorf("automountServiceAccountToken = %v, want false", val)
		}
	})

	t.Run("custom_image_pull_policy", func(t *testing.T) {
		cfg := &K8sConfig{
			PodName:         "test-pod",
			Image:           "myimage:latest",
			ImagePullPolicy: "Always",
			PodTTLSeconds:   7200,
		}

		w := &K8sWorkspace{
			cfg:       cfg,
			namespace: "default",
			podName:   cfg.PodName,
		}

		manifest, err := w.buildPodManifest(cfg)
		if err != nil {
			t.Fatalf("buildPodManifest: %v", err)
		}

		var pod map[string]any
		if err := json.Unmarshal(manifest, &pod); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		spec := pod["spec"].(map[string]any)
		containers := spec["containers"].([]any)
		c := containers[0].(map[string]any)

		if got := c["imagePullPolicy"].(string); got != "Always" {
			t.Errorf("imagePullPolicy = %q, want %q", got, "Always")
		}

		ads := spec["activeDeadlineSeconds"].(float64)
		if ads != 7200 {
			t.Errorf("activeDeadlineSeconds = %v, want 7200", ads)
		}
	})

	t.Run("security_context", func(t *testing.T) {
		trueVal := true
		uid := int64(1000)
		gid := int64(1000)
		fsGroup := int64(2000)

		cfg := &K8sConfig{
			PodName:         "test-pod",
			Image:           "ubuntu:22.04",
			ImagePullPolicy: "IfNotPresent",
			PodTTLSeconds:   3600,
			SecurityContext: &PodSecurityContext{
				RunAsNonRoot: &trueVal,
				RunAsUser:    &uid,
				RunAsGroup:   &gid,
				FSGroup:      &fsGroup,
			},
		}

		w := &K8sWorkspace{
			cfg:       cfg,
			namespace: "default",
			podName:   cfg.PodName,
		}

		manifest, err := w.buildPodManifest(cfg)
		if err != nil {
			t.Fatalf("buildPodManifest: %v", err)
		}

		var pod map[string]any
		if err := json.Unmarshal(manifest, &pod); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		spec := pod["spec"].(map[string]any)
		sc, ok := spec["securityContext"].(map[string]any)
		if !ok {
			t.Fatal("securityContext not set")
		}

		if v := sc["runAsNonRoot"].(bool); v != true {
			t.Errorf("runAsNonRoot = %v, want true", v)
		}
		if v := sc["runAsUser"].(float64); v != 1000 {
			t.Errorf("runAsUser = %v, want 1000", v)
		}
		if v := sc["runAsGroup"].(float64); v != 1000 {
			t.Errorf("runAsGroup = %v, want 1000", v)
		}
		if v := sc["fsGroup"].(float64); v != 2000 {
			t.Errorf("fsGroup = %v, want 2000", v)
		}
	})

	t.Run("resources", func(t *testing.T) {
		cfg := &K8sConfig{
			PodName:         "test-pod",
			Image:           "ubuntu:22.04",
			ImagePullPolicy: "IfNotPresent",
			PodTTLSeconds:   3600,
			Resources: &ResourceRequirements{
				CPULimit:      "2000m",
				MemoryLimit:   "1Gi",
				CPURequest:    "500m",
				MemoryRequest: "256Mi",
			},
		}

		w := &K8sWorkspace{
			cfg:       cfg,
			namespace: "default",
			podName:   cfg.PodName,
		}

		manifest, err := w.buildPodManifest(cfg)
		if err != nil {
			t.Fatalf("buildPodManifest: %v", err)
		}

		var pod map[string]any
		if err := json.Unmarshal(manifest, &pod); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		spec := pod["spec"].(map[string]any)
		containers := spec["containers"].([]any)
		c := containers[0].(map[string]any)

		res, ok := c["resources"].(map[string]any)
		if !ok {
			t.Fatal("resources not set")
		}

		limits := res["limits"].(map[string]any)
		if limits["cpu"] != "2000m" {
			t.Errorf("cpu limit = %v, want 2000m", limits["cpu"])
		}
		if limits["memory"] != "1Gi" {
			t.Errorf("memory limit = %v, want 1Gi", limits["memory"])
		}

		requests := res["requests"].(map[string]any)
		if requests["cpu"] != "500m" {
			t.Errorf("cpu request = %v, want 500m", requests["cpu"])
		}
		if requests["memory"] != "256Mi" {
			t.Errorf("memory request = %v, want 256Mi", requests["memory"])
		}
	})

	t.Run("labels_and_annotations", func(t *testing.T) {
		cfg := &K8sConfig{
			PodName:         "test-pod",
			Image:           "ubuntu:22.04",
			ImagePullPolicy: "IfNotPresent",
			PodTTLSeconds:   3600,
			Labels:          map[string]string{"app": "agent", "team": "platform"},
			Annotations:     map[string]string{"note": "test"},
		}

		w := &K8sWorkspace{
			cfg:       cfg,
			namespace: "custom-ns",
			podName:   cfg.PodName,
		}

		manifest, err := w.buildPodManifest(cfg)
		if err != nil {
			t.Fatalf("buildPodManifest: %v", err)
		}

		var pod map[string]any
		if err := json.Unmarshal(manifest, &pod); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		metadata := pod["metadata"].(map[string]any)
		labels := metadata["labels"].(map[string]any)
		if labels["app"] != "agent" {
			t.Errorf("label app = %v, want agent", labels["app"])
		}
		if labels["team"] != "platform" {
			t.Errorf("label team = %v, want platform", labels["team"])
		}

		annotations := metadata["annotations"].(map[string]any)
		if annotations["note"] != "test" {
			t.Errorf("annotation note = %v, want test", annotations["note"])
		}

		// Namespace check.
		if metadata["namespace"] != "custom-ns" {
			t.Errorf("namespace = %v, want custom-ns", metadata["namespace"])
		}
	})

	t.Run("pvc_volume_mounts", func(t *testing.T) {
		cfg := &K8sConfig{
			PodName:         "test-pod",
			Image:           "ubuntu:22.04",
			ImagePullPolicy: "IfNotPresent",
			PodTTLSeconds:   3600,
			PVCName:         "my-pvc",
		}

		w := &K8sWorkspace{
			cfg:       cfg,
			namespace: "default",
			podName:   cfg.PodName,
		}

		manifest, err := w.buildPodManifest(cfg)
		if err != nil {
			t.Fatalf("buildPodManifest: %v", err)
		}

		var pod map[string]any
		if err := json.Unmarshal(manifest, &pod); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		spec := pod["spec"].(map[string]any)

		// Check volumes.
		volumes := spec["volumes"].([]any)
		if len(volumes) != 1 {
			t.Fatalf("volumes count = %d, want 1", len(volumes))
		}
		vol := volumes[0].(map[string]any)
		pvc := vol["persistentVolumeClaim"].(map[string]any)
		if pvc["claimName"] != "my-pvc" {
			t.Errorf("pvc claimName = %v, want my-pvc", pvc["claimName"])
		}

		// Check volume mounts.
		containers := spec["containers"].([]any)
		c := containers[0].(map[string]any)
		mounts := c["volumeMounts"].([]any)
		if len(mounts) != 1 {
			t.Fatalf("volumeMounts count = %d, want 1", len(mounts))
		}
		m := mounts[0].(map[string]any)
		if m["mountPath"] != "/workspace" {
			t.Errorf("mountPath = %v, want /workspace", m["mountPath"])
		}
	})

	t.Run("service_account_name", func(t *testing.T) {
		cfg := &K8sConfig{
			PodName:            "test-pod",
			Image:              "ubuntu:22.04",
			ImagePullPolicy:    "IfNotPresent",
			PodTTLSeconds:      3600,
			ServiceAccountName: "custom-sa",
		}

		w := &K8sWorkspace{
			cfg:       cfg,
			namespace: "default",
			podName:   cfg.PodName,
		}

		manifest, err := w.buildPodManifest(cfg)
		if err != nil {
			t.Fatalf("buildPodManifest: %v", err)
		}

		var pod map[string]any
		if err := json.Unmarshal(manifest, &pod); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		spec := pod["spec"].(map[string]any)
		if spec["serviceAccountName"] != "custom-sa" {
			t.Errorf("serviceAccountName = %v, want custom-sa", spec["serviceAccountName"])
		}
	})
}

func TestK8sConfig_Validation(t *testing.T) {
	_, err := NewK8sWorkspace(&K8sConfig{
		PodName: "",
	})
	if err == nil {
		t.Fatal("expected error for empty PodName")
	}
	if got := err.Error(); got != "k8s workspace: pod name is required" {
		t.Errorf("error = %q, want %q", got, "k8s workspace: pod name is required")
	}
}

func TestK8sConfig_SecretToken(t *testing.T) {
	// Verify that SecretToken is resolved (just check it compiles and the field exists).
	cfg := &K8sConfig{
		PodName:     "test",
		Token:       "plain-token",
		SecretToken: model.NewSecretStr("secret-token"),
	}
	// SecretToken should take precedence.
	resolved := model.ResolveAPIKey(cfg.Token, cfg.SecretToken)
	if resolved != "secret-token" {
		t.Errorf("ResolveAPIKey = %q, want %q", resolved, "secret-token")
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "'simple'"},
		{"with spaces", "'with spaces'"},
		{"with'quote", `'with'\''quote'`},
		{"back`tick", "'back`tick'"},
		{"semi;colon", "'semi;colon'"},
		{`double"quote`, `'double"quote'`},
		{"$variable", "'$variable'"},
		{"new\nline", "'new\nline'"},
		{"", "''"},
		{"a'b'c", `'a'\''b'\''c'`},
	}

	for _, tt := range tests {
		got := shellQuote(tt.input)
		if got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
