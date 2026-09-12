package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitpkg "github.com/steveyegge/gastown/internal/git"
)

// fakeRebaseGit lets us drive autoRebaseOnTarget without a real git repo for
// the gating-decision tests.
type fakeRebaseGit struct {
	rebaseErr   error
	rebaseCalls int
	abortCalls  int
}

func (f *fakeRebaseGit) Rebase(onto string) error {
	f.rebaseCalls++
	return f.rebaseErr
}

func (f *fakeRebaseGit) AbortRebase() error {
	f.abortCalls++
	return nil
}

// TestAutoRebaseOnTarget_GatingDecisions verifies the skip/rebase decision
// matrix (gh#3400). The behavior under test is the *decision*, not the actual
// git mechanics — those are exercised separately below against a real repo.
func TestAutoRebaseOnTarget_GatingDecisions(t *testing.T) {
	tests := []struct {
		name          string
		behind        int
		preVerified   bool
		alreadyPushed bool
		wantRebased   bool
		wantSkip      string
		wantCalls     int
	}{
		{
			name:        "not behind: no-op",
			behind:      0,
			wantRebased: false,
			wantSkip:    "",
			wantCalls:   0,
		},
		{
			name:        "behind by 1: rebase runs",
			behind:      1,
			wantRebased: true,
			wantSkip:    "",
			wantCalls:   1,
		},
		{
			name:        "behind by 5: rebase runs",
			behind:      5,
			wantRebased: true,
			wantSkip:    "",
			wantCalls:   1,
		},
		{
			name:        "pre-verified: skip even when behind",
			behind:      3,
			preVerified: true,
			wantRebased: false,
			wantSkip:    "--pre-verified is set",
			wantCalls:   0,
		},
		{
			name:          "already pushed: skip to avoid divergence",
			behind:        3,
			alreadyPushed: true,
			wantRebased:   false,
			wantSkip:      "prior push checkpoint exists",
			wantCalls:     0,
		},
		{
			name:          "pre-verified takes precedence over already-pushed",
			behind:        3,
			preVerified:   true,
			alreadyPushed: true,
			wantRebased:   false,
			wantSkip:      "--pre-verified is set",
			wantCalls:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRebaseGit{}
			rebased, skipReason, err := autoRebaseOnTarget(fake, "origin/main", tt.behind, tt.preVerified, tt.alreadyPushed)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rebased != tt.wantRebased {
				t.Errorf("rebased = %v, want %v", rebased, tt.wantRebased)
			}
			if skipReason != tt.wantSkip {
				t.Errorf("skipReason = %q, want %q", skipReason, tt.wantSkip)
			}
			if fake.rebaseCalls != tt.wantCalls {
				t.Errorf("rebase calls = %d, want %d", fake.rebaseCalls, tt.wantCalls)
			}
			if fake.abortCalls != 0 {
				t.Errorf("abort calls = %d on success path, want 0", fake.abortCalls)
			}
		})
	}
}

func TestResolveDoneDefaultWorkBase_SplitAuthorityFailsBeforeMutation(t *testing.T) {
	tmp := t.TempDir()
	upstream := filepath.Join(tmp, "upstream.git")
	private := filepath.Join(tmp, "private.git")
	repo := filepath.Join(tmp, "polecat")
	testRunGit(t, tmp, "init", "--bare", upstream)
	testRunGit(t, tmp, "init", "--bare", private)
	testRunGit(t, tmp, "init", "--initial-branch", "main", repo)
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")
	writeRepoFile(t, repo, "README.md", "# done authority\n")
	testRunGit(t, repo, "add", "README.md")
	testRunGit(t, repo, "commit", "-m", "initial")
	testRunGit(t, repo, "remote", "add", "origin", upstream)
	testRunGit(t, repo, "push", "origin", "main")
	testRunGit(t, repo, "remote", "set-url", "origin", "--push", private)

	g := gitpkg.NewGit(repo)
	beforeBranch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch before: %v", err)
	}
	beforeHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("HEAD before: %v", err)
	}
	if _, err := resolveDoneDefaultWorkBase(g, "main", ""); !errors.Is(err, gitpkg.ErrMissingPrivateWorkAuthority) {
		t.Fatalf("resolveDoneDefaultWorkBase error = %v, want ErrMissingPrivateWorkAuthority", err)
	}
	afterBranch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch after: %v", err)
	}
	afterHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("HEAD after: %v", err)
	}
	if afterBranch != beforeBranch || afterHead != beforeHead {
		t.Fatalf("authority failure mutated done worktree: before %s/%s after %s/%s", beforeBranch, beforeHead, afterBranch, afterHead)
	}

	if authority, err := resolveDoneDefaultWorkBase(g, "main", "feature"); err != nil || authority != nil {
		t.Fatalf("explicit target should bypass default authority: authority=%v err=%v", authority, err)
	}
}

func TestDoneContaminationRefresh_ForkTipRaceFailsClosed(t *testing.T) {
	tmp := t.TempDir()
	upstream := filepath.Join(tmp, "upstream.git")
	private := filepath.Join(tmp, "private.git")
	repo := filepath.Join(tmp, "polecat")
	testRunGit(t, tmp, "init", "--bare", upstream)
	testRunGit(t, tmp, "init", "--bare", private)
	testRunGit(t, tmp, "init", "--initial-branch", "main", repo)
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")
	writeRepoFile(t, repo, "README.md", "# done race\n")
	testRunGit(t, repo, "add", "README.md")
	testRunGit(t, repo, "commit", "-m", "initial")
	testRunGit(t, repo, "remote", "add", "origin", upstream)
	testRunGit(t, repo, "push", "origin", "main")
	testRunGit(t, repo, "push", private, "main")
	testRunGit(t, repo, "remote", "set-url", "origin", "--push", private)
	testRunGit(t, repo, "remote", "add", "fork", private)
	testRunGit(t, private, "symbolic-ref", "HEAD", "refs/heads/main")

	g := gitpkg.NewGit(repo)
	authority, err := resolveDoneDefaultWorkBase(g, "main", "")
	if err != nil {
		t.Fatalf("resolve default work base: %v", err)
	}
	if authority == nil || authority.Ref != "fork/main" {
		t.Fatalf("authority = %+v, want fork/main", authority)
	}
	trackingRef := "refs/remotes/fork/main"
	trackingTip, err := g.Rev(trackingRef)
	if err != nil {
		t.Fatalf("tracking tip before race: %v", err)
	}
	trackingBytes := doneGuardGitOutput(t, repo, "show-ref", "--verify", trackingRef)
	beforeRefs := doneGuardGitOutput(t, repo, "show-ref")
	beforeWorktrees := doneGuardGitOutput(t, repo, "worktree", "list", "--porcelain")
	beforeBranch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("branch before race: %v", err)
	}
	beforeHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("HEAD before race: %v", err)
	}
	provenTip := authority.Tip

	seed := filepath.Join(tmp, "race-seed")
	testRunGit(t, tmp, "clone", private, seed)
	testRunGit(t, seed, "config", "user.email", "test@test.com")
	testRunGit(t, seed, "config", "user.name", "Test")
	writeRepoFile(t, seed, "race.txt", "advanced\n")
	testRunGit(t, seed, "add", "race.txt")
	testRunGit(t, seed, "commit", "-m", "advance private main")
	testRunGit(t, seed, "push", "origin", "main")
	newForkTip, err := g.RemoteBranchTip("fork", "main")
	if err != nil {
		t.Fatalf("fork tip after race: %v", err)
	}
	if newForkTip == provenTip {
		t.Fatalf("fork tip did not advance: still %s", provenTip)
	}

	strictRefresh, err := refreshDoneContaminationBase(g, authority, authority.Ref, authority.Remote)
	if !strictRefresh {
		t.Fatal("default authority should use strict contamination refresh")
	}
	if !errors.Is(err, gitpkg.ErrAmbiguousWorkAuthority) {
		t.Fatalf("post-proof refresh error = %v, want ErrAmbiguousWorkAuthority", err)
	}
	if authority.Tip != provenTip {
		t.Fatalf("authority tip changed after rejected race: got %s want %s", authority.Tip, provenTip)
	}
	afterTrackingTip, err := g.Rev(trackingRef)
	if err != nil {
		t.Fatalf("tracking tip after race: %v", err)
	}
	if afterTrackingTip != trackingTip {
		t.Fatalf("tracking tip changed after rejected post-proof race: got %s want %s", afterTrackingTip, trackingTip)
	}
	if afterTrackingBytes := doneGuardGitOutput(t, repo, "show-ref", "--verify", trackingRef); afterTrackingBytes != trackingBytes {
		t.Fatalf("tracking ref bytes changed after rejected post-proof race: got %q want %q", afterTrackingBytes, trackingBytes)
	}
	if afterRefs := doneGuardGitOutput(t, repo, "show-ref"); afterRefs != beforeRefs {
		t.Fatalf("refs changed after rejected post-proof race: before=%q after=%q", beforeRefs, afterRefs)
	}
	if afterWorktrees := doneGuardGitOutput(t, repo, "worktree", "list", "--porcelain"); afterWorktrees != beforeWorktrees {
		t.Fatalf("worktree registrations changed after rejected post-proof race")
	}
	afterBranch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("branch after race: %v", err)
	}
	afterHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("HEAD after race: %v", err)
	}
	if afterBranch != beforeBranch || afterHead != beforeHead {
		t.Fatalf("worktree mutated after rejected post-proof race: before %s/%s after %s/%s", beforeBranch, beforeHead, afterBranch, afterHead)
	}
	for _, artifact := range []string{
		filepath.Join(tmp, ".runtime"),
		filepath.Join(tmp, "polecats"),
		filepath.Join(tmp, "namepool"),
		filepath.Join(repo, ".git", "rebase-merge"),
		filepath.Join(repo, ".git", "rebase-apply"),
	} {
		if _, statErr := os.Stat(artifact); !os.IsNotExist(statErr) {
			t.Fatalf("unexpected artifact after rejected post-proof race %s: %v", artifact, statErr)
		}
	}
}

// TestAutoRebaseOnTarget_ConflictAborts verifies that a rebase failure causes
// AbortRebase to fire and the returned error includes remediation guidance.
func TestAutoRebaseOnTarget_ConflictAborts(t *testing.T) {
	fake := &fakeRebaseGit{rebaseErr: errors.New("CONFLICT (content): merge conflict in foo.txt")}

	rebased, skipReason, err := autoRebaseOnTarget(fake, "origin/main", 1, false, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if rebased {
		t.Error("rebased should be false on conflict")
	}
	if skipReason != "" {
		t.Errorf("skipReason should be empty on conflict, got %q", skipReason)
	}
	if fake.rebaseCalls != 1 {
		t.Errorf("expected 1 rebase call, got %d", fake.rebaseCalls)
	}
	if fake.abortCalls != 1 {
		t.Errorf("expected AbortRebase to fire on conflict, got %d calls", fake.abortCalls)
	}
	// Remediation guidance is part of the contract — agents read this message.
	msg := err.Error()
	if !strings.Contains(msg, "auto-rebase onto origin/main failed") {
		t.Errorf("error missing context: %q", msg)
	}
	if !strings.Contains(msg, "git rebase origin/main") {
		t.Errorf("error missing remediation hint: %q", msg)
	}
	if !strings.Contains(msg, "rerun gt done") {
		t.Errorf("error missing rerun hint: %q", msg)
	}
}

// TestAutoRebaseOnTarget_RealRepoSuccess exercises the rebase against a real
// git working tree to confirm the wiring (Rebase call) actually replays the
// branch onto a moved base. (gh#3400, scenario (a) from the bead notes.)
func TestAutoRebaseOnTarget_RealRepoSuccess(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	testRunGit(t, tmp, "init", "--initial-branch", "main", repo)
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")

	// Initial commit on main.
	writeRepoFile(t, repo, "README.md", "# initial\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "initial")

	// Branch off and add a polecat commit on a non-conflicting file.
	testRunGit(t, repo, "checkout", "-b", "feature")
	writeRepoFile(t, repo, "feature.txt", "feature work\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "feature")

	// Move main forward independently — also non-conflicting with feature.
	testRunGit(t, repo, "checkout", "main")
	writeRepoFile(t, repo, "main-new.txt", "new on main\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "advance main")

	testRunGit(t, repo, "checkout", "feature")

	g := gitpkg.NewGit(repo)
	rebased, skipReason, err := autoRebaseOnTarget(g, "main", 1, false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rebased {
		t.Fatalf("expected rebased=true, got false (skip=%q)", skipReason)
	}

	// After rebase, both files must be present and the feature commit must sit
	// on top of the advance-main commit.
	if _, statErr := os.Stat(filepath.Join(repo, "main-new.txt")); statErr != nil {
		t.Errorf("main-new.txt missing after rebase: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(repo, "feature.txt")); statErr != nil {
		t.Errorf("feature.txt missing after rebase: %v", statErr)
	}
}

// TestAutoRebaseOnTarget_RealRepoConflictAborts exercises the conflict path
// against a real git working tree: feature and main both touch the same file,
// rebase fails with a CONFLICT, and AbortRebase must restore the working tree
// so the polecat can address the conflict manually. (gh#3400, scenario (b).)
func TestAutoRebaseOnTarget_RealRepoConflictAborts(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	testRunGit(t, tmp, "init", "--initial-branch", "main", repo)
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")

	writeRepoFile(t, repo, "shared.txt", "v0\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "initial")

	// Feature changes shared.txt to v1.
	testRunGit(t, repo, "checkout", "-b", "feature")
	writeRepoFile(t, repo, "shared.txt", "v1-feature\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "feature edit")

	// Main changes shared.txt to a different value — guaranteed conflict.
	testRunGit(t, repo, "checkout", "main")
	writeRepoFile(t, repo, "shared.txt", "v1-main\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "main edit")

	testRunGit(t, repo, "checkout", "feature")

	g := gitpkg.NewGit(repo)
	rebased, skipReason, err := autoRebaseOnTarget(g, "main", 1, false, false)
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if rebased {
		t.Error("rebased should be false on conflict")
	}
	if skipReason != "" {
		t.Errorf("skipReason should be empty on conflict, got %q", skipReason)
	}

	// AbortRebase is required to leave the working tree in a clean state. After
	// abort, the rebase-merge dir must be gone — otherwise the polecat is stuck
	// in a half-rebased state.
	if _, statErr := os.Stat(filepath.Join(repo, ".git", "rebase-merge")); !os.IsNotExist(statErr) {
		t.Errorf(".git/rebase-merge should not exist after abort (stat err: %v)", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(repo, ".git", "rebase-apply")); !os.IsNotExist(statErr) {
		t.Errorf(".git/rebase-apply should not exist after abort (stat err: %v)", statErr)
	}
}

func writeRepoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
