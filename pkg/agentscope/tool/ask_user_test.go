package tool

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

func askUserTestParams() AskUserParams {
	return AskUserParams{Questions: []AskUserQuestion{{
		Question: "Which language?", Header: "Language", Context: "Pick a client language.",
		Options: []AskUserOption{{Label: "Go", Description: "Use the Go client", Preview: "package main"}, {Label: "Python", Description: "Use the Python client"}},
	}}}
}

func askUserTestObject(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func TestAskUserInputConstraints(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*AskUserParams)
		valid  bool
	}{
		{"valid", func(*AskUserParams) {}, true},
		{"unicode_header", func(p *AskUserParams) { p.Questions[0].Header = strings.Repeat("语", 12) }, true},
		{"long_header", func(p *AskUserParams) { p.Questions[0].Header = strings.Repeat("语", 13) }, false},
		{"no_questions", func(p *AskUserParams) { p.Questions = nil }, false},
		{"too_many_questions", func(p *AskUserParams) { p.Questions = make([]AskUserQuestion, 5) }, false},
		{"blank_question", func(p *AskUserParams) { p.Questions[0].Question = " " }, false},
		{"blank_header", func(p *AskUserParams) { p.Questions[0].Header = "" }, false},
		{"duplicate_question", func(p *AskUserParams) { p.Questions = append(p.Questions, p.Questions[0]) }, false},
		{"too_few_options", func(p *AskUserParams) { p.Questions[0].Options = p.Questions[0].Options[:1] }, false},
		{"too_many_options", func(p *AskUserParams) { p.Questions[0].Options = make([]AskUserOption, 5) }, false},
		{"blank_label", func(p *AskUserParams) { p.Questions[0].Options[0].Label = " " }, false},
		{"blank_description", func(p *AskUserParams) { p.Questions[0].Options[0].Description = "" }, false},
		{"duplicate_label", func(p *AskUserParams) { p.Questions[0].Options[1].Label = "Go" }, false},
		{"multiselect_preview", func(p *AskUserParams) { p.Questions[0].MultiSelect = true }, false},
		{"multiselect", func(p *AskUserParams) { p.Questions[0].MultiSelect = true; p.Questions[0].Options[0].Preview = "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := askUserTestParams()
			tc.change(&p)
			input := askUserTestObject(t, p)
			before := askUserTestObject(t, input)
			tool := AskUserTool()
			err := tool.(InputValidator).ValidateInput(input)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
			if !reflect.DeepEqual(input, before) {
				t.Fatal("input was mutated")
			}
			if tc.valid {
				// Exercise schema validation too, including Unicode lengths.
				if err := ValidateInput(tool.InputSchema(), input); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	for _, input := range []map[string]any{nil, {}, {"questions": "wrong type"}, {"questions": make(chan int)}} {
		if err := AskUserTool().(InputValidator).ValidateInput(input); err == nil {
			t.Fatalf("accepted invalid input %#v", input)
		}
	}
}

func TestAskUserAnswers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selected []string
		other    string
		multi    bool
		valid    bool
	}{
		{"single", []string{"Go"}, "", false, true},
		{"free_text", nil, "Rust", false, true},
		{"multiple", []string{"Go", "Python"}, "", true, true},
		{"too_many_selections", []string{"Go", "Python"}, "", false, false},
		{"unknown_label", []string{"Ruby"}, "", false, false},
		{"duplicate_label", []string{"Go", "Go"}, "", true, false},
		{"no_answer", nil, " ", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := askUserTestParams()
			p.Questions[0].MultiSelect = tc.multi
			p.Questions[0].Options[0].Preview = ""
			meta := AskUserMetadata{Answers: []AskUserAnswer{{Question: p.Questions[0].Question, Selected: tc.selected, Other: tc.other}}}
			result := &message.ToolResultBlock{State: message.ToolResultSuccess, Metadata: askUserTestObject(t, meta)}
			before := askUserTestObject(t, result.Metadata)
			err := AskUserTool().(ExternalResultValidator).ValidateExternalResult(askUserTestObject(t, p), result)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
			if !reflect.DeepEqual(before, result.Metadata) {
				t.Fatal("metadata was mutated")
			}
		})
	}
}

func TestAskUserAnswerCorrelationAndErrorResults(t *testing.T) {
	p := askUserTestParams()
	p.Questions = append(p.Questions, AskUserQuestion{Question: "Which runtime?", Header: "Runtime", Options: []AskUserOption{{Label: "Local", Description: "Run locally"}, {Label: "Cloud", Description: "Run remotely"}}})
	input := askUserTestObject(t, p)
	validator := AskUserTool().(ExternalResultValidator)
	valid := []AskUserAnswer{{Question: "Which runtime?", Selected: []string{"Local"}}, {Question: "Which language?", Selected: []string{"Go"}}}
	if err := validator.ValidateExternalResult(input, &message.ToolResultBlock{State: message.ToolResultSuccess, Metadata: askUserTestObject(t, AskUserMetadata{Answers: valid})}); err != nil {
		t.Fatalf("answers may arrive in a different order: %v", err)
	}
	for _, answers := range [][]AskUserAnswer{nil, valid[:1], {valid[0], valid[0]}, {{Question: "Unknown", Other: "answer"}, valid[1]}} {
		if err := validator.ValidateExternalResult(input, &message.ToolResultBlock{State: message.ToolResultSuccess, Metadata: askUserTestObject(t, AskUserMetadata{Answers: answers})}); err == nil {
			t.Fatalf("accepted unmatched answers: %+v", answers)
		}
	}
	for _, metadata := range []map[string]any{nil, {"answers": "invalid"}, {"answers": make(chan int)}} {
		if err := validator.ValidateExternalResult(input, &message.ToolResultBlock{State: message.ToolResultSuccess, Metadata: metadata}); err == nil {
			t.Fatalf("accepted invalid metadata: %#v", metadata)
		}
	}
	if err := validator.ValidateExternalResult(input, nil); err == nil {
		t.Fatal("accepted nil result")
	}
	if err := validator.ValidateExternalResult(nil, &message.ToolResultBlock{State: message.ToolResultSuccess}); err == nil {
		t.Fatal("accepted invalid original input")
	}
	for _, state := range []message.ToolResultState{message.ToolResultError, message.ToolResultDenied, message.ToolResultInterrupted} {
		if err := validator.ValidateExternalResult(input, &message.ToolResultBlock{State: state}); err != nil {
			t.Fatalf("state %s should not require success metadata: %v", state, err)
		}
	}
}

func TestAskUserExternalAndPermissionContract(t *testing.T) {
	questionTool := AskUserTool()
	if questionTool.Name() != "AskUser" || !questionTool.IsExternalTool() || !questionTool.IsReadOnly() || !questionTool.IsConcurrencySafe() {
		t.Fatal("incorrect external-tool capabilities")
	}
	input := askUserTestObject(t, askUserTestParams())
	for _, invalid := range []bool{false, true} {
		args := input
		if invalid {
			args = nil
		}
		resp, err := questionTool.Execute(context.Background(), args)
		if err != nil || resp == nil || resp.State != message.ToolResultError {
			t.Fatalf("direct execution must not invent an answer: %+v, %v", resp, err)
		}
	}
	for _, tc := range []struct {
		mode permission.PermissionMode
		rule permission.PermissionBehavior
		want permission.PermissionBehavior
	}{
		{permission.ModeDefault, "", permission.BehaviorAllow},
		{permission.ModeExplore, "", permission.BehaviorAllow},
		{permission.ModeDontAsk, "", permission.BehaviorDeny},
		{permission.ModeBypass, permission.BehaviorDeny, permission.BehaviorDeny},
		{permission.ModeDefault, permission.BehaviorAsk, permission.BehaviorAsk},
	} {
		engine := permission.NewEngine(permission.NewContext(tc.mode))
		if tc.rule != "" {
			engine.AddRule(permission.Rule{ToolName: "AskUser", Behavior: tc.rule})
		}
		decision, err := engine.CheckPermission(questionTool, input)
		if err != nil || decision.Behavior != tc.want {
			t.Fatalf("mode=%s rule=%s: decision=%+v, error=%v", tc.mode, tc.rule, decision, err)
		}
	}
	if NewEnhancedToolkit().Get("AskUser") != nil {
		t.Fatal("AskUser must be registered explicitly by a host")
	}
}
