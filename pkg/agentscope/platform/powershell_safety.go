package platform

import "regexp"

var psDangerousPatterns = []*struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)Remove-Item\s+.*-Recurse`), "recursive file deletion"},
	{regexp.MustCompile(`(?i)Format-Volume`), "disk formatting"},
	{regexp.MustCompile(`(?i)Stop-Process\s+.*-Force`), "forceful process termination"},
	{regexp.MustCompile(`(?i)Set-ExecutionPolicy`), "execution policy change"},
	{regexp.MustCompile(`(?i)Invoke-Expression`), "dynamic code execution (Invoke-Expression)"},
	{regexp.MustCompile(`(?i)Start-Process.*-Verb\s+RunAs`), "privilege escalation (RunAs)"},
	{regexp.MustCompile(`(?i)Clear-Content\s+.*-Recurse`), "recursive content clearing"},
	{regexp.MustCompile(`(?i)Remove-PSDrive`), "drive removal"},
	{regexp.MustCompile(`(?i)New-Service`), "service creation"},
	{regexp.MustCompile(`(?i)Set-MpPreference\s+.*-DisableRealtimeMonitoring`), "antivirus disable"},
}

// CheckPowerShellDangerous returns true if the command matches a known
// dangerous PowerShell pattern. Returns the reason string for the match.
func CheckPowerShellDangerous(command string) (bool, string) {
	for _, p := range psDangerousPatterns {
		if p.re.MatchString(command) {
			return true, p.reason
		}
	}
	return false, ""
}
