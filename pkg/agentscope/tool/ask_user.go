package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

// AskUserOption is one choice presented by an AskUser host.
type AskUserOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Preview     string `json:"preview,omitempty"`
}

// AskUserQuestion describes one question. Question text identifies its answer;
// it must be unique within the batch. Headers contain at most 12 Unicode code
// points, and each question has two to four distinct option labels.
type AskUserQuestion struct {
	Question    string          `json:"question"`
	Header      string          `json:"header"`
	Context     string          `json:"context,omitempty"`
	Options     []AskUserOption `json:"options"`
	MultiSelect bool            `json:"multi_select,omitempty"`
}

// AskUserParams is the input to AskUser, containing one to four questions.
type AskUserParams struct {
	Questions []AskUserQuestion `json:"questions"`
}

// AskUserAnswer uses the exact question text and selected option labels from
// the request. Other carries free text supplied by the user. An answer needs
// at least one selection or nonblank free text.
type AskUserAnswer struct {
	Question string   `json:"question"`
	Selected []string `json:"selected,omitempty"`
	Other    string   `json:"other,omitempty"`
}

// AskUserMetadata is the structured half of a successful external result.
// The host places its JSON object in ToolResultBlock.Metadata and supplies
// human-readable output separately. Every requested question needs one answer.
type AskUserMetadata struct {
	Answers []AskUserAnswer `json:"answers"`
}

var askUserSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "questions": {
      "type": "array", "minItems": 1, "maxItems": 4,
      "items": {
        "type": "object",
        "properties": {
          "question": {"type": "string", "minLength": 1},
          "header": {"type": "string", "minLength": 1, "maxLength": 12},
          "context": {"type": "string"},
          "multi_select": {"type": "boolean", "default": false},
          "options": {
            "type": "array", "minItems": 2, "maxItems": 4,
            "items": {
              "type": "object",
              "properties": {
                "label": {"type": "string", "minLength": 1},
                "description": {"type": "string", "minLength": 1},
                "preview": {"type": "string"}
              },
              "required": ["label", "description"]
            }
          }
        },
        "required": ["question", "header", "options"]
      }
    }
  },
  "required": ["questions"]
}`)

type askUserTool struct{ BaseTool }

// AskUserTool collects structured choices through an external host. Register it
// only when the host handles RequireExternalExecutionEvent and submits answers
// through UnifiedAgent.SubmitExternalResult. It has no built-in terminal UI.
// Explicit permission rules still apply; ModeDontAsk denies this tool.
func AskUserTool() Tool {
	return &askUserTool{BaseTool: BaseTool{
		ToolName: "AskUser",
		ToolDescription: "Ask the user one to four questions, each with two to four distinct options. " +
			"Question texts must be unique. Use headers of at most 12 characters. " +
			"Set multi_select only when several choices may apply. Previews are for single-select questions. " +
			"The host must also offer free-text input; do not add an Other option. " +
			"Place recommended choices first. Put supporting material in context. " +
			"Answers appear as readable output and structured answers metadata. Asking a question does not authorize other tools.",
		ToolSchema: askUserSchema, ReadOnly: true, ConcurrencySafe: true,
	}}
}

func (*askUserTool) IsExternalTool() bool { return true }

func (*askUserTool) CheckPermissions(_ map[string]any, ctx *permission.Context) permission.Decision {
	if ctx != nil && ctx.Mode == permission.ModeDontAsk {
		return permission.Decision{Behavior: permission.BehaviorDeny, Message: "AskUser requires an interactive user"}
	}
	return permission.Decision{Behavior: permission.BehaviorAllow, Message: "The host collects the user's answer"}
}

func (t *askUserTool) Execute(_ context.Context, input map[string]any) (*ToolResponse, error) {
	if err := t.ValidateInput(input); err != nil {
		return NewErrorResponse(err), nil
	}
	return NewErrorResponse(fmt.Errorf("AskUser requires an external host; use ReplyStream and SubmitExternalResult")), nil
}

func (*askUserTool) ValidateInput(input map[string]any) error {
	_, err := decodeAskUserParams(input)
	return err
}

func decodeAskUserParams(input map[string]any) (*AskUserParams, error) {
	var params AskUserParams
	if err := decodeAskUserObject(input, &params); err != nil {
		return nil, fmt.Errorf("AskUser input: %w", err)
	}
	if len(params.Questions) < 1 || len(params.Questions) > 4 {
		return nil, fmt.Errorf("questions must contain one to four questions")
	}
	seen := make(map[string]bool, len(params.Questions))
	for i, q := range params.Questions {
		if strings.TrimSpace(q.Question) == "" || strings.TrimSpace(q.Header) == "" {
			return nil, fmt.Errorf("questions[%d]: question and header must be nonblank", i)
		}
		if utf8.RuneCountInString(q.Header) > 12 {
			return nil, fmt.Errorf("questions[%d].header exceeds 12 characters", i)
		}
		if seen[q.Question] {
			return nil, fmt.Errorf("questions[%d]: question text must be unique", i)
		}
		seen[q.Question] = true
		if len(q.Options) < 2 || len(q.Options) > 4 {
			return nil, fmt.Errorf("questions[%d].options must contain two to four options", i)
		}
		labels := make(map[string]bool, len(q.Options))
		for j, option := range q.Options {
			if strings.TrimSpace(option.Label) == "" || strings.TrimSpace(option.Description) == "" {
				return nil, fmt.Errorf("questions[%d].options[%d]: label and description must be nonblank", i, j)
			}
			if labels[option.Label] {
				return nil, fmt.Errorf("questions[%d].options[%d]: option labels must be unique", i, j)
			}
			labels[option.Label] = true
			if q.MultiSelect && option.Preview != "" {
				return nil, fmt.Errorf("questions[%d].options[%d]: preview requires single-select", i, j)
			}
		}
	}
	return &params, nil
}

func (*askUserTool) ValidateExternalResult(input map[string]any, result *message.ToolResultBlock) error {
	if result == nil {
		return fmt.Errorf("AskUser result is nil")
	}
	if result.State != message.ToolResultSuccess {
		return nil
	}
	params, err := decodeAskUserParams(input)
	if err != nil {
		return err
	}
	var metadata AskUserMetadata
	if err := decodeAskUserObject(result.Metadata, &metadata); err != nil {
		return fmt.Errorf("AskUser metadata: %w", err)
	}
	if len(metadata.Answers) != len(params.Questions) {
		return fmt.Errorf("answers must contain exactly one answer per question")
	}
	questions := make(map[string]AskUserQuestion, len(params.Questions))
	for _, q := range params.Questions {
		questions[q.Question] = q
	}
	for i, answer := range metadata.Answers {
		q, ok := questions[answer.Question]
		if !ok {
			return fmt.Errorf("answers[%d].question is unknown or repeated", i)
		}
		delete(questions, answer.Question)
		if !q.MultiSelect && len(answer.Selected) > 1 {
			return fmt.Errorf("answers[%d]: single-select question accepts at most one selection", i)
		}
		if len(answer.Selected) == 0 && strings.TrimSpace(answer.Other) == "" {
			return fmt.Errorf("answers[%d]: provide a selection or free text", i)
		}
		labels := make(map[string]bool, len(q.Options))
		for _, option := range q.Options {
			labels[option.Label] = true
		}
		for _, selected := range answer.Selected {
			if !labels[selected] {
				return fmt.Errorf("answers[%d]: selected label is unknown or repeated", i)
			}
			delete(labels, selected)
		}
	}
	return nil
}

// Round-trip typed application values and JSON-decoded maps through the same
// representation without rewriting caller-owned input or metadata.
func decodeAskUserObject(value map[string]any, target any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
