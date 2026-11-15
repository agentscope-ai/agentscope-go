package platform

// ShellType identifies the shell interpreter.
type ShellType int

const (
	ShellBash ShellType = iota
	ShellZsh
	ShellSh
	ShellPowerShell
	ShellCmd
)

var shellTypeNames = [...]string{
	"bash",
	"zsh",
	"sh",
	"powershell",
	"cmd",
}

func (s ShellType) String() string {
	if int(s) < len(shellTypeNames) {
		return shellTypeNames[s]
	}
	return "unknown"
}

// Shell holds the detected shell type and its executable path.
type Shell struct {
	Type ShellType
	Path string
}

// DeriveExecArgs returns the command-line arguments to execute a command
// string in this shell.
func (s Shell) DeriveExecArgs(command string) []string {
	switch s.Type {
	case ShellBash, ShellZsh, ShellSh:
		return []string{s.Path, "-c", command}
	case ShellPowerShell:
		return []string{s.Path, "-NoProfile", "-Command", command}
	case ShellCmd:
		return []string{s.Path, "/c", command}
	}
	return []string{s.Path, "-c", command}
}

// IsUnix returns true for bash, zsh, and sh shell types.
func (s Shell) IsUnix() bool {
	switch s.Type {
	case ShellBash, ShellZsh, ShellSh:
		return true
	}
	return false
}
