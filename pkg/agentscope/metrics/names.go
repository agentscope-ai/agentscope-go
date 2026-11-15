package metrics

// Metric name constants following Prometheus naming conventions.
const (
	// ModelCallDuration is the histogram name for model call latency in seconds.
	ModelCallDuration = "agentscope_model_call_duration_seconds"
	// ModelCallTotal is the counter name for total model API calls.
	ModelCallTotal = "agentscope_model_call_total"
	// ModelCallErrors is the counter name for failed model API calls.
	ModelCallErrors = "agentscope_model_call_errors_total"
	// ToolExecDuration is the histogram name for tool execution latency in seconds.
	ToolExecDuration = "agentscope_tool_exec_duration_seconds"
	// ToolExecTotal is the counter name for total tool executions.
	ToolExecTotal = "agentscope_tool_exec_total"
	// ToolExecErrors is the counter name for failed tool executions.
	ToolExecErrors = "agentscope_tool_exec_errors_total"
	// TokensUsed is the counter name for total tokens consumed.
	TokensUsed = "agentscope_tokens_used_total"
	// LoopIterations is the counter name for total loop iterations.
	LoopIterations = "agentscope_loop_iterations_total"
	// ActiveLoops is the gauge name for currently active agent loops.
	ActiveLoops = "agentscope_active_loops"
)
