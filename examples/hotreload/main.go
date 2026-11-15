package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/hotreload"
)

// This example demonstrates hot-reload of configuration files.
// It shows how to:
// 1. Define a typed config struct
// 2. Write initial config to a temp file
// 3. Create a Watcher and a generic Reloader[AppConfig]
// 4. Detect config changes via ForceCheck
// 5. See the updated config values without restarting

// AppConfig represents application configuration that can be reloaded at runtime.
type AppConfig struct {
	ModelName    string  `json:"model_name"`
	Temperature  float64 `json:"temperature"`
	MaxTokens    int     `json:"max_tokens"`
	SystemPrompt string  `json:"system_prompt"`
}

func main() {
	fmt.Println("=== Hot-Reload Config Example ===")
	fmt.Println()

	// Step 1: Write initial config to a temp file.
	tmpDir := os.TempDir()
	configPath := filepath.Join(tmpDir, "agentscope-hotreload-example.json")

	initialConfig := AppConfig{
		ModelName:    "gpt-4o-mini",
		Temperature:  0.7,
		MaxTokens:    1024,
		SystemPrompt: "You are a helpful assistant.",
	}

	if err := writeConfig(configPath, &initialConfig); err != nil {
		fmt.Println("Error writing initial config:", err)
		return
	}
	defer os.Remove(configPath)

	fmt.Printf("Config file: %s\n", configPath)
	fmt.Println()

	// Step 2: Create a Watcher with a fast poll interval (for demo purposes).
	watcher := hotreload.NewWatcher(hotreload.WatcherConfig{
		PollInterval: 100 * time.Millisecond,
	})

	// Step 3: Create a typed Reloader with an onChange callback.
	reloader, err := hotreload.NewReloader[AppConfig](watcher, configPath,
		hotreload.WithOnChange(func(old, new_ *AppConfig) {
			fmt.Printf("  [callback] Config changed!\n")
			fmt.Printf("    ModelName: %q -> %q\n", old.ModelName, new_.ModelName)
			fmt.Printf("    Temperature: %.1f -> %.1f\n", old.Temperature, new_.Temperature)
			fmt.Printf("    MaxTokens: %d -> %d\n", old.MaxTokens, new_.MaxTokens)
		}),
	)
	if err != nil {
		fmt.Println("Error creating reloader:", err)
		return
	}

	// Print initial config.
	cfg := reloader.Get()
	fmt.Println("--- Initial Config ---")
	printConfig(cfg)
	fmt.Println()

	// Step 4: Modify the config file (simulate an operator changing settings).
	fmt.Println("Modifying config file (changing model and temperature)...")
	updatedConfig := AppConfig{
		ModelName:    "claude-sonnet-4-20250514",
		Temperature:  0.3,
		MaxTokens:    2048,
		SystemPrompt: "You are a concise technical assistant.",
	}
	if err := writeConfig(configPath, &updatedConfig); err != nil {
		fmt.Println("Error writing updated config:", err)
		return
	}

	// Step 5: Force a check to pick up the change immediately.
	// In production, the watcher.Start() goroutine handles this automatically.
	time.Sleep(10 * time.Millisecond) // ensure filesystem catches up
	errs := watcher.ForceCheck()
	if len(errs) > 0 {
		fmt.Println("Errors during check:", errs)
	}
	fmt.Println()

	// Read the updated config.
	cfg = reloader.Get()
	fmt.Println("--- Updated Config ---")
	printConfig(cfg)
	fmt.Println()

	// Verify the change was picked up.
	if cfg.ModelName == updatedConfig.ModelName {
		fmt.Println("SUCCESS: Config hot-reloaded without restart!")
	} else {
		fmt.Println("UNEXPECTED: Config did not update.")
	}
}

func writeConfig(path string, cfg *AppConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func printConfig(cfg *AppConfig) {
	fmt.Printf("  ModelName:    %s\n", cfg.ModelName)
	fmt.Printf("  Temperature:  %.1f\n", cfg.Temperature)
	fmt.Printf("  MaxTokens:    %d\n", cfg.MaxTokens)
	fmt.Printf("  SystemPrompt: %s\n", cfg.SystemPrompt)
}
