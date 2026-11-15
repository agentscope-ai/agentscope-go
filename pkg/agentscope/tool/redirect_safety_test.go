package tool

import (
	"runtime"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/permission"
)

// TestIsReadOnlyCommand_RejectsWriteRedirects proves the redirect-bypass fix:
// a command whose only executable is read-only but which redirects output to a
// file must NOT be classified read-only (otherwise it is auto-allowed with no
// prompt in every permission mode).
func TestIsReadOnlyCommand_RejectsWriteRedirects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	notReadOnly := []string{
		"cat foo > /etc/passwd",
		"echo pwned >> /home/user/.ssh/authorized_keys",
		"cat template > /etc/cron.d/backdoor",
		"echo x >| out",  // clobber
		"echo x &> log",  // redirect all
		"echo x &>> log", // append all
		"cat in <> file", // read-write open
	}
	for _, c := range notReadOnly {
		if IsReadOnlyCommand(c) {
			t.Errorf("expected NOT read-only (writes via redirect): %q", c)
		}
	}
}

// TestIsReadOnlyCommand_AllowsSafeRedirectsAndReads guards against over-blocking:
// fd duplication (2>&1) and input redirects are still read-only.
func TestIsReadOnlyCommand_AllowsSafeRedirectsAndReads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	readOnly := []string{
		"cat foo",
		"ls 2>&1",               // fd duplication, not a file write
		"grep foo < input.txt",  // input redirect
		"cat a b | grep needle", // pipeline of reads
	}
	for _, c := range readOnly {
		if !IsReadOnlyCommand(c) {
			t.Errorf("expected read-only: %q", c)
		}
	}
}

// TestIsReadOnlyCommand_CurlWgetNotReadOnly proves curl/wget are removed from the
// read-only allowlist (they enable SSRF / data exfiltration).
func TestIsReadOnlyCommand_CurlWgetNotReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	egress := []string{
		"curl http://example.com",
		"wget http://example.com/x.sh",
		"curl http://169.254.169.254/latest/meta-data/",
	}
	for _, c := range egress {
		if IsReadOnlyCommand(c) {
			t.Errorf("expected NOT read-only (network egress): %q", c)
		}
	}
}

// TestCheckDangerousRedirect verifies redirect targets are routed through a
// bypass-immune dangerous-path check covering dotfiles AND system paths.
func TestCheckDangerousRedirect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	dangerous := []string{
		"cat x > /etc/passwd",
		"echo pwned >> /home/user/.ssh/authorized_keys",
		"cat t > /etc/cron.d/backdoor",
		"echo x > /usr/bin/evil",
		"echo x > /root/.bashrc",
	}
	for _, c := range dangerous {
		if ok, _ := CheckDangerousRedirect(c); !ok {
			t.Errorf("expected dangerous redirect: %q", c)
		}
	}
	safe := []string{
		"echo x > out.txt",
		"echo x > ./build/log.txt",
		"ls 2>&1",
		"cat a",
	}
	for _, c := range safe {
		if ok, _ := CheckDangerousRedirect(c); ok {
			t.Errorf("expected safe redirect target: %q", c)
		}
	}
}

// TestBashCheckPermissions_RedirectBypassImmune is the end-to-end guard: writing
// to a system path via redirect must ASK (bypass-immune) even in AcceptEdits mode,
// never auto-allow.
func TestBashCheckPermissions_RedirectBypassImmune(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell")
	}
	bt := BashTool()
	ctx := &permission.Context{Mode: permission.ModeAcceptEdits}
	d := bt.CheckPermissions(map[string]any{"command": "cat x > /etc/passwd"}, ctx)
	if d.Behavior != permission.BehaviorAsk {
		t.Fatalf("expected Ask for redirect to /etc/passwd, got %v", d.Behavior)
	}
	if !d.BypassImmune {
		t.Errorf("expected BypassImmune=true for redirect to system path")
	}
}
