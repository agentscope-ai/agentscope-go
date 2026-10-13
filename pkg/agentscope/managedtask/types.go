// Package managedtask provides experimental, opt-in execution of one fixed
// text-only, non-streaming, tool-disabled deployment. It is independent of Agent
// and classifier routing. Share an Executor to share single-process admission.
package managedtask

import (
	"context"
	"time"

	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// Mode distinguishes ordinary production tasks from explicitly isolated evaluation.
type Mode string

const (
	Production Mode = "production"
	Evaluation Mode = "evaluation"
)

// Task contains trusted host identity/category and untrusted plain text. Empty
// Mode means Production. The host must enforce evaluation isolation, authorize
// destinations and project text; task text cannot grant itself permissions.
type Task struct {
	ID, Category, Text string
	Mode               Mode
}

// Deployment binds host capabilities to an actual adapter endpoint and version.
// ContextWindow is the configured server window, not a model-card estimate.
type Deployment struct {
	ID, Endpoint, Model                   string
	ContextWindow                         int
	TextOnly, NonStreaming, ToolsDisabled bool
	Qualifications                        []Qualification
}

// Qualification is a host assertion of task-category quality evidence. The
// library checks completeness and exact binding, not scientific validity or
// revocation. Authorize must recheck current host policy, including revocation.
type Qualification struct {
	DeploymentID, Endpoint, Model, Category   string
	EvidenceID, DatasetVersion, RubricVersion string
	SampleSize                                int
	MaxQualityLoss                            float64 // permitted fractional loss in [0,1]
	MaxLatency                                time.Duration
}

// Limits are finite per-executor and per-task caps. MaxTasks includes callbacks,
// queues, retries and cleanup. MaxQueued may be zero; all other fields must be
// positive. MaxAttempts counts actual transport dispatch starts, including errors.
type Limits struct {
	MaxTasks, MaxQueued, MaxConcurrent, MaxAttempts    int
	MaxRequestBytes, MaxResponseBytes, MaxOutputTokens int
	TaskTimeout, QueueTimeout, RetryBackoff            time.Duration
}

// DefaultLimits returns conservative finite defaults. Retry backoff is fixed;
// provider Retry-After can extend it, always within the task deadline.
func DefaultLimits() Limits {
	return Limits{MaxTasks: 32, MaxQueued: 16, MaxConcurrent: 4, MaxAttempts: 2, MaxRequestBytes: 1 << 20, MaxResponseBytes: 4 << 20, MaxOutputTokens: 512, TaskTimeout: 30 * time.Second, QueueTimeout: 5 * time.Second, RetryBackoff: 100 * time.Millisecond}
}

// Prices are optional host-supplied per-token rates in one currency. Nil rates
// mean unknown. Without CachedInputPerToken, InputPerToken is a flat rate over
// all prompt tokens; with it, cached/noncached input require a known cache count.
// OutputPerToken must cover all completion tokens, including reasoning. This
// reports best-effort arithmetic, not billing reconciliation or a budget bound.
type Prices struct {
	Currency                                           string
	InputPerToken, CachedInputPerToken, OutputPerToken *float64
}

// Config binds a concrete supported primitive. Callbacks are required, trusted,
// concurrency-safe host code and must honor ctx. Authorize runs before Project
// and again after admission, on every attempt. Project returns only permitted
// content. InputTokenBound must supply a trustworthy upper bound for the exact
// final JSON plus server framing/hidden overhead for this deployment; an estimate
// such as ChatModel.CountTokens does not satisfy this contract. It receives a copy.
type Config struct {
	Client          *model.OpenAITextClient
	Deployment      Deployment
	Limits          *Limits // nil selects DefaultLimits; otherwise every limit is explicit
	Authorize       func(context.Context, Task, Deployment) error
	Project         func(context.Context, Task, Deployment) (string, error)
	InputTokenBound func(context.Context, Deployment, []byte) (int, error)
	Prices          *Prices
	StrictBudget    *float64 // any non-nil value is rejected in this initial version
}

// Status is an explicit terminal outcome; only Succeeded permits Text.
type Status string

const (
	Succeeded   Status = "succeeded"
	Rejected    Status = "rejected"
	Failed      Status = "failed"
	Canceled    Status = "canceled"
	TimedOut    Status = "timed_out"
	Unsupported Status = "unsupported"
	Truncated   Status = "truncated"
	Filtered    Status = "filtered"
)

// AttemptKind identifies an actual transport dispatch, including local fixtures.
type AttemptKind string

const PhysicalAttempt AttemptKind = "physical"

// Attempt contains safe accounting metadata, never request/response bodies or
// credentials. Completed=false means cleanup was still running at observation;
// its usage/cost remains unknown. Returned records are frozen independent copies.
type Attempt struct {
	Kind                                          AttemptKind
	Number                                        int
	DeploymentID, Endpoint, RequestedModel, Model string
	Duration                                      time.Duration
	Usage                                         *model.TextUsage
	Completed                                     bool
	Err                                           error
}

// Cost separates the sum of recognized priced components from attempts having
// any unknown component. UnknownAttempts includes failures and unfinished work.
type Cost struct {
	Currency        string
	KnownSubtotal   float64
	UnknownAttempts int
}

// Result is a frozen terminal snapshot. Err preserves context sentinels, but
// omits raw host/provider errors. Cleanup may outlive Execute; late responses
// cannot change this result. Cancellation does not establish remote compute stop.
type Result struct {
	TaskID   string
	Status   Status
	Text     string
	Attempts []Attempt
	Cost     Cost
	Err      error
}

// Structured error sentinels support errors.Is and errors.As(*errors.AgentError).
var (
	ErrConfig       = &ae.AgentError{Category: ae.CategoryConfig, Code: "managedtask.config", Message: "managed task: invalid configuration"}
	ErrTask         = &ae.AgentError{Category: ae.CategoryConfig, Code: "managedtask.task", Message: "managed task: invalid task"}
	ErrDenied       = &ae.AgentError{Category: ae.CategoryPermission, Code: "managedtask.denied", Message: "managed task: authorization denied"}
	ErrUnqualified  = &ae.AgentError{Category: ae.CategoryPermission, Code: "managedtask.unqualified", Message: "managed task: deployment is not qualified for category"}
	ErrCapacity     = &ae.AgentError{Category: ae.CategoryResource, Code: "managedtask.capacity", Message: "managed task: capacity exhausted"}
	ErrQueueTimeout = &ae.AgentError{Category: ae.CategoryResource, Code: "managedtask.queue_timeout", Message: "managed task: queue wait expired"}
	ErrContext      = &ae.AgentError{Category: ae.CategoryContext, Code: "managedtask.context", Message: "managed task: no valid context bound or request exceeds window"}
	ErrProjection   = &ae.AgentError{Category: ae.CategoryPermission, Code: "managedtask.projection", Message: "managed task: data projection failed"}
)
