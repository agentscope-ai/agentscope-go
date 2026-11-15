package permission

// WorkingDirectory is an additional directory included in permission scope.
// Working directories determine which file paths are auto-allowed in
// ModeAcceptEdits.
type WorkingDirectory struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

// Context holds the permission mode, working directories, and all configured
// permission rules organized by behavior type.
type Context struct {
	Mode               PermissionMode              `json:"mode"`
	WorkingDirectories map[string]WorkingDirectory `json:"working_directories,omitempty"`
	AllowRules         map[string][]Rule           `json:"allow_rules,omitempty"`
	DenyRules          map[string][]Rule           `json:"deny_rules,omitempty"`
	AskRules           map[string][]Rule           `json:"ask_rules,omitempty"`

	// TargetShell optionally pins the shell that tool permission checks
	// assume: "posix" or "powershell". Empty means host detection
	// (platform.Detect). The tool orchestrator derives a per-call override
	// of "posix" when a workspace backend will execute the command —
	// container backends are POSIX regardless of the host OS.
	TargetShell string `json:"target_shell,omitempty"`
}

// NewContext creates a Context with the given mode and empty rule maps.
func NewContext(mode PermissionMode) *Context {
	return &Context{
		Mode:               mode,
		WorkingDirectories: make(map[string]WorkingDirectory),
		AllowRules:         make(map[string][]Rule),
		DenyRules:          make(map[string][]Rule),
		AskRules:           make(map[string][]Rule),
	}
}
