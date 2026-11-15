package tool

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// DangerousFiles lists files that should be protected from auto-editing.
// Includes shell configs, credential files, and environment secrets.
var DangerousFiles = []string{
	// Shell configs
	".gitconfig",
	".gitmodules",
	".bashrc",
	".bash_profile",
	".zshrc",
	".zprofile",
	".profile",
	// SSH
	".ssh/config",
	".ssh/authorized_keys",
	".ssh/id_rsa",
	".ssh/id_ed25519",
	".ssh/id_ecdsa",
	".ssh/id_dsa",
	".ssh/known_hosts",
	// Cloud provider credentials
	".aws/credentials",
	".aws/config",
	".azure/credentials",
	".config/gcloud/credentials.db",
	".config/gcloud/application_default_credentials.json",
	// Container / Kubernetes credentials
	".kube/config",
	".docker/config.json",
	".helm/repository/repositories.yaml",
	// Package manager configs (can contain registry tokens)
	".netrc",
	".npmrc",
	".pypirc",
	".gem/credentials",
	".cargo/credentials.toml",
	".composer/auth.json",
	// GnuPG
	".gnupg/secring.gpg",
	".gnupg/trustdb.gpg",
	// Environment secrets
	".env",
	".envrc",
	".env.local",
	".env.development",
	".env.development.local",
	".env.test",
	".env.test.local",
	".env.staging",
	".env.production",
	".env.production.local",
}

// DangerousDirectories lists directories that contain sensitive config or executables.
var DangerousDirectories = []string{
	".git",
	".vscode",
	".idea",
	".ssh",
	".gnupg",
	".kube",
	".aws",
	".docker",
}

// systemPathPrefixes are absolute directory prefixes that must never be written
// to (system integrity + persistence surfaces such as cron). /dev and /var are
// intentionally excluded: `> /dev/null` is ubiquitous, /var holds temp/logs, and
// dangerous device writes like `> /dev/sda` are caught by DangerousCommandPatterns.
var systemPathPrefixes = []string{
	"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64",
	"/boot", "/sys", "/proc", "/root",
}

// isSystemPath reports whether p is, or is under, a protected system directory.
func isSystemPath(p string) bool {
	clean := filepath.Clean(p)
	if clean == "/" {
		return true
	}
	for _, prefix := range systemPathPrefixes {
		if clean == prefix || strings.HasPrefix(clean, prefix+"/") {
			return true
		}
	}
	return false
}

// DangerousCommandPatterns lists command patterns (word-boundary aware) that can
// cause data loss, system damage, or security issues.
var DangerousCommandPatterns = []string{
	"rm -rf",
	"sudo rm",
	"dd ",
	"mkfs",
	"fdisk",
	"format ",
	"chmod 777",
	"chmod -R 777",
	"chown -R",
	"kill -9",
	"> /dev/",
	// Windows / PowerShell patterns
	"del /s",
	"rmdir /s",
	"rd /s",
	"Remove-Item.*-Recurse",
	"Format-Volume",
	"Stop-Process.*-Force",
	"Clear-Content",
}

var dangerousRegexPatterns []*regexp.Regexp
var dangerousLiteralPatterns []string

func init() {
	for _, p := range DangerousCommandPatterns {
		if strings.Contains(p, ".*") || strings.Contains(p, ".+") {
			dangerousRegexPatterns = append(dangerousRegexPatterns,
				regexp.MustCompile("(?i)"+p))
		} else {
			dangerousLiteralPatterns = append(dangerousLiteralPatterns, p)
		}
	}
}

// CheckDangerousCommand returns true if cmd contains a dangerous command pattern.
// It first attempts AST-based analysis via CheckDangerousRemoval and CheckInjectionRisk,
// then falls back to simple string matching for patterns not covered by the parser.
func CheckDangerousCommand(cmd string) (bool, string) {
	// AST-based checks first
	if dangerous, reason := CheckDangerousRemoval(cmd); dangerous {
		return true, reason
	}
	if risky, reason := CheckInjectionRisk(cmd); risky {
		return true, reason
	}

	// Literal pattern matching
	lower := strings.ToLower(cmd)
	for _, pattern := range dangerousLiteralPatterns {
		if strings.Contains(lower, strings.ToLower(pattern)) {
			return true, fmt.Sprintf("command contains dangerous pattern %q", pattern)
		}
	}

	// Regex pattern matching
	for _, re := range dangerousRegexPatterns {
		if re.MatchString(cmd) {
			return true, fmt.Sprintf("command matches dangerous pattern %q", re.String())
		}
	}
	return false, ""
}

// CheckDangerousPath returns true if path touches a dangerous file or directory.
func CheckDangerousPath(path string) (bool, string) {
	clean := filepath.Clean(path)

	for _, df := range DangerousFiles {
		if matchesDangerousFile(clean, df) {
			return true, fmt.Sprintf("path touches dangerous file %q", df)
		}
	}

	parts := strings.Split(clean, string(filepath.Separator))
	for _, part := range parts {
		for _, dd := range DangerousDirectories {
			if part == dd {
				return true, fmt.Sprintf("path traverses dangerous directory %q", dd)
			}
		}
	}

	return false, ""
}

// CheckDangerousFilePaths extracts file paths from a command and checks each
// against the dangerous files/directories list. Returns true if any path is dangerous.
func CheckDangerousFilePaths(cmd string) (bool, string) {
	paths := ExtractFilePaths(cmd)
	for _, p := range paths {
		if dangerous, reason := CheckDangerousPath(p); dangerous {
			return true, reason
		}
	}
	return false, ""
}

func matchesDangerousFile(path, dangerous string) bool {
	if filepath.Base(path) == filepath.Base(dangerous) {
		if !strings.Contains(dangerous, "/") {
			return true
		}
		return strings.HasSuffix(filepath.ToSlash(path), filepath.ToSlash(dangerous))
	}
	return false
}
