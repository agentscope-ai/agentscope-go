package permission

import (
	"testing"
)

func TestGrantDirRule_RoundTrip(t *testing.T) {
	r := NewGrantDirRule("/outside/dir")
	if r.ToolName != GrantRuleToolName || r.Behavior != BehaviorAllow {
		t.Errorf("unexpected grant rule shape: %+v", r)
	}
	dir, ok := ParseGrantDirRule(r)
	if !ok || dir != "/outside/dir" {
		t.Errorf("round trip failed: %q ok=%v", dir, ok)
	}
}

func TestParseGrantDirRule_Rejects(t *testing.T) {
	cases := []Rule{
		{ToolName: "Bash", RuleContent: "grant:dir:/x", Behavior: BehaviorAllow},
		{ToolName: GrantRuleToolName, RuleContent: "grant:file:/x", Behavior: BehaviorAllow},
		{ToolName: GrantRuleToolName, RuleContent: "grant:dir:", Behavior: BehaviorAllow},
		{ToolName: GrantRuleToolName, RuleContent: "", Behavior: BehaviorAllow},
	}
	for _, c := range cases {
		if dir, ok := ParseGrantDirRule(c); ok {
			t.Errorf("rule %+v should not parse as a grant, got %q", c, dir)
		}
	}
}

func TestEngine_WorkingDirectories(t *testing.T) {
	e := NewEngine(NewContext(ModeBypass))

	e.AddWorkingDirectory(WorkingDirectory{Path: "/a", Source: "granted"})
	e.AddWorkingDirectory(WorkingDirectory{Path: "/b", Source: "session"})
	// Idempotent re-add by path.
	e.AddWorkingDirectory(WorkingDirectory{Path: "/a2", Source: "granted"})
	e.AddWorkingDirectory(WorkingDirectory{Path: "/a2", Source: "granted"})

	dirs := e.GrantedDirectories()
	if len(dirs) != 3 {
		t.Errorf("expected 3 granted dirs, got %v", dirs)
	}

	// Merge: restored entries fill gaps, host-declared entries win.
	e.Context.WorkingDirectories["/b"] = WorkingDirectory{Path: "/b", Source: "session"}
	e.MergeWorkingDirectories(map[string]WorkingDirectory{
		"/b": {Path: "/b", Source: "granted"},
		"/c": {Path: "/c", Source: "granted"},
	})
	if got := e.Context.WorkingDirectories["/b"].Source; got != "session" {
		t.Errorf("host-declared dir was overwritten: %s", got)
	}
	if _, ok := e.Context.WorkingDirectories["/c"]; !ok {
		t.Error("restored dir /c was not merged")
	}
}

func TestEngine_SnapshotContext_DeepCopy(t *testing.T) {
	e := NewEngine(NewContext(ModeDefault))
	e.AddRule(Rule{ToolName: "Bash", RuleContent: "ls:*", Behavior: BehaviorAllow, Source: "user"})
	e.AddWorkingDirectory(WorkingDirectory{Path: "/a", Source: "granted"})

	snap := e.SnapshotContext()
	snap.WorkingDirectories["/a"] = WorkingDirectory{Path: "/a", Source: "mutated"}
	snap.AllowRules["Bash"][0].RuleContent = "mutated"

	if got := e.Context.WorkingDirectories["/a"].Source; got != "granted" {
		t.Errorf("snapshot shares working-directory state: %s", got)
	}
	if got := e.Context.AllowRules["Bash"][0].RuleContent; got != "ls:*" {
		t.Errorf("snapshot shares rule state: %s", got)
	}
}
