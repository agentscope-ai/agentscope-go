package permission

import (
	"encoding/json"
	"testing"
)

func TestDefaultAskOrigin(t *testing.T) {
	d, err := NewEngine(NewContext(ModeDefault)).CheckPermission(newTool("checked"), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["origin"] != "default_ask" {
		t.Fatalf("default Ask has no typed provenance: %s", data)
	}
}

func TestDecisionOriginBoundaries(t *testing.T) {
	for _, mode := range []PermissionMode{ModeDefault, ModeExplore, ModeAcceptEdits, ModeBypass, ModeDontAsk} {
		for _, kind := range []string{"fallback", "allow rule", "ask rule", "deny rule", "tool allow", "tool deny", "safety ask"} {
			t.Run(string(mode)+"/"+kind, func(t *testing.T) {
				ctx := NewContext(mode)
				e := NewEngine(ctx)
				tool := newTool("checked")
				switch kind {
				case "allow rule":
					e.AddRule(Rule{ToolName: "checked", Behavior: BehaviorAllow})
				case "ask rule":
					e.AddRule(Rule{ToolName: "checked", Behavior: BehaviorAsk})
				case "deny rule":
					e.AddRule(Rule{ToolName: "checked", Behavior: BehaviorDeny})
				case "tool allow", "tool deny", "safety ask":
					tool.checkFn = func(map[string]any, *Context) Decision {
						b := BehaviorAsk
						if kind == "tool allow" {
							b = BehaviorAllow
						}
						if kind == "tool deny" {
							b = BehaviorDeny
						}
						return Decision{Behavior: b, BypassImmune: kind == "safety ask", Origin: OriginDefaultAsk}
					}
				}
				d, err := e.CheckPermission(tool, nil)
				if err != nil {
					t.Fatal(err)
				}
				expected := OriginUnknown
				if mode == ModeDefault && kind == "fallback" {
					expected = OriginDefaultAsk
				}
				if d.Origin != expected {
					t.Fatalf("origin=%q want %q for %+v", d.Origin, expected, d)
				}
				if expected == OriginDefaultAsk && d.Behavior != BehaviorAsk {
					t.Fatal("fallback origin attached to non-Ask")
				}
			})
		}
	}
}

func TestDecisionOriginSerialization(t *testing.T) {
	for _, origin := range []DecisionOrigin{OriginUnknown, OriginDefaultAsk, DecisionOrigin("future")} {
		d := Decision{Behavior: BehaviorAsk, Origin: origin}
		encoded, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var out Decision
		if err := json.Unmarshal(encoded, &out); err != nil {
			t.Fatal(err)
		}
		if out.Origin != origin {
			t.Fatal("origin round trip lost")
		}
		var fields map[string]any
		_ = json.Unmarshal(encoded, &fields)
		if origin == OriginUnknown {
			if _, ok := fields["origin"]; ok {
				t.Fatal("unknown origin should be omitted")
			}
		}
	}
	var legacy Decision
	if err := json.Unmarshal([]byte(`{"behavior":"ask","message":"old"}`), &legacy); err != nil || legacy.Origin != OriginUnknown {
		t.Fatal("legacy JSON not accepted")
	}
}
