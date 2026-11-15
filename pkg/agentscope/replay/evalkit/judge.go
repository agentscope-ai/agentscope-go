package evalkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

// LLMJudgeScorer grades free-form outputs with a separate judge model
// (HARNESS_DESIGN C3/H3). Results are cached by (task, output) hash so
// re-runs do not re-bill identical judgments.
type LLMJudgeScorer struct {
	Judge  model.ChatModel
	Rubric string // grading instructions; defaults to a generic rubric

	mu    sync.Mutex
	cache map[string]float64
}

const defaultJudgeRubric = `Grade the assistant's answer for the given task.
Respond with ONLY an integer score from 0 to 10 where:
0 = wrong or no answer, 5 = partially correct, 10 = fully correct.
First line of your response must be the integer alone.`

// NewLLMJudge builds the judge scorer.
func NewLLMJudge(judge model.ChatModel, rubric string) *LLMJudgeScorer {
	if rubric == "" {
		rubric = defaultJudgeRubric
	}
	return &LLMJudgeScorer{Judge: judge, Rubric: rubric, cache: map[string]float64{}}
}

// Score implements Scorer.
func (j *LLMJudgeScorer) Score(ctx context.Context, spec *TaskSpec, out *TaskOutcome) (float64, error) {
	if j.Judge == nil {
		return 0, fmt.Errorf("llm judge: no judge model configured")
	}
	// Include the rubric in the cache key: reconfiguring the rubric on a
	// shared judge instance must not serve stale scores (HARNESS review L5).
	h := sha256.Sum256([]byte(spec.ID + "\x00" + j.Rubric + "\x00" + out.FinalText))
	key := hex.EncodeToString(h[:16])

	j.mu.Lock()
	if v, ok := j.cache[key]; ok {
		j.mu.Unlock()
		return v, nil
	}
	j.mu.Unlock()

	prompt := fmt.Sprintf("%s\n\nTask:\n%s\n\nAssistant answer:\n%s\n\nScore:",
		j.Rubric, spec.Input, out.FinalText)
	resp, err := j.Judge.Chat(ctx, []*message.Msg{message.UserMsg("judge", prompt)})
	if err != nil {
		return 0, fmt.Errorf("llm judge: %w", err)
	}
	text := ""
	for _, b := range resp.Content {
		if tb, ok := b.(message.TextBlock); ok {
			text += tb.Text
		}
	}
	score, err := parseJudgeScore(text)
	if err != nil {
		return 0, err
	}
	j.mu.Lock()
	j.cache[key] = score
	j.mu.Unlock()
	return score, nil
}

func parseJudgeScore(text string) (float64, error) {
	line := strings.TrimSpace(strings.Split(strings.TrimSpace(text), "\n")[0])
	v, err := strconv.Atoi(line)
	if err != nil || v < 0 || v > 10 {
		return 0, fmt.Errorf("llm judge: unparseable score %q", line)
	}
	return float64(v) / 10.0, nil
}
