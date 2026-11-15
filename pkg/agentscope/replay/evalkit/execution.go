package evalkit

import (
	"os"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/protocol"
)

// taskExecution has one owner: execution, then the scoring phase/worker.
// It never escapes into a published report.
type taskExecution struct {
	task        TaskSpec
	result      TaskResult
	outcome     TaskOutcome
	workDir     string
	completedAt time.Time
}

func (e *taskExecution) close() {
	if e != nil && e.workDir != "" {
		_ = os.RemoveAll(e.workDir)
		e.workDir = ""
	}
}

func cloneTask(input *TaskSpec) TaskSpec {
	t := *input
	t.Tags = append([]string(nil), t.Tags...)
	t.Turns = append([]string(nil), t.Turns...)
	t.Tools = append([]string(nil), t.Tools...)
	t.Scorer.Items = append([]string(nil), t.Scorer.Items...)
	if t.Sampling.Temperature != nil {
		v := *t.Sampling.Temperature
		t.Sampling.Temperature = &v
	}
	if t.Sampling.Seed != nil {
		v := *t.Sampling.Seed
		t.Sampling.Seed = &v
	}
	return t
}

// Each turn gets a fresh completion channel before ReplyStream starts. The
// channel signals lifecycle only: OnLoopEnd's error argument is always nil.
type executionCompletion struct{ done chan struct{} }

func (*executionCompletion) BeforeModelCall(protocol.LoopState, int)                       {}
func (*executionCompletion) AfterModelCall(protocol.LoopState, int, error)                 {}
func (*executionCompletion) BeforeToolExec(protocol.LoopState, int, string)                {}
func (*executionCompletion) AfterToolExec(protocol.LoopState, int, string, error)          {}
func (*executionCompletion) OnStateTransition(protocol.LoopState, protocol.LoopState, int) {}
func (*executionCompletion) OnLoopStart()                                                  {}
func (c *executionCompletion) OnLoopEnd(error)                                             { close(c.done) }
