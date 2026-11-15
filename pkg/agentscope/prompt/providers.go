package prompt

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/skill"
)

// Section name and priority constants for built-in providers.
const (
	SectionProjectConfig = "project_config"
	SectionEnvironment   = "environment"
	SectionSkills        = "skills"
	SectionGitStatus     = "git_status"

	PriorityProjectConfig = 10
	PriorityEnvironment   = 20
	PriorityGitStatus     = 30
	PrioritySkills        = 40
)

// ProjectConfigProvider reads a project configuration document from dir. It
// looks for CLAUDE.md, then AGENTS.md, then README.md and returns the first
// found as a section. If none exist, it returns an empty section (no error).
func ProjectConfigProvider(dir string) (PromptSection, error) {
	candidates := []string{"CLAUDE.md", "AGENTS.md", "README.md"}
	for _, name := range candidates {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return PromptSection{}, fmt.Errorf("reading %s: %w", path, err)
		}
		content := fmt.Sprintf("# Project configuration (%s)\n\n%s", name, strings.TrimSpace(string(data)))
		return PromptSection{
			Name:     SectionProjectConfig,
			Priority: PriorityProjectConfig,
			Content:  content,
		}, nil
	}
	return PromptSection{Name: SectionProjectConfig, Priority: PriorityProjectConfig}, nil
}

// EnvironmentProvider generates a section describing the runtime environment:
// OS, shell, and current working directory.
func EnvironmentProvider() PromptSection {
	var b strings.Builder
	b.WriteString("# Environment\n\n")
	fmt.Fprintf(&b, "- OS: %s\n", runtime.GOOS)
	fmt.Fprintf(&b, "- Arch: %s\n", runtime.GOARCH)

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "unknown"
	}
	fmt.Fprintf(&b, "- Shell: %s\n", shell)

	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd = "unknown"
	}
	fmt.Fprintf(&b, "- Working directory: %s\n", cwd)

	return PromptSection{
		Name:     SectionEnvironment,
		Priority: PriorityEnvironment,
		Content:  strings.TrimRight(b.String(), "\n"),
	}
}

// SkillListProvider generates a section listing available skills, reusing
// skill.FormatSkillInstructions. Returns an empty section when there are no
// skills.
func SkillListProvider(skills []skill.Skill) PromptSection {
	instructions := skill.FormatSkillInstructions(skills)
	return PromptSection{
		Name:     SectionSkills,
		Priority: PrioritySkills,
		Content:  instructions,
	}
}

// GitStatusProvider generates a section with git branch, clean/dirty state, and
// recent commits for the repository at dir. If dir is not a git repository (or
// git is unavailable), it returns an empty section without an error.
func GitStatusProvider(dir string) (PromptSection, error) {
	empty := PromptSection{Name: SectionGitStatus, Priority: PriorityGitStatus}

	if _, err := exec.LookPath("git"); err != nil {
		return empty, nil
	}

	// Confirm dir is inside a git work tree; if not, return empty gracefully.
	if out, err := gitOutput(dir, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return empty, nil
	}

	branch, err := gitOutput(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return empty, nil
	}
	branch = strings.TrimSpace(branch)

	status, err := gitOutput(dir, "status", "--porcelain")
	if err != nil {
		return empty, nil
	}
	state := "clean"
	if strings.TrimSpace(status) != "" {
		state = "dirty"
	}

	var b strings.Builder
	b.WriteString("# Git status\n\n")
	fmt.Fprintf(&b, "- Branch: %s\n", branch)
	fmt.Fprintf(&b, "- Working tree: %s\n", state)

	if commits, err := gitOutput(dir, "log", "-5", "--oneline"); err == nil {
		commits = strings.TrimSpace(commits)
		if commits != "" {
			b.WriteString("- Recent commits:\n")
			for _, line := range strings.Split(commits, "\n") {
				fmt.Fprintf(&b, "  %s\n", line)
			}
		}
	}

	return PromptSection{
		Name:     SectionGitStatus,
		Priority: PriorityGitStatus,
		Content:  strings.TrimRight(b.String(), "\n"),
	}, nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
