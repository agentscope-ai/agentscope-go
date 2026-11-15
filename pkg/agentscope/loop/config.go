// pkg/agentscope/loop/config.go
package loop

import (
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// Config holds the loop's configuration.
type Config struct {
	ModelCaller    ModelCaller
	ToolExecutor   ToolExecutor
	ContextManager ContextManager
	SchemaProvider ToolSchemaProvider
	Hooks          *HookRunner
	MaxIters       int
	SystemPrompt   string
	ExitCondition  func(resp *model.ChatResponse) bool
	ErrorHandler   func(err error) ErrorAction
}

// ErrorAction tells the loop what to do when a model call fails.
type ErrorAction int

const (
	ErrorActionBreak    ErrorAction = iota // Stop the loop
	ErrorActionContinue                    // Skip this iteration and continue
	ErrorActionRetry                       // Retry the same iteration
)

// Option configures a Loop.
type Option func(*Config)

// WithModelCaller sets the ModelCaller used for LLM API calls.
func WithModelCaller(mc ModelCaller) Option {
	return func(c *Config) { c.ModelCaller = mc }
}

// WithToolExecutor sets the ToolExecutor used for tool invocations.
func WithToolExecutor(te ToolExecutor) Option {
	return func(c *Config) { c.ToolExecutor = te }
}

// WithContextManager sets the ContextManager that maintains conversation history.
func WithContextManager(cm ContextManager) Option {
	return func(c *Config) { c.ContextManager = cm }
}

// WithSchemaProvider sets the provider of tool schemas for model calls.
func WithSchemaProvider(sp ToolSchemaProvider) Option {
	return func(c *Config) { c.SchemaProvider = sp }
}

// WithHooks installs lifecycle hooks that receive notifications at key loop events.
func WithHooks(hooks ...Hook) Option {
	return func(c *Config) { c.Hooks = NewHookRunner(hooks...) }
}

// WithMaxIters sets the maximum number of reasoning iterations before the loop exits.
func WithMaxIters(n int) Option {
	return func(c *Config) { c.MaxIters = n }
}

// WithSystemPrompt sets the system prompt prepended to the conversation.
func WithSystemPrompt(prompt string) Option {
	return func(c *Config) { c.SystemPrompt = prompt }
}

// WithExitCondition sets a predicate that, when true, causes the loop to exit
// after inspecting the model response.
func WithExitCondition(fn func(resp *model.ChatResponse) bool) Option {
	return func(c *Config) { c.ExitCondition = fn }
}

// WithErrorHandler sets a callback that decides how to handle model call errors.
func WithErrorHandler(fn func(err error) ErrorAction) Option {
	return func(c *Config) { c.ErrorHandler = fn }
}

func defaultConfig() *Config {
	return &Config{
		MaxIters: 25,
		Hooks:    NewHookRunner(),
	}
}
