package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	gitpkg "github.com/steveyegge/gastown/internal/git"
	rigpkg "github.com/steveyegge/gastown/internal/rig"
)

func TestResolveMQSubmitCommitSHAUsesSubmittedBranch(t *testing.T) {
	repo := t.TempDir()
	runGitForMQSubmitTest(t, repo, "init")
	runGitForMQSubmitTest(t, repo, "config", "user.email", "test@example.com")
	runGitForMQSubmitTest(t, repo, "config", "user.name", "Test User")

	writeMQSubmitTestFile(t, repo, "file.txt", "main\n")
	runGitForMQSubmitTest(t, repo, "add", "file.txt")
	runGitForMQSubmitTest(t, repo, "commit", "-m", "main")
	runGitForMQSubmitTest(t, repo, "branch", "-M", "main")
	mainSHA := runGitForMQSubmitTest(t, repo, "rev-parse", "HEAD")

	runGitForMQSubmitTest(t, repo, "checkout", "-b", "feature/pr-target")
	writeMQSubmitTestFile(t, repo, "file.txt", "feature\n")
	runGitForMQSubmitTest(t, repo, "commit", "-am", "feature")
	featureSHA := runGitForMQSubmitTest(t, repo, "rev-parse", "HEAD")
	runGitForMQSubmitTest(t, repo, "tag", "feature/pr-target", mainSHA)

	runGitForMQSubmitTest(t, repo, "checkout", "main")
	g := gitpkg.NewGit(repo)
	got, err := resolveMQSubmitCommitSHA(g, "feature/pr-target")
	if err != nil {
		t.Fatalf("resolveMQSubmitCommitSHA: %v", err)
	}
	if got != featureSHA {
		t.Fatalf("resolveMQSubmitCommitSHA() = %s, want submitted branch tip %s", got, featureSHA)
	}
	if got == mainSHA {
		t.Fatalf("resolveMQSubmitCommitSHA() used HEAD %s instead of submitted branch tip", mainSHA)
	}
}

func TestVerifyMQSubmitPushedBranchRequiresRemoteBranch(t *testing.T) {
	repo := t.TempDir()
	remote := t.TempDir()
	runGitForMQSubmitTest(t, remote, "init", "--bare")

	runGitForMQSubmitTest(t, repo, "init")
	runGitForMQSubmitTest(t, repo, "config", "user.email", "test@example.com")
	runGitForMQSubmitTest(t, repo, "config", "user.name", "Test User")
	runGitForMQSubmitTest(t, repo, "remote", "add", "origin", remote)

	writeMQSubmitTestFile(t, repo, "file.txt", "main\n")
	runGitForMQSubmitTest(t, repo, "add", "file.txt")
	runGitForMQSubmitTest(t, repo, "commit", "-m", "main")
	runGitForMQSubmitTest(t, repo, "branch", "-M", "main")
	runGitForMQSubmitTest(t, repo, "push", "-u", "origin", "main")

	runGitForMQSubmitTest(t, repo, "checkout", "-b", "feature/pr-target")
	writeMQSubmitTestFile(t, repo, "file.txt", "feature\n")
	runGitForMQSubmitTest(t, repo, "commit", "-am", "feature")
	featureSHA := runGitForMQSubmitTest(t, repo, "rev-parse", "HEAD")

	g := gitpkg.NewGit(repo)
	err := verifyMQSubmitPushedBranch(g, "feature/pr-target", featureSHA)
	if err == nil {
		t.Fatal("verifyMQSubmitPushedBranch() = nil, want missing remote branch error")
	}
	for _, want := range []string{"git push origin feature/pr-target", "gt done"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("verifyMQSubmitPushedBranch() error missing %q: %v", want, err)
		}
	}

	runGitForMQSubmitTest(t, repo, "push", "origin", "feature/pr-target")
	if err := verifyMQSubmitPushedBranch(g, "feature/pr-target", featureSHA); err != nil {
		t.Fatalf("verifyMQSubmitPushedBranch() after push: %v", err)
	}
}

func TestRegisteredMQSubmitPushTargetPrefersRegisteredPushURL(t *testing.T) {
	ctx := &mqSubmitRigContext{
		RigName: "gastown",
		Registry: config.RigEntry{
			GitURL:  "https://github.com/example/gastown.git",
			PushURL: "ssh://git@github.com/example/private-gastown.git",
		},
	}

	got, err := registeredMQSubmitPushTarget(ctx)
	if err != nil {
		t.Fatalf("registeredMQSubmitPushTarget() error = %v", err)
	}
	if got != ctx.Registry.PushURL {
		t.Fatalf("registeredMQSubmitPushTarget() = %q, want registered PushURL %q", got, ctx.Registry.PushURL)
	}
}

func TestRegisteredMQSubmitPushTargetFallsBackToRegisteredGitURL(t *testing.T) {
	ctx := &mqSubmitRigContext{
		RigName:  "gastown",
		Registry: config.RigEntry{GitURL: "https://github.com/example/gastown.git"},
	}

	got, err := registeredMQSubmitPushTarget(ctx)
	if err != nil {
		t.Fatalf("registeredMQSubmitPushTarget() error = %v", err)
	}
	if got != ctx.Registry.GitURL {
		t.Fatalf("registeredMQSubmitPushTarget() = %q, want registered GitURL %q", got, ctx.Registry.GitURL)
	}
}

func TestRegisteredMQSubmitPushTargetNeverUsesRigLocalFallback(t *testing.T) {
	rigPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(`{"push_url":"https://local.invalid/not-authoritative.git"}
`), 0o644); err != nil {
		t.Fatalf("write rig config: %v", err)
	}
	ctx := &mqSubmitRigContext{
		RigName: "gastown",
		Rig:     &rigpkg.Rig{Path: rigPath},
		Registry: config.RigEntry{
			GitURL: "https://github.com/example/gastown.git",
		},
	}

	got, err := registeredMQSubmitPushTarget(ctx)
	if err != nil {
		t.Fatalf("registeredMQSubmitPushTarget() error = %v", err)
	}
	if got != ctx.Registry.GitURL {
		t.Fatalf("registeredMQSubmitPushTarget() = %q, want registered GitURL %q", got, ctx.Registry.GitURL)
	}
}

func TestNormalizeMQSubmitRemoteURLPreservesUnprovenEndpointDifferences(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
	}{
		{
			name:  "transport mismatch",
			left:  "https://github.com/example/gastown.git",
			right: "ssh://git@github.com/example/gastown.git",
		},
		{
			name:  "host mismatch",
			left:  "https://github.com/example/gastown.git",
			right: "https://gitlab.com/example/gastown.git",
		},
		{
			name:  "path mismatch",
			left:  "https://github.com/example/gastown.git",
			right: "https://github.com/other/gastown.git",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if left, right := normalizeMQSubmitRemoteURL(tt.left), normalizeMQSubmitRemoteURL(tt.right); left == right {
				t.Fatalf("normalizeMQSubmitRemoteURL() collapsed %s: %q and %q", tt.name, left, right)
			}
		})
	}
}

func TestNormalizeMQSubmitRemoteURLAcceptsOnlyEquivalentEndpointSyntax(t *testing.T) {
	equivalent := [][2]string{
		{"https://GITHUB.com/example/gastown.git", "https://github.com/example/gastown/"},
		{"git@github.com:example/gastown.git", "ssh://git@github.com/example/gastown/"},
	}
	for _, pair := range equivalent {
		left, right := normalizeMQSubmitRemoteURL(pair[0]), normalizeMQSubmitRemoteURL(pair[1])
		if left != right {
			t.Errorf("normalizeMQSubmitRemoteURL(%q, %q) = %q, %q; want equivalent", pair[0], pair[1], left, right)
		}
	}
}

func TestResolveMQSubmitRigRejectsExternalCwdWithoutExplicitRig(t *testing.T) {
	townRoot := t.TempDir()
	outside := t.TempDir()
	writeMQSubmitTownRegistry(t, townRoot, map[string]config.RigEntry{
		"gastown": {GitURL: "git@example.invalid/gastown.git", PushURL: "git@example.invalid/private/gastown.git"},
	})
	if _, err := resolveMQSubmitRig(townRoot, outside, ""); err == nil || !strings.Contains(err.Error(), "--rig") {
		t.Fatalf("resolveMQSubmitRig() error = %v, want explicit --rig rejection", err)
	}
	if _, err := resolveMQSubmitRig(townRoot, outside, "unknown"); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("resolveMQSubmitRig(unknown) error = %v, want unknown-rig rejection", err)
	}
}

func TestResolveMQSubmitRigUsesRegisteredOwnerForExternalSource(t *testing.T) {
	townRoot := t.TempDir()
	outside := t.TempDir()
	owner := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(owner, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMQSubmitTownRegistry(t, townRoot, map[string]config.RigEntry{
		"gastown": {GitURL: "git@example.invalid/gastown.git", PushURL: "git@example.invalid/private/gastown.git"},
	})

	resolved, err := resolveMQSubmitRig(townRoot, outside, "gastown")
	if err != nil {
		t.Fatalf("resolveMQSubmitRig() error = %v", err)
	}
	if resolved.RigName != "gastown" || resolved.BeadsWorkDir != owner || !resolved.External {
		t.Fatalf("resolved = %+v, want registered owner rig and external source", resolved)
	}
}

func TestValidateMQSubmitTargetRejectsMalformedAndEqualSource(t *testing.T) {
	for _, target := range []string{"", "bad..target", "bad target", "bad~target", "bad^target", "bad:target", "bad[target"} {
		if err := validateMQSubmitTargetName(target); err == nil {
			t.Errorf("validateMQSubmitTargetName(%q) = nil, want malformed-target error", target)
		}
	}
	if err := validateMQSubmitSourceTarget("polecat/refuge/gt-source", "polecat/refuge/gt-source"); err == nil {
		t.Fatal("validateMQSubmitSourceTarget() = nil, want source/target equality error")
	}
}

func TestValidateMQSubmitTargetRejectsMissingRemoteBranch(t *testing.T) {
	repo := t.TempDir()
	remote := t.TempDir()
	runGitForMQSubmitTest(t, remote, "init", "--bare")
	runGitForMQSubmitTest(t, repo, "init")
	runGitForMQSubmitTest(t, repo, "config", "user.email", "test@example.com")
	runGitForMQSubmitTest(t, repo, "config", "user.name", "Test User")
	runGitForMQSubmitTest(t, repo, "remote", "add", "origin", remote)
	writeMQSubmitTestFile(t, repo, "file.txt", "main\n")
	runGitForMQSubmitTest(t, repo, "add", "file.txt")
	runGitForMQSubmitTest(t, repo, "commit", "-m", "main")
	runGitForMQSubmitTest(t, repo, "branch", "-M", "main")
	runGitForMQSubmitTest(t, repo, "push", "-u", "origin", "main")

	ownerGit := gitpkg.NewGit(repo)
	if err := validateMQSubmitTarget(ownerGit, "main"); err != nil {
		t.Fatalf("validateMQSubmitTarget(main) = %v, want nil", err)
	}
	if err := validateMQSubmitTarget(ownerGit, "missing-target"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("validateMQSubmitTarget(missing-target) = %v, want missing-target error", err)
	}
}

func TestValidateMQSubmitExistingMRRequiresExactRoutingDedup(t *testing.T) {
	mr := &beads.Issue{
		ID: "gt-mr",
		Description: "branch: polecat/refuge/gt-source\n" +
			"target: main\nsource_issue: gt-source\nrig: gastown\ncommit_sha: abc123\n",
	}
	if err := validateMQSubmitExistingMR(mr, "polecat/refuge/gt-source", "main", "gastown", "abc123"); err != nil {
		t.Fatalf("validateMQSubmitExistingMR exact = %v, want nil", err)
	}
	for name, target := range map[string]string{"wrong target": "gte-00-rob", "missing target": ""} {
		t.Run(name, func(t *testing.T) {
			if err := validateMQSubmitExistingMR(mr, "polecat/refuge/gt-source", target, "gastown", "abc123"); err == nil {
				t.Fatalf("validateMQSubmitExistingMR target %q = nil, want exact-dedup rejection", target)
			}
		})
	}
}

func TestGtE47ExternalWorktreeResolutionUsesRegisteredRig(t *testing.T) {
	assertExternalMQSubmitOwner(t, "gt-e47", "gastown")
}

func TestPpo853ExternalWorktreeResolutionUsesOwnerRigBeads(t *testing.T) {
	assertExternalMQSubmitOwner(t, "ppo-853", "pierpoint_ops")
}

func assertExternalMQSubmitOwner(t *testing.T, issueID, rigName string) {
	t.Helper()
	townRoot := t.TempDir()
	outside := t.TempDir()
	owner := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(owner, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMQSubmitTownRegistry(t, townRoot, map[string]config.RigEntry{
		rigName: {GitURL: "git@example.invalid/" + rigName + ".git", PushURL: "git@example.invalid/private/" + rigName + ".git"},
	})
	resolved, err := resolveMQSubmitRig(townRoot, outside, rigName)
	if err != nil {
		t.Fatalf("%s resolveMQSubmitRig: %v", issueID, err)
	}
	if resolved.RigName != rigName || resolved.BeadsWorkDir != owner || !resolved.External {
		t.Fatalf("%s resolved = %+v, want owner rig %q and owner Beads path %q", issueID, resolved, rigName, owner)
	}
}

func TestPpo4jkExplicitMainTargetOverridesConfiguredWrongTarget(t *testing.T) {
	configuredDefault := "gte-00-rob"
	explicitTarget := "main"
	if got := mqSubmitExplicitTarget(explicitTarget, configuredDefault); got != "main" {
		t.Fatalf("mqSubmitExplicitTarget(%q, %q) = %q, want main", explicitTarget, configuredDefault, got)
	}
}

func writeMQSubmitTownRegistry(t *testing.T, townRoot string, entries map[string]config.RigEntry) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{\"name\":\"test-town\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"), &config.RigsConfig{
		Version: config.CurrentRigsVersion,
		Rigs:    entries,
	}); err != nil {
		t.Fatal(err)
	}
}

func runGitForMQSubmitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeMQSubmitTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidateMoleculePrereqs(t *testing.T) {
	tests := []struct {
		name      string
		children  []*beads.Issue
		wantErr   bool
		wantInErr []string // Substrings expected in error message
	}{
		{
			name:     "nil children",
			children: nil,
			wantErr:  false,
		},
		{
			name:     "empty children",
			children: []*beads.Issue{},
			wantErr:  false,
		},
		{
			name: "all prereqs closed",
			children: []*beads.Issue{
				{ID: "gt-mol.1", Title: "Load context", Status: "closed"},
				{ID: "gt-mol.2", Title: "Set up branch", Status: "closed"},
				{ID: "gt-mol.3", Title: "Implement", Status: "closed"},
				{ID: "gt-mol.4", Title: "Self-review", Status: "closed"},
				{ID: "gt-mol.5", Title: "Build check", Status: "closed"},
				{ID: "gt-mol.6", Title: "Commit changes", Status: "closed"},
				{ID: "gt-mol.7", Title: "Rebase verify", Status: "closed"},
				{ID: "gt-mol.8", Title: "Submit MR", Status: "open"},
				{ID: "gt-mol.9", Title: "Wait for verdict", Status: "open"},
				{ID: "gt-mol.10", Title: "Self-clean", Status: "open"},
			},
			wantErr: false,
		},
		{
			name: "missing self-review step",
			children: []*beads.Issue{
				{ID: "gt-mol.1", Title: "Load context", Status: "closed"},
				{ID: "gt-mol.2", Title: "Set up branch", Status: "closed"},
				{ID: "gt-mol.3", Title: "Implement", Status: "closed"},
				{ID: "gt-mol.4", Title: "Self-review", Status: "open"},
				{ID: "gt-mol.5", Title: "Build check", Status: "closed"},
				{ID: "gt-mol.6", Title: "Commit changes", Status: "closed"},
				{ID: "gt-mol.7", Title: "Rebase verify", Status: "closed"},
				{ID: "gt-mol.8", Title: "Submit MR", Status: "open"},
			},
			wantErr:   true,
			wantInErr: []string{"gt-mol.4", "Self-review", "--skip-deps"},
		},
		{
			name: "multiple incomplete steps",
			children: []*beads.Issue{
				{ID: "gt-mol.1", Title: "Load context", Status: "closed"},
				{ID: "gt-mol.2", Title: "Set up branch", Status: "open"},
				{ID: "gt-mol.3", Title: "Implement", Status: "in_progress"},
				{ID: "gt-mol.4", Title: "Self-review", Status: "open"},
				{ID: "gt-mol.5", Title: "Submit MR", Status: "open"},
			},
			wantErr:   true,
			wantInErr: []string{"gt-mol.2", "gt-mol.3", "gt-mol.4"},
		},
		{
			name: "no submit step found — checks all steps",
			children: []*beads.Issue{
				{ID: "gt-mol.1", Title: "Load context", Status: "closed"},
				{ID: "gt-mol.2", Title: "Implement", Status: "open"},
				{ID: "gt-mol.3", Title: "Build check", Status: "open"},
			},
			wantErr:   true,
			wantInErr: []string{"gt-mol.2", "gt-mol.3"},
		},
		{
			name: "post-submit steps open is OK",
			children: []*beads.Issue{
				{ID: "gt-mol.1", Title: "Load context", Status: "closed"},
				{ID: "gt-mol.2", Title: "Submit MR", Status: "open"},
				{ID: "gt-mol.3", Title: "Wait for verdict", Status: "open"},
			},
			wantErr: false,
		},
		{
			name: "case insensitive submit detection",
			children: []*beads.Issue{
				{ID: "gt-mol.1", Title: "Implement", Status: "closed"},
				{ID: "gt-mol.2", Title: "SUBMIT MR and enter awaiting_verdict", Status: "open"},
				{ID: "gt-mol.3", Title: "Self-clean", Status: "open"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMoleculePrereqs(tt.children)
			if tt.wantErr && err == nil {
				t.Errorf("validateMoleculePrereqs() = nil, want error")
				return
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateMoleculePrereqs() = %v, want nil", err)
				return
			}
			if err != nil {
				errMsg := err.Error()
				for _, want := range tt.wantInErr {
					if !strings.Contains(errMsg, want) {
						t.Errorf("error message missing %q, got: %s", want, errMsg)
					}
				}
			}
		})
	}
}
