package wrappers

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// expectedWrappers is the canonical list of wrapper scripts.
// Keep in sync with Install() and Remove() in wrappers.go.
var expectedWrappers = []string{"gt-codex", "gt-gemini", "gt-opencode"}

func TestEmbeddedScripts_Exist(t *testing.T) {
	t.Parallel()
	for _, name := range expectedWrappers {
		t.Run(name, func(t *testing.T) {
			content, err := scriptsFS.ReadFile("scripts/" + name)
			if err != nil {
				t.Fatalf("Embedded script %s not found: %v", name, err)
			}
			if len(content) == 0 {
				t.Fatalf("Embedded script %s is empty", name)
			}
		})
	}
}

func TestEmbeddedScripts_HaveShebang(t *testing.T) {
	t.Parallel()
	for _, name := range expectedWrappers {
		t.Run(name, func(t *testing.T) {
			content, err := scriptsFS.ReadFile("scripts/" + name)
			if err != nil {
				t.Fatalf("Failed to read %s: %v", name, err)
			}
			if !strings.HasPrefix(string(content), "#!/") {
				t.Errorf("Script %s missing shebang line", name)
			}
		})
	}
}

func TestEmbeddedScripts_HaveExecLine(t *testing.T) {
	t.Parallel()
	// Each wrapper should exec its target binary.
	// gt-codex → exec codex, gt-gemini → exec gemini, gt-opencode → exec opencode
	for _, name := range expectedWrappers {
		t.Run(name, func(t *testing.T) {
			content, err := scriptsFS.ReadFile("scripts/" + name)
			if err != nil {
				t.Fatalf("Failed to read %s: %v", name, err)
			}

			// Extract expected binary name: gt-codex → codex
			binary := strings.TrimPrefix(name, "gt-")
			expectedExec := "exec " + binary

			if !strings.Contains(string(content), expectedExec) {
				t.Errorf("Script %s missing expected exec line %q", name, expectedExec)
			}
		})
	}
}

func TestEmbeddedScripts_HaveGtPrime(t *testing.T) {
	t.Parallel()
	for _, name := range expectedWrappers {
		t.Run(name, func(t *testing.T) {
			content, err := scriptsFS.ReadFile("scripts/" + name)
			if err != nil {
				t.Fatalf("Failed to read %s: %v", name, err)
			}

			if !strings.Contains(string(content), "gt prime") {
				t.Errorf("Script %s should run 'gt prime' before launching agent", name)
			}
		})
	}
}

func TestInstall_CreatesAllWrappers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("wrapper install not supported on Windows")
	}

	// Override HOME to use a temp directory
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	if err := Install(); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	binDir := filepath.Join(tmpHome, "bin")
	for _, name := range expectedWrappers {
		path := filepath.Join(binDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("Wrapper %s not created: %v", name, err)
			continue
		}
		// Check executable permissions
		if info.Mode()&0111 == 0 {
			t.Errorf("Wrapper %s is not executable: mode=%v", name, info.Mode())
		}
	}
}

func TestRemove_CleansUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("wrapper removal not supported on Windows")
	}

	// Override HOME to use a temp directory
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	// Install first
	if err := Install(); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	// Verify files exist
	binDir := filepath.Join(tmpHome, "bin")
	for _, name := range expectedWrappers {
		if _, err := os.Stat(filepath.Join(binDir, name)); err != nil {
			t.Fatalf("Precondition: wrapper %s should exist after Install", name)
		}
	}

	// Remove
	if err := Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	// Verify files are gone
	for _, name := range expectedWrappers {
		if _, err := os.Stat(filepath.Join(binDir, name)); err == nil {
			t.Errorf("Wrapper %s still exists after Remove()", name)
		}
	}
}

func TestRemove_NoErrorWhenMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("wrapper removal not supported on Windows")
	}

	// Override HOME to use a temp directory (no wrappers installed)
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	// Remove should not error when files don't exist
	if err := Remove(); err != nil {
		t.Errorf("Remove() should not error when wrappers don't exist: %v", err)
	}
}

func TestInstall_Idempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("wrapper install not supported on Windows")
	}

	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	// Install twice
	if err := Install(); err != nil {
		t.Fatalf("First Install() error = %v", err)
	}
	if err := Install(); err != nil {
		t.Fatalf("Second Install() error = %v", err)
	}

	// All wrappers should still exist with correct content
	binDir := filepath.Join(tmpHome, "bin")
	for _, name := range expectedWrappers {
		content, err := os.ReadFile(filepath.Join(binDir, name))
		if err != nil {
			t.Errorf("Wrapper %s missing after double install: %v", name, err)
			continue
		}
		// Should match embedded content
		embedded, _ := scriptsFS.ReadFile("scripts/" + name)
		if string(content) != string(embedded) {
			t.Errorf("Wrapper %s content doesn't match embedded script after double install", name)
		}
	}
}

func TestEmbeddedScripts_NounsetSafeGasTownEnv(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is required for nounset test: %v", err)
	}

	tests := []struct {
		name      string
		env       []string
		wantCalls []string
	}{
		{name: "unset", wantCalls: []string{"agent"}},
		{name: "enabled", env: []string{"GASTOWN_ENABLED=1"}, wantCalls: []string{"gt prime", "agent"}},
		{name: "disabled", env: []string{"GASTOWN_DISABLED=1"}, wantCalls: []string{"agent"}},
		{
			name:      "disabled takes precedence",
			env:       []string{"GASTOWN_DISABLED=1", "GASTOWN_ENABLED=1"},
			wantCalls: []string{"agent"},
		},
	}

	for _, name := range expectedWrappers {
		t.Run(name, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					tmpHome := t.TempDir()
					binDir := filepath.Join(tmpHome, "bin")
					if err := os.MkdirAll(binDir, 0755); err != nil {
						t.Fatalf("creating bin directory: %v", err)
					}

					wrapperPath := filepath.Join(binDir, name)
					content, err := scriptsFS.ReadFile("scripts/" + name)
					if err != nil {
						t.Fatalf("reading embedded %s: %v", name, err)
					}
					if err := os.WriteFile(wrapperPath, content, 0755); err != nil {
						t.Fatalf("writing wrapper: %v", err)
					}

					callLog := filepath.Join(tmpHome, "calls")
					target := strings.TrimPrefix(name, "gt-")
					agentScript := "#!/bin/bash\nprintf '%s\\n' agent >> \"$GASTOWN_TEST_LOG\"\n"
					if err := os.WriteFile(filepath.Join(binDir, target), []byte(agentScript), 0755); err != nil {
						t.Fatalf("writing %s stub: %v", target, err)
					}
					gtScript := "#!/bin/bash\nprintf '%s\\n' \"gt $*\" >> \"$GASTOWN_TEST_LOG\"\n"
					if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
						t.Fatalf("writing gt stub: %v", err)
					}

					cmd := exec.Command(bashPath, "-u", wrapperPath, "argument")
					cmd.Dir = tmpHome
					cmd.Env = append(envWithout("GASTOWN_DISABLED", "GASTOWN_ENABLED", "GASTOWN_TEST_LOG", "HOME", "PATH"),
						append([]string{
							"HOME=" + tmpHome,
							"PATH=" + binDir,
							"GASTOWN_TEST_LOG=" + callLog,
						}, tt.env...)...)

					if output, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("bash -u %s failed: %v\nOutput: %s", name, err, output)
					}

					calls, err := os.ReadFile(callLog)
					if err != nil {
						t.Fatalf("reading call log: %v", err)
					}
					gotCalls := strings.Split(strings.TrimSpace(string(calls)), "\n")
					if !reflect.DeepEqual(gotCalls, tt.wantCalls) {
						t.Errorf("calls = %v, want %v", gotCalls, tt.wantCalls)
					}
				})
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
