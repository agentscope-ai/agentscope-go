package openaiserver_test

import (
	"strings"
	"testing"
)

func TestParameterAndHistoryBoundaries(t *testing.T) {
	h := handler(t, fixtureModel{}, nil)
	for _, extra := range []string{
		`"temperature":-0.1`, `"temperature":2.1`, `"top_p":-0.1`, `"top_p":1.1`, `"max_tokens":0`, `"max_tokens":-1`,
		`"stream_options":{"include_usage":true}`, `"tool_choice":{"type":"function","function":{"name":"lookup"}}`,
		`"stop":5`, `"stop":[]`, `"stop":["a","b","c","d","e"]`,
		`"tools":[{"type":"custom","function":{"name":"x"}}]`,
		`"tools":[{"type":"function","function":{"name":"x"}},{"type":"function","function":{"name":"x"}}]`,
		`"tools":[{"type":"function","function":{"name":"x","parameters":[]}}]`,
	} {
		t.Run(extra, func(t *testing.T) {
			w := request(h, strings.TrimSuffix(basic, "}")+","+extra+"}")
			if w.Code != 400 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
	for _, extra := range []string{`"stop":"end"`, `"stop":["end"]`, `"stop":null`, `"tool_choice":"auto"`, `"temperature":0,"top_p":1,"max_tokens":1,"n":1`, `"tools":[]`} {
		w := request(h, strings.TrimSuffix(basic, "}")+","+extra+"}")
		if w.Code != 200 {
			t.Fatalf("accepted parameters: %d %s", w.Code, w.Body)
		}
	}
	for _, messages := range []string{
		`[{"role":"user","content":42}]`,
		`[{"role":"user","content":[{"type":"text"}]}]`,
		`[{"role":"function","content":"legacy"},{"role":"user","content":"hi"}]`,
		`[{"role":"user","content":"hi","tool_calls":[{}]}]`,
		`[{"role":"user","content":"hi","tool_call_id":"x"}]`,
		`[{"role":"user","content":"hi"},{"role":"system","content":"late"},{"role":"user","content":"next"}]`,
		`[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"x","arguments":"null"}}]},{"role":"user","content":"hi"}]`,
		`[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"x","arguments":"{}"}},{"id":"a","type":"function","function":{"name":"x","arguments":"{}"}}]},{"role":"user","content":"hi"}]`,
	} {
		t.Run(messages, func(t *testing.T) {
			w := request(h, `{"model":"support","messages":`+messages+`}`)
			if w.Code != 400 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
	w := request(h, `{"model":"support","messages":[{"role":"developer","content":[{"type":"text","text":"help"}]},{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"x","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"x","arguments":"{}"}}]},{"role":"tool","tool_call_id":"b","content":"second"},{"role":"tool","tool_call_id":"a","content":"first"},{"role":"user","content":"next"}]}`)
	if w.Code != 200 {
		t.Fatalf("parallel tool history: %d %s", w.Code, w.Body)
	}
}
