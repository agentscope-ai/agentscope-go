package evalkit

import (
	"context"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func mkReport(suite string, results ...TaskResult) *SuiteReport {
	return &SuiteReport{Suite: suite, Results: results}
}

func TestCompare_FlipsAndVerdict(t *testing.T) {
	base := mkReport("base",
		TaskResult{TaskID: "t1", Repeat: 1, Pass: true, Score: 1, InputTokens: 100, OutputTokens: 50},
		TaskResult{TaskID: "t2", Repeat: 1, Pass: false, Score: 0, InputTokens: 100, OutputTokens: 50},
		TaskResult{TaskID: "t3", Repeat: 1, Pass: true, Score: 1, InputTokens: 100, OutputTokens: 50},
	)
	cand := mkReport("cand",
		TaskResult{TaskID: "t1", Repeat: 1, Pass: true, Score: 1, InputTokens: 90, OutputTokens: 40},
		TaskResult{TaskID: "t2", Repeat: 1, Pass: true, Score: 1, InputTokens: 110, OutputTokens: 60},
		TaskResult{TaskID: "t3", Repeat: 1, Pass: false, Score: 0, InputTokens: 100, OutputTokens: 50},
	)
	c := Compare(base, cand)
	if len(c.FlippedUp) != 1 || c.FlippedUp[0] != "t2" {
		t.Errorf("FlippedUp = %v", c.FlippedUp)
	}
	if len(c.FlippedDn) != 1 || c.FlippedDn[0] != "t3" {
		t.Errorf("FlippedDn = %v", c.FlippedDn)
	}
	if !strings.Contains(c.Verdict(), "1 regression(s), 1 gain(s)") {
		t.Errorf("verdict = %q", c.Verdict())
	}
	md := c.Markdown()
	if !strings.Contains(md, "t2") || !strings.Contains(md, "⬆️") {
		t.Errorf("markdown malformed:\n%s", md)
	}
}

func TestCompare_StrictlyBetterVerdict(t *testing.T) {
	base := mkReport("b", TaskResult{TaskID: "t1", Repeat: 1, Pass: false})
	cand := mkReport("c", TaskResult{TaskID: "t1", Repeat: 1, Pass: true})
	if v := Compare(base, cand).Verdict(); !strings.Contains(v, "strictly better") {
		t.Errorf("verdict = %q", v)
	}
}

type judgeModel struct{ reply string }

func (j *judgeModel) Chat(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (*model.ChatResponse, error) {
	return &model.ChatResponse{Content: []message.ContentBlock{
		message.TextBlock{Type: "text", Text: j.reply},
	}, IsLast: true}, nil
}
func (j *judgeModel) ChatStream(_ context.Context, _ []*message.Msg, _ ...model.CallOption) (<-chan model.ChatResponse, error) {
	return nil, model.ErrStreamNotSupported
}
func (j *judgeModel) CountTokens(_ []*message.Msg, _ []model.ToolSchema) int { return 1 }

func TestLLMJudge_ScoresAndCaches(t *testing.T) {
	jm := &judgeModel{reply: "8"}
	j := NewLLMJudge(jm, "")
	spec := TaskSpec{ID: "jt", Input: "what is 1+1?"}
	out := TaskOutcome{FinalText: "2"}

	s1, err := j.Score(context.Background(), &spec, &out)
	if err != nil {
		t.Fatal(err)
	}
	if s1 != 0.8 {
		t.Errorf("score = %v, want 0.8", s1)
	}
	// Second identical call must hit the cache (judge returns garbage now;
	// cached value wins).
	jm.reply = "not a number"
	s2, err := j.Score(context.Background(), &spec, &out)
	if err != nil {
		t.Fatalf("cache miss caused re-judge: %v", err)
	}
	if s2 != 0.8 {
		t.Errorf("cached score = %v", s2)
	}
	// Unparseable judge output surfaces an error.
	j2 := NewLLMJudge(&judgeModel{reply: "meh"}, "")
	if _, err := j2.Score(context.Background(), &spec, &TaskOutcome{FinalText: "x"}); err == nil {
		t.Error("unparseable judge reply must error")
	}
}

func TestRunTask_MultiTurn(t *testing.T) {
	m := &scriptedModel{responses: []model.ChatResponse{
		{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "first answer"}}, IsLast: true},
		{Content: []message.ContentBlock{message.TextBlock{Type: "text", Text: "second answer FINAL"}}, IsLast: true},
	}}
	task := TaskSpec{
		ID:     "mt",
		Input:  "turn one",
		Turns:  []string{"turn two"},
		Scorer: ScorerSpec{Ref: "contains", Expect: "FINAL"},
	}
	r := &Runner{}
	res := r.RunTask(context.Background(), &task, m)
	if res.Error != "" {
		t.Fatalf("error: %s", res.Error)
	}
	if !res.Pass {
		t.Errorf("multi-turn task should score the LAST reply: %+v", res)
	}
	if res.Iters != 2 {
		t.Errorf("iters = %d, want 2 (one per turn)", res.Iters)
	}
}
