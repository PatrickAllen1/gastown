// Package cmd provides polecat spawning utilities for gt sling.
package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/witness"
	"github.com/steveyegge/gastown/internal/workspace"
)

const minPolecatDirsPerRig = 30

// SpawnedPolecatInfo contains info about a spawned polecat session.
type SpawnedPolecatInfo struct {
	RigName           string // Rig name (e.g., "gastown")
	PolecatName       string // Polecat name (e.g., "Toast")
	ClonePath         string // Path to polecat's git worktree
	SessionName       string // Tmux session name (e.g., "gt-gastown-p-Toast")
	Pane              string // Tmux pane ID (empty until StartSession is called)
	HookSetAtomically bool   // HookBead was persisted while creating the identity
	BaseBranch        string // Effective base branch (e.g., "main", "integration/epic-id")
	Branch            string // Git branch name (for cleanup on rollback)

	// Internal fields for deferred session start
	account string
	agent   string
}

// AgentID returns the agent identifier (e.g., "gastown/polecats/Toast")
func (s *SpawnedPolecatInfo) AgentID() string {
	return fmt.Sprintf("%s/polecats/%s", s.RigName, s.PolecatName)
}

// SessionStarted returns true if the tmux session has been started.
func (s *SpawnedPolecatInfo) SessionStarted() bool {
	return s.Pane != ""
}

// SlingSpawnOptions contains options for spawning a polecat via sling.
type SlingSpawnOptions struct {
	TownRoot      string // Gas Town workspace root; falls back to cwd when empty
	Force         bool   // Force spawn even if polecat has uncommitted work
	Account       string // Claude Code account handle to use
	Create        bool   // Create polecat if it doesn't exist (currently always true for sling)
	HookBead      string // Bead ID to set as hook_bead at spawn time (atomic assignment)
	Agent         string // Agent override for this spawn (e.g., "gemini", "codex", "claude-haiku")
	PolecatName   string // Explicit polecat name to create or revive (empty = allocate/reuse)
	BaseBranch    string // Override base branch for polecat worktree (e.g., "develop", "release/v2")
	ResumeBranch  string // Resume an existing branch (e.g. PR head) instead of creating polecat/<name>/<bead>+<ts>
	SkipAdmission bool   // Caller already holds a polecat admission reservation
}

func effectivePolecatDirCap(configured int) int {
	if configured < minPolecatDirsPerRig {
		return minPolecatDirsPerRig
	}
	return configured
}

func reclaimBrokenIdlePolecatForSling(polecatMgr *polecat.Manager) (bool, error) {
	polecats, err := polecatMgr.List()
	if err != nil {
		return false, err
	}

	for _, candidate := range polecats {
		if candidate == nil || candidate.State != polecat.StateIdle || candidate.Issue != "" {
			continue
		}
		verifyErr := verifyWorktreeExists(candidate.ClonePath)
		if verifyErr == nil || !polecat.IsStructuralWorktreeError(verifyErr) {
			continue
		}

		fmt.Printf("  Reclaiming broken idle polecat %s before allocation: %v\n", candidate.Name, verifyErr)
		if err := polecatMgr.ReclaimBrokenIdlePolecat(candidate.Name); err != nil {
			fmt.Printf("  Broken idle polecat %s was not safe to reclaim: %v\n", candidate.Name, err)
			continue
		}
		fmt.Printf("  %s Broken idle polecat %s reclaimed before assigning new work\n", style.Bold.Render("✓"), candidate.Name)
		return true, nil
	}

	return false, nil
}

// SpawnPolecatForSling creates a fresh polecat and optionally starts its session.
// This is used by gt sling when the target is a rig name.
// The caller (sling) handles hook attachment and nudging.
func SpawnPolecatForSling(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
	// Find workspace
	townRoot := opts.TownRoot
	if townRoot == "" {
		var err error
		townRoot, err = workspace.FindFromCwdOrError()
		if err != nil {
			return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
		}
	}

	// Load rig config
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	r, err := rigMgr.GetRig(rigName)
	if err != nil {
		return nil, fmt.Errorf("rig '%s' not found", rigName)
	}

	// Get polecat manager (with tmux for session-aware allocation)
	polecatGit := git.NewGit(r.Path)
	t := tmux.NewTmux()
	polecatMgr := polecat.NewManager(r, polecatGit, t)

	// Validate explicit runtime profiles before allocating a name or mutating
	// polecat state. A requested profile must never silently become the rig
	// default on either the initial spawn or a retry.
	if opts.Agent != "" {
		if _, _, err := config.ResolveAgentConfigWithOverride(townRoot, r.Path, opts.Agent); err != nil {
			return nil, fmt.Errorf("resolving agent config for %s: %w", opts.Agent, err)
		}
	}

	// Pre-spawn Dolt health check (gt-94llt7): verify Dolt is reachable before
	// allocating a polecat. Prevents orphaned polecats when Dolt is down.
	if err := polecatMgr.CheckDoltHealth(); err != nil {
		return nil, fmt.Errorf("pre-spawn health check failed: %w", err)
	}

	// Pre-spawn admission control (gt-1obzke): verify Dolt server has connection
	// capacity before spawning. Prevents connection storms during mass sling.
	if err := polecatMgr.CheckDoltServerCapacity(); err != nil {
		return nil, fmt.Errorf("admission control: %w", err)
	}

	if blocked, reason := IsRigParkedOrDocked(townRoot, rigName); blocked {
		undoCmd := "gt rig unpark"
		if reason == "docked" {
			undoCmd = "gt rig undock"
		}
		return nil, fmt.Errorf("cannot sling to %s rig %q\n%s %s", reason, rigName, undoCmd, rigName)
	}

	var admission *polecatAdmissionHandle
	if !opts.SkipAdmission {
		admission, _, err = acquirePolecatAdmissionFn(townRoot, rigName, opts.HookBead, "spawn-or-reuse")
		if err != nil {
			return nil, err
		}
		defer admission.Release()
	}

	// Per-bead respawn circuit breaker (clown show #22):
	// Track named and allocated respawns alike so an exact target cannot bypass
	// the safety limit while recovering a repeatedly failing startup.
	if opts.HookBead != "" && !opts.Force {
		if witness.ShouldBlockRespawn(townRoot, opts.HookBead) {
			maxRespawns := config.LoadOperationalConfig(townRoot).GetWitnessConfig().MaxBeadRespawnsV()
			return nil, fmt.Errorf("respawn limit reached for %s (%d attempts). "+
				"This bead keeps failing — investigate before re-dispatching.\n"+
				"Override: gt sling %s %s --force\n"+
				"Reset:    gt sling respawn-reset %s",
				opts.HookBead, maxRespawns,
				opts.HookBead, rigName, opts.HookBead)
		}
		witness.RecordBeadRespawn(townRoot, opts.HookBead)
	}

	// An explicit polecat target must remain that polecat across a stopped
	// session, a nuked identity, and a retry. Do this before idle-pool reuse so
	// the requested identity is never replaced with an unrelated allocation.
	if opts.PolecatName != "" {
		return spawnNamedPolecatForSling(rigName, r, polecatMgr, t, opts)
	}

	if reclaimed, err := reclaimBrokenIdlePolecatForSling(polecatMgr); err != nil {
		style.PrintWarning("could not reclaim broken idle polecat before allocation: %v", err)
	} else if reclaimed {
		fmt.Println("  Allocating fresh polecat after reclaiming broken idle sandbox...")
	}

	// Persistent polecat model (gt-4ac): try to reuse an idle polecat first.
	// Idle polecats have completed their work but kept their sandbox (worktree).
	// Reusing avoids the overhead of creating a new worktree.
	idlePolecat, findErr := polecatMgr.FindIdlePolecat()
	if findErr == nil && idlePolecat != nil {
		polecatName := idlePolecat.Name
		fmt.Printf("Reusing idle polecat: %s\n", polecatName)

		// ResumeBranch takes precedence over BaseBranch / integration auto-detection:
		// when the user (or scheduler) wants to resume an existing PR branch, we
		// must not start from main or an integration branch.
		baseBranch := opts.BaseBranch
		if opts.ResumeBranch == "" {
			if baseBranch == "" && opts.HookBead != "" {
				settingsPath := filepath.Join(r.Path, "settings", "config.json")
				polecatIntegrationEnabled := true
				if settings, err := config.LoadRigSettings(settingsPath); err == nil && settings.MergeQueue != nil {
					polecatIntegrationEnabled = settings.MergeQueue.IsPolecatIntegrationEnabled()
				}
				if polecatIntegrationEnabled {
					repoGit, repoErr := getRigGit(r.Path)
					if repoErr == nil {
						bd := beads.New(r.Path)
						detected, detectErr := beads.DetectIntegrationBranch(bd, repoGit, opts.HookBead)
						if detectErr == nil && detected != "" {
							baseBranch = "origin/" + detected
							fmt.Printf("  Auto-detected integration branch: %s\n", detected)
						}
					}
				}
			}
			if baseBranch != "" && !strings.HasPrefix(baseBranch, "origin/") {
				baseBranch = "origin/" + baseBranch
			}
		}

		// Reuse the idle polecat with branch-only operations (no worktree add/remove).
		// Phase 3 of persistent-polecat-pool: eliminates ~5s worktree creation overhead.
		// If reuse is unsafe or fails, allocate a new polecat instead of repairing
		// this worktree destructively.
		addOpts := polecat.AddOptions{
			HookBead:     opts.HookBead,
			AgentProfile: opts.Agent,
			BaseBranch:   baseBranch,
			ResumeBranch: opts.ResumeBranch,
		}
		reuseOK := false
		if _, err := polecatMgr.ReuseIdlePolecat(polecatName, addOpts); err != nil {
			if errors.Is(err, polecat.ErrPolecatNeedsRecovery) {
				fmt.Printf("  Idle polecat %s needs recovery before reuse: %v; allocating new...\n", polecatName, err)
			} else {
				fmt.Printf("  Branch-only reuse failed for idle polecat %s: %v; allocating new...\n", polecatName, err)
			}
		} else {
			reuseOK = true
		}

		if reuseOK {
			polecatObj, err := polecatMgr.Get(polecatName)
			if err != nil {
				return nil, fmt.Errorf("getting idle polecat after reuse: %w", err)
			}
			if err := verifyWorktreeExists(polecatObj.ClonePath); err != nil {
				return nil, fmt.Errorf("worktree verification failed for reused %s: %w", polecatName, err)
			}

			polecatSessMgr := polecat.NewSessionManager(t, r)
			sessionName := polecatSessMgr.SessionName(polecatName)

			fmt.Printf("%s Polecat %s reused (idle → working, session start deferred)\n", style.Bold.Render("✓"), polecatName)
			_ = events.LogFeed(events.TypeSpawn, "gt", events.SpawnPayload(rigName, polecatName))

			effectiveBranch := strings.TrimPrefix(baseBranch, "origin/")
			if effectiveBranch == "" {
				effectiveBranch = r.DefaultBranch()
			}
			if opts.ResumeBranch != "" {
				effectiveBranch = opts.ResumeBranch
			}

			return &SpawnedPolecatInfo{
				RigName:           rigName,
				PolecatName:       polecatName,
				ClonePath:         polecatObj.ClonePath,
				SessionName:       sessionName,
				Pane:              "",
				BaseBranch:        effectiveBranch,
				Branch:            polecatObj.Branch,
				HookSetAtomically: opts.HookBead != "",
				account:           opts.Account,
				agent:             opts.Agent,
			}, nil
		}
	}

	// Per-rig directory cap: prevent unbounded worktree accumulation, but only
	// after trying safe reuse. A reusable preserved polecat should not be blocked
	// just because the rig is already at the directory cap.
	maxPolecatDirsPerRig := effectivePolecatDirCap(r.GetIntConfig("max_polecats"))
	rigPolecatDir := filepath.Join(townRoot, rigName, "polecats")
	if entries, err := os.ReadDir(rigPolecatDir); err == nil {
		dirCount := 0
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				dirCount++
			}
		}
		if dirCount >= maxPolecatDirsPerRig {
			return nil, fmt.Errorf("rig %s has %d polecat directories (max %d). "+
				"Resolve recovery-needed polecats before allocating more slots: gt polecat list %s",
				rigName, dirCount, maxPolecatDirsPerRig, rigName)
		}
	}

	// Determine base branch for polecat worktree.
	// ResumeBranch (gh#3602) takes precedence: when resuming an existing branch
	// we must not start from main or auto-detect an integration branch.
	baseBranch := opts.BaseBranch
	if opts.ResumeBranch == "" {
		if baseBranch == "" && opts.HookBead != "" {
			// Auto-detect: check if the hooked bead's parent epic has an integration branch
			settingsPath := filepath.Join(r.Path, "settings", "config.json")
			polecatIntegrationEnabled := true
			if settings, err := config.LoadRigSettings(settingsPath); err == nil && settings.MergeQueue != nil {
				polecatIntegrationEnabled = settings.MergeQueue.IsPolecatIntegrationEnabled()
			}
			if polecatIntegrationEnabled {
				repoGit, repoErr := getRigGit(r.Path)
				if repoErr == nil {
					bd := beads.New(r.Path)
					detected, detectErr := beads.DetectIntegrationBranch(bd, repoGit, opts.HookBead)
					if detectErr == nil && detected != "" {
						baseBranch = "origin/" + detected
						fmt.Printf("  Auto-detected integration branch: %s\n", detected)
					}
				}
			}
		}
		if baseBranch != "" && !strings.HasPrefix(baseBranch, "origin/") {
			baseBranch = "origin/" + baseBranch
		}
	}

	// Build add options with hook_bead set atomically at spawn time
	addOpts := polecat.AddOptions{
		HookBead:     opts.HookBead,
		AgentProfile: opts.Agent,
		BaseBranch:   baseBranch,
		ResumeBranch: opts.ResumeBranch,
	}

	// No idle polecat available — allocate and create atomically (GH#2215).
	// AllocateAndAdd holds the pool lock through directory creation, preventing
	// concurrent processes from allocating the same name.
	polecatName, _, err := polecatMgr.AllocateAndAdd(addOpts)
	if err != nil {
		return nil, fmt.Errorf("allocating and creating polecat: %w", err)
	}
	fmt.Printf("Created polecat: %s\n", polecatName)

	// Get polecat object for path info
	polecatObj, err := polecatMgr.Get(polecatName)
	if err != nil {
		return nil, fmt.Errorf("getting polecat after creation: %w", err)
	}

	// Verify worktree was actually created (fixes #1070)
	// The identity bead may exist but worktree creation can fail silently
	if err := verifyWorktreeExists(polecatObj.ClonePath); err != nil {
		// Clean up the partial state before returning error
		_ = polecatMgr.Remove(polecatName, true) // force=true to clean up partial state
		return nil, fmt.Errorf("worktree verification failed for %s: %w\nHint: try 'gt polecat nuke %s/%s --force' to clean up",
			polecatName, err, rigName, polecatName)
	}

	// Get session manager for session name (session start is deferred)
	polecatSessMgr := polecat.NewSessionManager(t, r)
	sessionName := polecatSessMgr.SessionName(polecatName)

	fmt.Printf("%s Polecat %s spawned (session start deferred)\n", style.Bold.Render("✓"), polecatName)

	// Log spawn event to activity feed
	_ = events.LogFeed(events.TypeSpawn, "gt", events.SpawnPayload(rigName, polecatName))

	// Compute effective base branch (strip origin/ prefix since formula prepends it)
	effectiveBranch := strings.TrimPrefix(baseBranch, "origin/")
	if effectiveBranch == "" {
		effectiveBranch = r.DefaultBranch()
	}
	if opts.ResumeBranch != "" {
		effectiveBranch = opts.ResumeBranch
	}

	return &SpawnedPolecatInfo{
		RigName:           rigName,
		PolecatName:       polecatName,
		ClonePath:         polecatObj.ClonePath,
		SessionName:       sessionName,
		Pane:              "", // Empty until StartSession is called
		BaseBranch:        effectiveBranch,
		Branch:            polecatObj.Branch,
		HookSetAtomically: opts.HookBead != "",
		account:           opts.Account,
		agent:             opts.Agent,
	}, nil
}

// spawnNamedPolecatForSling creates or revives the exact polecat requested by
// the target. Existing idle identities use the same branch/reset/bead lifecycle
// as pooled reuse so the name and durable profile survive without preserving
// unrelated work from the previous run.
func spawnNamedPolecatForSling(rigName string, r *rig.Rig, polecatMgr *polecat.Manager, t *tmux.Tmux, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
	polecatName := opts.PolecatName
	if polecatName == "" || polecatName == "." || polecatName == ".." || strings.ContainsAny(polecatName, "/\\") {
		return nil, fmt.Errorf("invalid named polecat %q", polecatName)
	}

	polecatSessMgr := polecat.NewSessionManager(t, r)
	sessionName := polecatSessMgr.SessionName(polecatName)
	if running, err := t.HasSession(sessionName); err == nil && running {
		return nil, fmt.Errorf("target polecat %s/%s has an active session but its pane could not be resolved", rigName, polecatName)
	}

	polecatDir := filepath.Join(r.Path, "polecats", polecatName)
	polecatDirInfo, statErr := os.Stat(polecatDir)
	if statErr == nil && polecatDirInfo.IsDir() {
		polecatObj, err := polecatMgr.Get(polecatName)
		if err != nil {
			return nil, fmt.Errorf("getting named polecat %s: %w", polecatName, err)
		}
		if polecatObj.State != polecat.StateIdle {
			decision := polecatMgr.ReuseDecisionForPolecat(polecatName, polecatObj.State)
			reason := decision.Reason
			if reason == "" {
				reason = "state=" + string(polecatObj.State)
			}
			return nil, fmt.Errorf("%w: named polecat %s is not idle (%s)", polecat.ErrPolecatNeedsRecovery, polecatName, reason)
		}

		baseBranch := opts.BaseBranch
		if opts.ResumeBranch == "" && baseBranch != "" && !strings.HasPrefix(baseBranch, "origin/") {
			baseBranch = "origin/" + baseBranch
		}
		reused, err := polecatMgr.ReuseIdlePolecat(polecatName, polecat.AddOptions{
			HookBead:     opts.HookBead,
			AgentProfile: opts.Agent,
			BaseBranch:   baseBranch,
			ResumeBranch: opts.ResumeBranch,
		})
		if err != nil {
			return nil, fmt.Errorf("reusing named polecat %s: %w", polecatName, err)
		}
		if err := verifyWorktreeExists(reused.ClonePath); err != nil {
			return nil, fmt.Errorf("worktree verification failed for reused %s: %w", polecatName, err)
		}

		effectiveBranch := strings.TrimPrefix(baseBranch, "origin/")
		if effectiveBranch == "" {
			effectiveBranch = r.DefaultBranch()
		}
		if opts.ResumeBranch != "" {
			effectiveBranch = opts.ResumeBranch
		}
		fmt.Printf("%s Polecat %s reused (session start deferred)\n", style.Bold.Render("✓"), polecatName)
		return &SpawnedPolecatInfo{
			RigName:           rigName,
			PolecatName:       polecatName,
			ClonePath:         reused.ClonePath,
			SessionName:       sessionName,
			BaseBranch:        effectiveBranch,
			Branch:            reused.Branch,
			HookSetAtomically: opts.HookBead != "",
			account:           opts.Account,
			agent:             opts.Agent,
		}, nil
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("checking named polecat %s: %w", polecatName, statErr)
	}

	// A nuked identity can outlive its worktree. If no new override is supplied,
	// carry the identity's previous profile into the recreated worktree.
	agentProfile := opts.Agent
	if agentProfile == "" {
		persisted, profileErr := polecatMgr.AgentProfile(polecatName)
		if profileErr != nil {
			return nil, fmt.Errorf("reading persisted agent profile for %s: %w", polecatName, profileErr)
		}
		agentProfile = persisted
	}
	baseBranch := opts.BaseBranch
	if opts.ResumeBranch == "" && baseBranch != "" && !strings.HasPrefix(baseBranch, "origin/") {
		baseBranch = "origin/" + baseBranch
	}
	addOpts := polecat.AddOptions{
		HookBead:     opts.HookBead,
		AgentProfile: agentProfile,
		BaseBranch:   baseBranch,
		ResumeBranch: opts.ResumeBranch,
	}
	if _, err := polecatMgr.AddWithOptions(polecatName, addOpts); err != nil {
		return nil, fmt.Errorf("creating named polecat %s: %w", polecatName, err)
	}
	polecatObj, err := polecatMgr.Get(polecatName)
	if err != nil {
		return nil, fmt.Errorf("getting named polecat after creation: %w", err)
	}
	if err := verifyWorktreeExists(polecatObj.ClonePath); err != nil {
		return nil, fmt.Errorf("worktree verification failed for %s: %w", polecatName, err)
	}

	effectiveBranch := strings.TrimPrefix(baseBranch, "origin/")
	if effectiveBranch == "" {
		effectiveBranch = r.DefaultBranch()
	}
	if opts.ResumeBranch != "" {
		effectiveBranch = opts.ResumeBranch
	}
	fmt.Printf("%s Polecat %s created (session start deferred)\n", style.Bold.Render("✓"), polecatName)
	return &SpawnedPolecatInfo{
		RigName:           rigName,
		PolecatName:       polecatName,
		ClonePath:         polecatObj.ClonePath,
		SessionName:       sessionName,
		BaseBranch:        effectiveBranch,
		Branch:            polecatObj.Branch,
		HookSetAtomically: opts.HookBead != "",
		account:           opts.Account,
		agent:             agentProfile,
	}, nil
}

// StartSession starts the tmux session for a spawned polecat.
// This is called after the molecule/bead is attached, so the polecat
// sees its work when gt prime runs on session start.
// Returns the pane ID after session start.
func (s *SpawnedPolecatInfo) StartSession() (string, error) {
	if s.SessionStarted() {
		return s.Pane, nil
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Load rig config
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	r, err := rigMgr.GetRig(s.RigName)
	if err != nil {
		return "", fmt.Errorf("rig '%s' not found", s.RigName)
	}

	// Resolve account
	accountsPath := constants.MayorAccountsPath(townRoot)
	claudeConfigDir, _, err := config.ResolveAccountConfigDir(accountsPath, s.account)
	if err != nil {
		return "", fmt.Errorf("resolving account: %w", err)
	}

	// Start session
	t := tmux.NewTmux()
	polecatSessMgr := polecat.NewSessionManager(t, r)
	agentProfile := s.agent
	if agentProfile == "" {
		agentProfile, err = polecatSessMgr.AgentProfile(s.PolecatName)
		if err != nil {
			return "", fmt.Errorf("resolving persisted agent profile: %w", err)
		}
	}

	fmt.Printf("Starting session for %s/%s...\n", s.RigName, s.PolecatName)
	startOpts := polecat.SessionStartOptions{
		RuntimeConfigDir: claudeConfigDir,
		Agent:            agentProfile,
	}
	if err := polecatSessMgr.Start(s.PolecatName, startOpts); err != nil {
		return "", fmt.Errorf("starting session: %w", err)
	}

	// Wait for runtime to be fully ready before returning.
	// When an agent override is specified (e.g., --agent codex), resolve the runtime
	// config from the override so WaitForRuntimeReady uses the correct readiness
	// strategy (delay-based for Codex vs prompt-polling for Claude). Without this,
	// ResolveRoleAgentConfig returns the default agent (Claude) and polls for "❯ "
	// in a Codex session, always timing out after 30 seconds (gt-1j3m).
	spawnTownRoot := filepath.Dir(r.Path)
	var runtimeConfig *config.RuntimeConfig
	if agentProfile != "" {
		rc, _, err := config.ResolveAgentConfigWithOverride(spawnTownRoot, r.Path, agentProfile)
		if err != nil {
			return "", fmt.Errorf("resolving agent config for %s: %w", agentProfile, err)
		}
		runtimeConfig = rc
	} else {
		runtimeConfig = config.ResolveRoleAgentConfig("polecat", spawnTownRoot, r.Path)
	}
	if err := t.WaitForRuntimeReady(s.SessionName, runtimeConfig, 30*time.Second); err != nil {
		style.PrintWarning("runtime may not be fully ready: %v", err)
	}

	// Update agent state with retry logic (gt-94llt7: fail-safe Dolt writes).
	// Note: warn-only, not fail-hard. The tmux session is already started above,
	// so returning an error here would leave an orphaned session with no cleanup path.
	// The polecat can still function without the agent state update — it only affects
	// monitoring visibility, not correctness. Compare with createAgentBeadWithRetry
	// which fails hard because a polecat without an agent bead is untrackable.
	polecatGit := git.NewGit(r.Path)
	polecatMgr := polecat.NewManager(r, polecatGit, t)
	if err := polecatMgr.SetAgentStateWithRetry(s.PolecatName, "working"); err != nil {
		style.PrintWarning("could not update agent state after retries: %v", err)
	}

	// Update issue status from hooked to in_progress.
	// Also warn-only for the same reason: session is already running.
	if err := polecatMgr.SetState(s.PolecatName, polecat.StateWorking); err != nil {
		style.PrintWarning("could not update issue status to in_progress: %v", err)
	}

	// Get pane — if this fails, the session may have died during startup.
	// Kill the dead session to prevent "session already running" on next attempt (gt-jn40ft).
	pane, err := getSessionPane(s.SessionName)
	if err != nil {
		// Session likely died — clean up the tmux session so it doesn't block re-sling
		_ = t.KillSession(s.SessionName)
		return "", fmt.Errorf("getting pane for %s (session likely died during startup): %w", s.SessionName, err)
	}

	s.Pane = pane
	return pane, nil
}

// IsRigName checks if a target string is a rig name (not a role or path).
// Returns the rig name and true if it's a valid rig.
func IsRigName(target string) (string, bool) {
	// If it contains a slash, it's a path format (rig/role or rig/crew/name)
	if strings.Contains(target, "/") {
		return "", false
	}

	// Check known non-rig role names
	switch strings.ToLower(target) {
	case constants.RoleMayor, "may", constants.RoleDeacon, "dea", constants.RoleCrew, constants.RoleWitness, "wit", constants.RoleRefinery, "ref":
		return "", false
	}

	// Try to load as a rig
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", false
	}

	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return "", false
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	_, err = rigMgr.GetRig(target)
	if err != nil {
		return "", false
	}

	return target, true
}

// verifyWorktreeExists checks that a git worktree was actually created at the given path
// and that it is a functional git repository. Returns an error if the worktree is missing,
// has a broken .git reference, or fails basic git validation. (GH#2056)
func verifyWorktreeExists(clonePath string) error {
	return polecat.VerifyWorktreeExists(clonePath)
}
