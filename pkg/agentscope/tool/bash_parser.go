package tool

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/platform"
	"mvdan.cc/sh/v3/syntax"
)

func isWindowsShell() bool {
	s := platform.Detect()
	return s.Type == platform.ShellPowerShell || s.Type == platform.ShellCmd
}

var powerShellReadOnlyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(Get-ChildItem|gci|dir|ls)\b`),
	regexp.MustCompile(`(?i)^(Get-Content|gc|cat|type)\b`),
	regexp.MustCompile(`(?i)^(Select-String|sls)\b`),
	regexp.MustCompile(`(?i)^(Get-Process|gps)\b`),
	regexp.MustCompile(`(?i)^(Get-Location|gl|pwd)\b`),
	regexp.MustCompile(`(?i)^(Get-Item|gi)\b`),
	regexp.MustCompile(`(?i)^(Get-ItemProperty|gp)\b`),
	regexp.MustCompile(`(?i)^(Test-Path)\b`),
	regexp.MustCompile(`(?i)^(Measure-Object)\b`),
	regexp.MustCompile(`(?i)^(Get-Date)\b`),
	regexp.MustCompile(`(?i)^(Get-Host)\b`),
	regexp.MustCompile(`(?i)^(Get-Variable|gv)\b`),
	regexp.MustCompile(`(?i)^(Write-Host|Write-Output|echo)\b`),
	regexp.MustCompile(`(?i)^(Get-Service|gsv)\b`),
	regexp.MustCompile(`(?i)^(Get-Command|gcm)\b`),
	regexp.MustCompile(`(?i)^(Get-Help)\b`),
	regexp.MustCompile(`(?i)^(Get-Member|gm)\b`),
	regexp.MustCompile(`(?i)^(Format-Table|ft)\b`),
	regexp.MustCompile(`(?i)^(Format-List|fl)\b`),
	regexp.MustCompile(`(?i)^(Where-Object|where)\b`),
	regexp.MustCompile(`(?i)^(Sort-Object|sort)\b`),
}

var cmdReadOnlyCommands = map[string]bool{
	"dir": true, "type": true, "find": true, "findstr": true,
	"where": true, "echo": true, "set": true, "ver": true,
	"hostname": true, "whoami": true, "date": true, "time": true,
	"tree": true, "more": true, "sort": true,
}

var powerShellInjectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)Invoke-Expression`),
	regexp.MustCompile(`(?i)\biex\b`),
	regexp.MustCompile(`(?i)&\s*\$`),
	regexp.MustCompile(`(?i)\.\s*\$`),
	regexp.MustCompile(`(?i)Invoke-Command.*-ScriptBlock`),
	regexp.MustCompile(`(?i)\bpowershell\s+-[eE]`),
	regexp.MustCompile(`(?i)\bpwsh\s+-[eE]`),
}

var powerShellDangerousRemovalPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)Remove-Item\s+.*-Recurse`),
	regexp.MustCompile(`(?i)\bdel\s+/[sS]`),
	regexp.MustCompile(`(?i)\brmdir\s+/[sS]`),
	regexp.MustCompile(`(?i)\brd\s+/[sS]`),
}

func isWindowsReadOnlyCommand(cmd string) bool {
	shell := platform.Detect()
	return checkWindowsReadOnly(cmd, shell.Type)
}

func checkWindowsReadOnly(cmd string, shellType platform.ShellType) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	if shellType == platform.ShellPowerShell {
		return isPowerShellReadOnly(cmd)
	}
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return false
	}
	return cmdReadOnlyCommands[strings.ToLower(parts[0])]
}

func isPowerShellReadOnly(cmd string) bool {
	if strings.TrimSpace(cmd) == "" {
		return false
	}
	segments := strings.Split(cmd, "|")
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		matched := false
		for _, re := range powerShellReadOnlyPatterns {
			if re.MatchString(seg) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return len(segments) > 0
}

// ReadOnlyCommands is the set of commands considered safe (read-only).
var ReadOnlyCommands = map[string]bool{
	"cat": true, "head": true, "tail": true, "less": true, "more": true,
	"ls": true, "dir": true, "find": true, "locate": true, "which": true,
	"whereis": true, "file": true, "stat": true, "wc": true, "du": true,
	"df": true, "free": true, "uptime": true, "uname": true, "hostname": true,
	"whoami": true, "id": true, "groups": true, "env": true, "printenv": true,
	"echo": true, "printf": true, "true": true, "false": true, "test": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true,
	// awk is NOT read-only: it can execute arbitrary commands via system()
	// and write to files via output redirection within awk programs.
	"sed":  false, // sed can modify files, handled separately
	"sort": true, "uniq": true, "cut": true, "tr": true, "paste": true,
	"diff": true, "cmp": true, "comm": true,
	"date": true, "cal": true, "bc": true, "expr": true,
	"ps": true, "top": true, "htop": true, "pgrep": true,
	"git":    true, // git subcommands are checked separately below
	"pwd":    true,
	"type":   true,
	"man":    true,
	"help":   true,
	"jq":     true,
	"yq":     true,
	"tree":   true,
	"column": true,
	"md5sum": true, "sha256sum": true, "sha1sum": true,
	"xxd": true, "hexdump": true, "od": true,
	// curl/wget are intentionally NOT read-only: they enable network egress
	// (SSRF to cloud metadata endpoints, data exfiltration, remote payload fetch).
	"ping": true, "traceroute": true, "nslookup": true, "dig": true, "host": true,
	"ip": true, "ifconfig": true, "netstat": true, "ss": true,
	"realpath": true, "basename": true, "dirname": true, "readlink": true,
}

// ReadOnlyGitSubcommands lists git subcommands that are read-only.
var ReadOnlyGitSubcommands = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "branch": true,
	"tag": true, "describe": true, "rev-parse": true, "rev-list": true,
	"ls-files": true, "ls-tree": true, "ls-remote": true, "remote": true,
	"config": true, "blame": true, "shortlog": true, "stash": true,
	"reflog": true, "cat-file": true, "name-rev": true,
}

// parseBashAST parses a bash command string into an AST.
func parseBashAST(cmd string) (*syntax.File, error) {
	parser := syntax.NewParser(syntax.KeepComments(false), syntax.Variant(syntax.LangBash))
	f, err := parser.Parse(strings.NewReader(cmd), "")
	if err != nil {
		return nil, fmt.Errorf("parse bash: %w", err)
	}
	return f, nil
}

// IsReadOnlyCommand checks whether a bash command is read-only by parsing it
// into an AST and checking each simple command against the ReadOnlyCommands set.
// Returns true only if ALL commands in the pipeline/script are read-only.
func IsReadOnlyCommand(cmd string) bool {
	if isWindowsShell() {
		return isWindowsReadOnlyCommand(cmd)
	}

	f, err := parseBashAST(cmd)
	if err != nil {
		return false // can't parse => not safe to assume read-only
	}

	// A command that redirects output to a file is never read-only, even if the
	// executable itself is (e.g. `cat x > /etc/passwd`). File-descriptor
	// duplications (2>&1) and input redirects are ignored.
	if len(extractWriteRedirectTargets(f)) > 0 {
		return false
	}

	allReadOnly := true
	hasCommands := false

	syntax.Walk(f, func(node syntax.Node) bool {
		if !allReadOnly {
			return false
		}
		callExpr, ok := node.(*syntax.CallExpr)
		if !ok || len(callExpr.Args) == 0 {
			return true
		}

		hasCommands = true
		name := extractCommandName(callExpr.Args[0])
		if name == "" {
			allReadOnly = false
			return false
		}

		// Check if the command is in our read-only set
		if !ReadOnlyCommands[name] {
			allReadOnly = false
			return false
		}

		// Special handling for git: check subcommand
		if name == "git" && len(callExpr.Args) > 1 {
			sub := extractWordLiteral(callExpr.Args[1])
			if sub != "" && !ReadOnlyGitSubcommands[sub] {
				allReadOnly = false
				return false
			}
		}

		return true
	})

	return hasCommands && allReadOnly
}

// isWriteRedirOp reports whether a redirection operator writes to its file target.
func isWriteRedirOp(op syntax.RedirOperator) bool {
	switch op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrInOut,
		syntax.RdrClob, syntax.AppClob,
		syntax.RdrAll, syntax.RdrAllClob, syntax.AppAll, syntax.AppAllClob:
		return true
	}
	return false
}

// isAllDigits reports whether s is a non-empty run of ASCII digits (a file
// descriptor number such as the "1" in `2>&1`).
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// extractWriteRedirectTargets returns the file targets of output redirections in
// cmd's AST. File-descriptor duplications (e.g. `2>&1`) and input redirections
// (`<`, heredocs) are ignored. A write to a non-literal target (e.g. `> $LOG`)
// is represented by an empty string so callers can decide how conservative to be.
func extractWriteRedirectTargets(f *syntax.File) []string {
	var targets []string
	syntax.Walk(f, func(node syntax.Node) bool {
		stmt, ok := node.(*syntax.Stmt)
		if !ok {
			return true
		}
		for _, r := range stmt.Redirs {
			if r == nil || r.Word == nil {
				continue
			}
			switch {
			case isWriteRedirOp(r.Op):
				targets = append(targets, extractWordLiteral(r.Word))
			case r.Op == syntax.DplOut:
				// `>&x`: a file target unless x is a bare fd number (e.g. 2>&1).
				if w := extractWordLiteral(r.Word); w != "" && !isAllDigits(w) {
					targets = append(targets, w)
				}
			}
		}
		return true
	})
	return targets
}

// CheckDangerousRedirect reports whether cmd redirects output to a dangerous
// path — a sensitive dotfile/config (via CheckDangerousPath) or a system
// directory (via isSystemPath). Non-literal targets are left to the normal
// permission flow rather than flagged here to avoid false positives.
func CheckDangerousRedirect(cmd string) (bool, string) {
	if isWindowsShell() {
		return false, "" // Windows redirects covered by PowerShell dangerous patterns
	}
	f, err := parseBashAST(cmd)
	if err != nil {
		return false, ""
	}
	for _, target := range extractWriteRedirectTargets(f) {
		if target == "" {
			continue
		}
		if dangerous, reason := CheckDangerousPath(target); dangerous {
			return true, reason
		}
		if isSystemPath(target) {
			return true, fmt.Sprintf("output redirected to system path %q", target)
		}
	}
	return false, ""
}

// CheckInjectionRisk detects potentially dangerous shell constructs:

// CheckInjectionRisk detects potentially dangerous shell constructs:
// command substitution $(...) or `...`, process substitution <(...) or >(...),
// eval, exec, source, and similar.
func CheckInjectionRisk(cmd string) (bool, string) {
	if isWindowsShell() {
		for _, re := range powerShellInjectionPatterns {
			if re.MatchString(cmd) {
				return true, "command matches PowerShell injection pattern"
			}
		}
		return false, ""
	}

	f, err := parseBashAST(cmd)
	if err != nil {
		return true, "command could not be parsed as valid bash"
	}

	var risk bool
	var reason string

	syntax.Walk(f, func(node syntax.Node) bool {
		if risk {
			return false
		}
		switch n := node.(type) {
		case *syntax.CmdSubst:
			risk = true
			reason = "contains command substitution $(...)  or `...`"
		case *syntax.ProcSubst:
			risk = true
			reason = "contains process substitution"
		case *syntax.CallExpr:
			if len(n.Args) > 0 {
				name := extractCommandName(n.Args[0])
				switch name {
				case "eval":
					risk = true
					reason = "uses eval which can execute arbitrary code"
				case "exec":
					risk = true
					reason = "uses exec which replaces the current process"
				case "source", ".":
					risk = true
					reason = "uses source/. which can execute arbitrary scripts"
				}
			}
		}
		return true
	})

	return risk, reason
}

// CheckDangerousRemoval detects potentially dangerous rm commands that could
// cause significant data loss (recursive removal, force removal of system paths, etc.).
func CheckDangerousRemoval(cmd string) (bool, string) {
	if isWindowsShell() {
		for _, re := range powerShellDangerousRemovalPatterns {
			if re.MatchString(cmd) {
				return true, "command matches dangerous removal pattern"
			}
		}
		return false, ""
	}

	f, err := parseBashAST(cmd)
	if err != nil {
		return false, ""
	}

	var dangerous bool
	var reason string

	syntax.Walk(f, func(node syntax.Node) bool {
		if dangerous {
			return false
		}
		callExpr, ok := node.(*syntax.CallExpr)
		if !ok || len(callExpr.Args) == 0 {
			return true
		}

		name := extractCommandName(callExpr.Args[0])
		if name != "rm" {
			return true
		}

		hasRecursive := false
		hasForce := false
		var targets []string

		for _, arg := range callExpr.Args[1:] {
			word := extractWordLiteral(arg)
			if word == "" {
				continue
			}
			if strings.HasPrefix(word, "-") {
				if strings.Contains(word, "r") || strings.Contains(word, "R") {
					hasRecursive = true
				}
				if strings.Contains(word, "f") {
					hasForce = true
				}
				continue
			}
			targets = append(targets, word)
		}

		if hasRecursive && hasForce {
			dangerous = true
			reason = "rm -rf detected"
			return false
		}

		for _, t := range targets {
			if t == "/" || t == "/*" || t == "~" || t == "~/" ||
				strings.HasPrefix(t, "/etc") || strings.HasPrefix(t, "/usr") ||
				strings.HasPrefix(t, "/var") || strings.HasPrefix(t, "/home") ||
				strings.HasPrefix(t, "/root") || strings.HasPrefix(t, "/sys") ||
				strings.HasPrefix(t, "/boot") || strings.HasPrefix(t, "/bin") ||
				strings.HasPrefix(t, "/sbin") || strings.HasPrefix(t, "/lib") {
				dangerous = true
				reason = fmt.Sprintf("rm targets system path %q", t)
				return false
			}
		}

		return true
	})

	return dangerous, reason
}

// ExtractFilePaths extracts file path arguments from commands that operate on
// files (rm, mv, cp, chmod, chown, etc.).
func ExtractFilePaths(cmd string) []string {
	f, err := parseBashAST(cmd)
	if err != nil {
		return nil
	}

	fileCommands := map[string]bool{
		"rm": true, "mv": true, "cp": true, "chmod": true, "chown": true,
		"chgrp": true, "touch": true, "mkdir": true, "rmdir": true,
		"ln": true, "install": true,
	}

	var paths []string
	syntax.Walk(f, func(node syntax.Node) bool {
		callExpr, ok := node.(*syntax.CallExpr)
		if !ok || len(callExpr.Args) == 0 {
			return true
		}

		name := extractCommandName(callExpr.Args[0])
		if !fileCommands[name] {
			return true
		}

		for _, arg := range callExpr.Args[1:] {
			word := extractWordLiteral(arg)
			if word == "" || strings.HasPrefix(word, "-") {
				continue
			}
			// Skip mode arguments for chmod (e.g., "755")
			if name == "chmod" && isChmodMode(word) {
				continue
			}
			// Skip user:group arguments for chown
			if name == "chown" && strings.Contains(word, ":") {
				continue
			}
			paths = append(paths, word)
		}

		return true
	})

	return paths
}

// ExtractCommandPrefixes extracts command name prefixes for permission rule
// suggestions. It returns up to max prefixes like "git *", "npm *", etc.
func ExtractCommandPrefixes(cmd string, max int) []string {
	f, err := parseBashAST(cmd)
	if err != nil {
		return nil
	}

	seen := make(map[string]bool)
	var prefixes []string

	syntax.Walk(f, func(node syntax.Node) bool {
		if len(prefixes) >= max {
			return false
		}
		callExpr, ok := node.(*syntax.CallExpr)
		if !ok || len(callExpr.Args) == 0 {
			return true
		}

		name := extractCommandName(callExpr.Args[0])
		if name == "" {
			return true
		}

		// For git-like commands, include subcommand: "git status"
		prefix := name
		if len(callExpr.Args) > 1 {
			sub := extractWordLiteral(callExpr.Args[1])
			if sub != "" && !strings.HasPrefix(sub, "-") {
				prefix = name + " " + sub
			}
		}

		suggestion := prefix + ":*"
		if !seen[suggestion] {
			seen[suggestion] = true
			prefixes = append(prefixes, suggestion)
		}
		return true
	})

	return prefixes
}

// collectSubcommandTexts returns the source text of every simple command in a
// compound command, including commands inside $(...) and backquote
// substitutions (syntax.Walk descends into them). The boolean is false when
// the command does not parse — callers must fail safe (no rule matches).
func collectSubcommandTexts(cmd string) ([]string, bool) {
	f, err := parseBashAST(cmd)
	if err != nil {
		return nil, false
	}
	src := []byte(cmd)
	seen := make(map[string]bool)
	var texts []string
	syntax.Walk(f, func(node syntax.Node) bool {
		callExpr, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		// A pure assignment (X=1) is not a command; skip it. Its value's
		// $(...)/`...` substitutions are still walked as their own call
		// expressions, so nothing inside escapes rule matching.
		if len(callExpr.Args) == 0 {
			return true
		}
		// Start at the first argument, not the call: mvdan/sh spans leading
		// env assignments into the CallExpr range, and leaving them in the
		// matched text lets "X=1 hostname -f" slip past a "hostname:*" rule.
		posNode := syntax.Node(callExpr.Args[0])
		pos, end := int(posNode.Pos().Offset()), int(callExpr.End().Offset())
		if pos < 0 || end > len(src) || pos >= end {
			return true
		}
		text := strings.TrimSpace(string(src[pos:end]))
		if text != "" && !seen[text] {
			seen[text] = true
			texts = append(texts, text)
		}
		return true
	})
	if len(texts) == 0 {
		// No simple commands (e.g. bare assignments): treat the whole input
		// as one unit so rules can still match it.
		texts = append(texts, strings.TrimSpace(cmd))
	}
	return texts, true
}

// matchSingleCommandRule applies one anchored glob rule to one simple command.
func matchSingleCommandRule(ruleContent, cmd string) bool {
	pattern := ruleToRegex(ruleContent)
	re, err := regexp.Compile("^" + pattern + "$")
	if err != nil {
		return strings.Contains(cmd, ruleContent)
	}
	return re.MatchString(cmd)
}

// sedAllowedFlags is the set of sed flags that are safe to use.
var sedAllowedFlags = map[string]bool{
	"-e": true, "-n": true, "--quiet": true, "--silent": true,
	"-E": true, "-r": true, "--regexp-extended": true,
}

// CheckSedConstraints checks whether a sed command is safe: it must not use -i
// (in-place edit) or target dangerous files.
func CheckSedConstraints(cmd string, dangerousFiles []string) (bool, string) {
	f, err := parseBashAST(cmd)
	if err != nil {
		return false, ""
	}

	var violation bool
	var reason string

	syntax.Walk(f, func(node syntax.Node) bool {
		if violation {
			return false
		}
		callExpr, ok := node.(*syntax.CallExpr)
		if !ok || len(callExpr.Args) == 0 {
			return true
		}

		name := extractCommandName(callExpr.Args[0])
		if name != "sed" {
			return true
		}

		var filePaths []string
		for _, arg := range callExpr.Args[1:] {
			word := extractWordLiteral(arg)
			if word == "" {
				continue
			}
			// Check for in-place editing
			if word == "-i" || strings.HasPrefix(word, "-i") || word == "--in-place" {
				violation = true
				reason = "sed uses -i (in-place editing) which modifies files"
				return false
			}
			// Check for disallowed flags; unrecognized flags are ignored
			// as they may be sed expressions.
			if strings.HasPrefix(word, "-") && !sedAllowedFlags[word] {
				continue
			}
			if !strings.HasPrefix(word, "-") {
				filePaths = append(filePaths, word)
			}
		}

		// Check if any target file is dangerous
		for _, fp := range filePaths {
			for _, df := range dangerousFiles {
				if matchesDangerousFile(fp, df) {
					violation = true
					reason = fmt.Sprintf("sed targets dangerous file %q", df)
					return false
				}
			}
		}

		return true
	})

	return violation, reason
}

// --- helpers ---

// extractCommandName extracts the command name from an AST word node,
// handling simple cases and ignoring complex expansions.
func extractCommandName(word *syntax.Word) string {
	if len(word.Parts) == 0 {
		return ""
	}
	lit, ok := word.Parts[0].(*syntax.Lit)
	if !ok {
		return ""
	}
	// Strip path prefix: /usr/bin/grep -> grep
	name := lit.Value
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	return name
}

// extractWordLiteral extracts a simple literal string from a word node.
// Returns empty string if the word contains expansions or other complex parts.
func extractWordLiteral(word *syntax.Word) string {
	if len(word.Parts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range word.Parts {
		lit, ok := p.(*syntax.Lit)
		if !ok {
			// Contains variable expansion, command substitution, etc.
			return ""
		}
		b.WriteString(lit.Value)
	}
	return b.String()
}

// interpreterCommands maps interpreter binaries to their inline-execution
// flags. When an agent runs e.g. `python3 -c "os.system('rm -rf /')"`, the
// bash AST only sees "python3" as the command; the dangerous payload is
// hidden inside a string literal. This function detects such patterns.
var interpreterCommands = map[string][]string{
	"python":  {"-c"},
	"python2": {"-c"},
	"python3": {"-c"},
	"node":    {"-e", "--eval"},
	"perl":    {"-e", "-E"},
	"ruby":    {"-e"},
	"lua":     {"-e"},
	"php":     {"-r"},
}

// interpreterDangerousAPIs lists language-specific patterns that indicate
// the inline code can escape the interpreter sandbox (shell-out, file I/O,
// network access, process spawning, etc.).
var interpreterDangerousAPIs = []string{
	// Python
	"os.system", "os.popen", "os.exec", "subprocess",
	"os.remove", "os.unlink", "os.rmdir", "shutil.rmtree",
	"__import__",
	// Node.js
	"child_process", "execSync", "spawnSync", "exec(",
	"require('fs')", "require(\"fs\")",
	// Perl
	"system(", "exec(", "qx{", "qx(", "`",
	// Ruby
	"system(", "exec(", "IO.popen", "Kernel.exec",
	// General (any language)
	"eval(", "curl ", "wget ",
}

// CheckInterpreterAttack detects interpreter-wrapped commands where dangerous
// code is hidden inside a string argument (e.g. python3 -c "import os; ...").
// Returns (true, reason) if the command invokes an interpreter with inline
// code that contains dangerous API calls.
func CheckInterpreterAttack(cmd string) (bool, string) {
	if isWindowsShell() {
		return false, "" // Windows covered by PowerShell injection patterns
	}

	f, err := parseBashAST(cmd)
	if err != nil {
		return false, ""
	}

	var attack bool
	var reason string

	syntax.Walk(f, func(node syntax.Node) bool {
		if attack {
			return false
		}
		callExpr, ok := node.(*syntax.CallExpr)
		if !ok || len(callExpr.Args) == 0 {
			return true
		}

		name := extractCommandName(callExpr.Args[0])
		flags, isInterpreter := interpreterCommands[name]
		if !isInterpreter {
			return true
		}

		// Look for the inline-code flag followed by a code string.
		for i := 1; i < len(callExpr.Args); i++ {
			argVal := extractWordLiteral(callExpr.Args[i])
			if argVal == "" {
				continue
			}

			isInlineFlag := false
			for _, flag := range flags {
				if argVal == flag {
					isInlineFlag = true
					break
				}
			}

			if isInlineFlag && i+1 < len(callExpr.Args) {
				// The next argument is the inline code; extract it even if it
				// contains quotes (simple literal extraction).
				codeArg := extractInlineCode(callExpr.Args[i+1])
				if codeArg == "" {
					// Non-literal code (variable expansion etc.) is risky by itself.
					attack = true
					reason = fmt.Sprintf("%s with %s and non-literal code argument", name, argVal)
					return false
				}
				lower := strings.ToLower(codeArg)
				for _, api := range interpreterDangerousAPIs {
					if strings.Contains(lower, strings.ToLower(api)) {
						attack = true
						reason = fmt.Sprintf("%s inline code contains dangerous API %q", name, api)
						return false
					}
				}
			}
		}
		return true
	})

	return attack, reason
}

// extractInlineCode extracts the text content of a word node, including
// content inside quotes. Unlike extractWordLiteral, it handles single-quoted
// and double-quoted strings.
func extractInlineCode(word *syntax.Word) string {
	if len(word.Parts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range word.Parts {
		switch v := p.(type) {
		case *syntax.Lit:
			b.WriteString(v.Value)
		case *syntax.SglQuoted:
			b.WriteString(v.Value)
		case *syntax.DblQuoted:
			for _, dp := range v.Parts {
				if lit, ok := dp.(*syntax.Lit); ok {
					b.WriteString(lit.Value)
				} else {
					return "" // contains expansion — can't statically analyze
				}
			}
		default:
			return "" // complex node — can't analyze
		}
	}
	return b.String()
}

// isChmodMode returns true if s looks like a chmod mode argument (e.g. "755", "u+x").
func isChmodMode(s string) bool {
	if len(s) == 0 {
		return false
	}
	// Numeric modes: 644, 755, 0755
	allDigits := true
	for _, c := range s {
		if c < '0' || c > '9' {
			allDigits = false
			break
		}
	}
	if allDigits && len(s) >= 3 && len(s) <= 4 {
		return true
	}
	// Symbolic modes: u+x, go-w, a+r, etc.
	for _, c := range s {
		switch {
		case c == 'u', c == 'g', c == 'o', c == 'a',
			c == '+', c == '-', c == '=',
			c == 'r', c == 'w', c == 'x', c == 'X',
			c == 's', c == 't':
			continue
		default:
			return false
		}
	}
	return true
}
