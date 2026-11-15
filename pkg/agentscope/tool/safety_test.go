package tool

import (
	"testing"
)

func TestCheckDangerousCommand(t *testing.T) {
	tests := []struct {
		cmd      string
		wantDang bool
	}{
		{"ls -la", false},
		{"cat file.txt", false},
		{"rm -rf /", true},
		{"sudo rm -f file", true},
		{"dd if=/dev/zero of=/dev/sda", true},
		{"mkfs.ext4 /dev/sdb1", true},
		{"chmod 777 /tmp/script.sh", true},
		{"chmod -R 777 .", true},
		{"chown -R root:root /etc", true},
		{"kill -9 1234", true},
		{"echo test > /dev/null", true},
		{"echo hello", false},
		{"grep pattern file", false},
	}
	for _, tt := range tests {
		got, _ := CheckDangerousCommand(tt.cmd)
		if got != tt.wantDang {
			t.Errorf("CheckDangerousCommand(%q) = %v, want %v", tt.cmd, got, tt.wantDang)
		}
	}
}

func TestCheckDangerousPath(t *testing.T) {
	tests := []struct {
		path     string
		wantDang bool
	}{
		{"src/main.go", false},
		{"README.md", false},
		{".bashrc", true},
		{".env", true},
		{".env.production", true},
		{"/home/user/.ssh/config", true},
		{".ssh/authorized_keys", true},
		{".git/config", true},
		{".gitconfig", true},
		{".vscode/settings.json", true},
		{".idea/workspace.xml", true},
		{".npmrc", true},
		{".netrc", true},
		{"/tmp/safe/file.txt", false},
	}
	for _, tt := range tests {
		got, _ := CheckDangerousPath(tt.path)
		if got != tt.wantDang {
			t.Errorf("CheckDangerousPath(%q) = %v, want %v", tt.path, got, tt.wantDang)
		}
	}
}
