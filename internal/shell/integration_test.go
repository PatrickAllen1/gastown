// ABOUTME: Tests for shell integration install/remove functionality.
// ABOUTME: Verifies RC file manipulation and hook script creation.

package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectShell(t *testing.T) {
	tests := []struct {
		shellEnv string
		want     string
	}{
		{"/bin/zsh", "zsh"},
		{"/usr/bin/zsh", "zsh"},
		{"/bin/bash", "bash"},
		{"/usr/bin/bash", "bash"},
		{"", "zsh"},
	}

	for _, tt := range tests {
		t.Run(tt.shellEnv, func(t *testing.T) {
			orig := os.Getenv("SHELL")
			defer os.Setenv("SHELL", orig)

			os.Setenv("SHELL", tt.shellEnv)
			got := DetectShell()
			if got != tt.want {
				t.Errorf("DetectShell() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRCFilePath(t *testing.T) {
	home, _ := os.UserHomeDir()

	tests := []struct {
		shell string
		want  string
	}{
		{"zsh", filepath.Join(home, ".zshrc")},
		{"bash", filepath.Join(home, ".bashrc")},
	}

	for _, tt := range tests {
		t.Run(tt.shell, func(t *testing.T) {
			got := RCFilePath(tt.shell)
			if got != tt.want {
				t.Errorf("RCFilePath(%q) = %q, want %q", tt.shell, got, tt.want)
			}
		})
	}
}

func TestAddRemoveFromRCFile(t *testing.T) {
	tmpDir := t.TempDir()
	rcPath := filepath.Join(tmpDir, ".zshrc")

	originalContent := "# existing content\nalias foo=bar\n"
	if err := os.WriteFile(rcPath, []byte(originalContent), 0644); err != nil {
		t.Fatal(err)
	}

	if err := addToRCFile(rcPath); err != nil {
		t.Fatalf("addToRCFile() error = %v", err)
	}

	data, err := os.ReadFile(rcPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	if !strings.Contains(content, markerStart) {
		t.Error("RC file should contain start marker")
	}
	if !strings.Contains(content, markerEnd) {
		t.Error("RC file should contain end marker")
	}
	if !strings.Contains(content, "shell-hook.sh") {
		t.Error("RC file should source shell-hook.sh")
	}
	if !strings.Contains(content, "# existing content") {
		t.Error("RC file should preserve original content")
	}

	if err := removeFromRCFile(rcPath); err != nil {
		t.Fatalf("removeFromRCFile() error = %v", err)
	}

	data, err = os.ReadFile(rcPath)
	if err != nil {
		t.Fatal(err)
	}
	content = string(data)

	if strings.Contains(content, markerStart) {
		t.Error("RC file should not contain start marker after removal")
	}
	if strings.Contains(content, markerEnd) {
		t.Error("RC file should not contain end marker after removal")
	}
	if !strings.Contains(content, "# existing content") {
		t.Error("RC file should preserve original content after removal")
	}
}

func TestUpdateRCFile(t *testing.T) {
	tmpDir := t.TempDir()
	rcPath := filepath.Join(tmpDir, ".zshrc")

	if err := addToRCFile(rcPath); err != nil {
		t.Fatalf("initial addToRCFile() error = %v", err)
	}

	if err := addToRCFile(rcPath); err != nil {
		t.Fatalf("second addToRCFile() error = %v", err)
	}

	data, _ := os.ReadFile(rcPath)
	content := string(data)

	startCount := strings.Count(content, markerStart)
	if startCount != 1 {
		t.Errorf("RC file has %d start markers, want 1", startCount)
	}
}

func TestShellHook_NounsetSafeGasTownEnv(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is required for nounset test: %v", err)
	}

	home := t.TempDir()
	hookPath := filepath.Join(home, "shell-hook.sh")
	if err := os.WriteFile(hookPath, []byte(shellHookScript), 0644); err != nil {
		t.Fatalf("writing shell hook: %v", err)
	}

	tests := []struct {
		name        string
		env         []string
		wantEnabled string
	}{
		{name: "unset", wantEnabled: "0"},
		{name: "enabled", env: []string{"GASTOWN_ENABLED=1"}, wantEnabled: "1"},
		{name: "disabled", env: []string{"GASTOWN_DISABLED=1"}, wantEnabled: "0"},
		{
			name:        "disabled takes precedence",
			env:         []string{"GASTOWN_DISABLED=1", "GASTOWN_ENABLED=1"},
			wantEnabled: "0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(bashPath, "-u", "-c", `
source "$1"
if _gastown_enabled; then
    result=1
else
    result=0
fi
[[ "$result" == "$2" ]]
`, "bash", hookPath, tt.wantEnabled)
			cmd.Dir = t.TempDir()
			cmd.Env = append(envWithout("GASTOWN_DISABLED", "GASTOWN_ENABLED", "HOME", "SHELL"),
				append([]string{"HOME=" + home, "SHELL=/bin/bash"}, tt.env...)...)

			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("bash -u shell hook failed: %v\nOutput: %s", err, output)
			}
		})
	}
}

func envWithout(keys ...string) []string {
	blocked := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		blocked[key] = struct{}{}
	}

	filtered := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, found := blocked[key]; !found {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
