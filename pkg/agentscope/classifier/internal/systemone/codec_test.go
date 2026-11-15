package systemone_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	c "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/internal/systemone"
)

func invalid(s string) error { return fmt.Errorf("fixture: %s", s) }

func request() c.Request {
	return c.Request{OperationID: "op", State: json.RawMessage(`"state"`), Questions: map[string]c.Question{
		"b": &c.BinaryQuestion{Instructions: "true?"},
		"c": &c.ChoiceQuestion{Instructions: "team?", Choices: map[string]string{"yes": "Y", "no": "N"}},
		"s": &c.ScoreQuestion{Instructions: "rate?", Levels: []string{"low", "high"}},
	}}
}

const fixture = `{"model":"fixture","answers":{"b":{"type":"noul","noul":0},"c":{"type":"choice","choice":"yes","confidence":0.4,"probabilities":{"yes":0.75,"no":0.25}},"s":{"type":"score","score":0.75,"confidence":0.5,"legend":{"0":"low","1":"high"},"probabilities":{"0":0.25,"1":0.75}}},"usage":{"input_tokens":12,"output_tokens":0}}`

func TestRoundTrip(t *testing.T) {
	r := request()
	for _, criteria := range []bool{false, true} {
		if criteria {
			r.Questions["b"] = &c.BinaryQuestion{Instructions: "true?", True: "positive", False: "negative"}
		}
		body, err := systemone.Encode("requested", r, invalid)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Model     string `json:"model"`
			State     string `json:"state"`
			Questions map[string]struct {
				Type     string          `json:"type"`
				Criteria json.RawMessage `json:"criteria"`
			} `json:"questions"`
		}
		if err = json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got.Model != "requested" || got.State != "state" || got.Questions["b"].Type != "noul" || got.Questions["c"].Type != "choice" || got.Questions["s"].Type != "score" || (len(got.Questions["b"].Criteria) > 0) != criteria {
			t.Fatalf("wrong mapping: %s", body)
		}
	}
	out := &c.Response{OperationID: r.OperationID, Attempts: []c.Attempt{{Kind: c.PhysicalAttempt, RequestedModel: "requested"}}}
	if err := systemone.Decode([]byte(fixture), r, out, invalid); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateResponse(r, out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "fixture" || out.Attempts[0].Model != "fixture" || *out.Attempts[0].Usage.InputTokens != 12 || *out.Attempts[0].Usage.OutputTokens != 0 {
		t.Fatal("lost model/usage")
	}
}

func TestDecodeFailuresRetainUsage(t *testing.T) {
	for name, body := range map[string]string{
		"answer count":        strings.Replace(fixture, `"b":`, `"unexpected":`, 1),
		"missing answers":     `{"model":"fixture","usage":{"input_tokens":12}}`,
		"bad answer":          strings.Replace(fixture, `"noul":0`, `"noul":"bad"`, 1),
		"missing numeric":     strings.Replace(fixture, `,"noul":0`, "", 1),
		"null probability":    strings.Replace(fixture, `"no":0.25`, `"no":null`, 1),
		"wrong score legend":  strings.Replace(fixture, `"0":"low"`, `"0":"wrong"`, 1),
		"missing score level": strings.Replace(fixture, `"0":0.25,"1":0.75`, `"1":1`, 1),
		"wrong model":         strings.Replace(fixture, `"model":"fixture"`, `"model":123`, 1),
		"invalid usage":       strings.Replace(fixture, `"output_tokens":0`, `"output_tokens":-1`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			out := &c.Response{Attempts: []c.Attempt{{}}}
			if err := systemone.Decode([]byte(body), request(), out, invalid); err == nil {
				t.Fatal("accepted invalid reply")
			}
			if out.Attempts[0].Usage == nil || out.Attempts[0].Usage.InputTokens == nil || *out.Attempts[0].Usage.InputTokens != 12 {
				t.Fatal("valid usage lost")
			}
		})
	}
}

func TestMalformedEnvelopeAndUnknownUsage(t *testing.T) {
	for _, body := range []string{`[]`, `null`, `{}`, `{"usage":[]}`, fixture + `{}`, "\xff"} {
		out := &c.Response{Attempts: []c.Attempt{{}}}
		if err := systemone.Decode([]byte(body), request(), out, invalid); err == nil {
			t.Fatal("accepted malformed response")
		}
	}
	for _, usage := range []string{"", `,"usage":null`, `,"usage":{"input_tokens":null}`} {
		body := strings.Replace(fixture, `,"usage":{"input_tokens":12,"output_tokens":0}`, usage, 1)
		out := &c.Response{Attempts: []c.Attempt{{}}}
		if err := systemone.Decode([]byte(body), request(), out, invalid); err != nil {
			t.Fatal(err)
		}
		u := out.Attempts[0].Usage
		if u != nil && (u.InputTokens != nil || u.OutputTokens != nil) {
			t.Fatal("invented unknown usage")
		}
	}
}

func TestEncodingFailureUsesCallerError(t *testing.T) {
	r := request()
	r.State = json.RawMessage(`{`)
	want := errors.New("encoding rejected")
	_, err := systemone.Encode("m", r, func(string) error { return want })
	if err != want {
		t.Fatal("error identity lost")
	}
}
