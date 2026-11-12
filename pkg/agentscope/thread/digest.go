package thread

import (
	"strings"
)

// toolTargetKeys are the arguments that name what a tool acted on, in the
// order they are tried.
var toolTargetKeys = []string{
	"path", "file_path", "file", "filename", "url", "uri", "dir", "directory", "pattern", "query",
}

// toolCommandKeys are the arguments whose first word names the target when
// no toolTargetKeys argument is present.
var toolCommandKeys = []string{"command", "cmd"}

// toolTargetRunes bounds a tool target.
const toolTargetRunes = 80

// DigestArgsRunes bounds the arguments of one call in a ToolDigest. Longer
// arguments are cut and end in "...": a write_file call carries a whole
// file, which belongs in the workspace and would fill an edge window as a
// log entry.
const DigestArgsRunes = 200

// DigestBuilder collects the tool calls of one reply into a ToolDigest: one
// item per call, in call order, with the tool name and the arguments as the
// model wrote them, cut at DigestArgsRunes (stage 0's tool summary). The zero
// value is ready to use. It is not safe for concurrent use.
type DigestBuilder struct {
	calls []ToolCallDigest
}

// Add records one call; args is its raw JSON arguments. A call with an empty
// name is ignored.
func (b *DigestBuilder) Add(name, args string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	b.calls = append(b.calls, ToolCallDigest{Name: name, Args: clipArgs(strings.TrimSpace(args))})
}

// Digest returns the calls added so far, nil when there are none. The
// result is a snapshot: later calls do not change it.
func (b *DigestBuilder) Digest() *ToolDigest {
	if len(b.calls) == 0 {
		return nil
	}
	return &ToolDigest{Calls: append([]ToolCallDigest(nil), b.calls...)}
}

func clipArgs(args string) string {
	if r := []rune(args); len(r) > DigestArgsRunes {
		return string(r[:DigestArgsRunes]) + "..."
	}
	return args
}

// ToolTarget names what a call acted on: the first present, non-empty string
// argument among path, file_path, file, filename, url, uri, dir, directory,
// pattern and query; else the first word of command or cmd; else "". It is at
// most 80 runes and on one line. The event list records it with each output.
func ToolTarget(args map[string]any) string {
	for _, k := range toolTargetKeys {
		if s, ok := args[k].(string); ok {
			if t := oneLine(s); t != "" {
				return truncateRunes(t, toolTargetRunes)
			}
		}
	}
	for _, k := range toolCommandKeys {
		if s, ok := args[k].(string); ok {
			if f := strings.Fields(s); len(f) > 0 {
				return truncateRunes(f[0], toolTargetRunes)
			}
		}
	}
	return ""
}

// oneLine trims s and folds its whitespace runs into single spaces, so a
// target never breaks the line it is rendered on.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
