package cmd

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// branchInfo holds parsed branch information.
type branchInfo struct {
	Branch string // Full branch name
	Issue  string // Issue ID extracted from branch
	Worker string // Worker name (polecat name)
}

// mqSubmitRigContext binds one submission to the registered owning rig. The
// source Git wrapper can point at an external candidate worktree, while Beads
// and push/target verification use the owning-rig authority.
type mqSubmitRigContext struct {
	RigName      string
	Rig          *rig.Rig
	Registry     config.RigEntry
	External     bool
	BeadsWorkDir string
	AuthorityGit *git.Git
}

// resolveMQSubmitRig resolves a registered rig for a submission. A path
// outside the town is never interpreted as a rig named ".."; it requires an
// explicit registered rig instead.
func resolveMQSubmitRig(townRoot, cwd, explicitRig string) (*mqSubmitRigContext, error) {
	townRoot = filepath.Clean(townRoot)
	cwd = filepath.Clean(cwd)
	explicitRig = strings.TrimSpace(explicitRig)

	rigName := explicitRig
	if rigName == "" {
		if !mqSubmitPathWithin(townRoot, cwd) {
			return nil, fmt.Errorf("current directory %q is outside Gas Town; use --rig <registered-rig> for an external worktree", cwd)
		}
		relPath, err := filepath.Rel(townRoot, cwd)
		if err != nil {
			return nil, fmt.Errorf("computing rig path: %w", err)
		}
		parts := strings.Split(relPath, string(filepath.Separator))
		if len(parts) > 0 && parts[0] != "" && parts[0] != "." {
			rigName = parts[0]
		} else {
			rigName = strings.TrimSpace(os.Getenv("GT_RIG"))
		}
	}
	if rigName == "" {
		return nil, fmt.Errorf("cannot determine owning rig; use --rig <registered-rig>")
	}
	if filepath.Base(rigName) != rigName || strings.ContainsAny(rigName, `/\\`) || rigName == "." || rigName == ".." {
		return nil, fmt.Errorf("invalid rig name %q", rigName)
	}

	rigsConfig, err := config.LoadRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"))
	if err != nil {
		return nil, fmt.Errorf("loading registered rigs: %w", err)
	}
	entry, ok := rigsConfig.Rigs[rigName]
	if !ok {
		return nil, fmt.Errorf("rig %q is not registered", rigName)
	}

	rigMgr := rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot))
	owner, err := rigMgr.GetRig(rigName)
	if err != nil {
		return nil, fmt.Errorf("registered rig %q is unavailable: %w", rigName, err)
	}

	external := !mqSubmitPathWithin(townRoot, cwd)
	beadsWorkDir := cwd
	if external || explicitRig != "" {
		beadsWorkDir = owner.Path
	}
	return &mqSubmitRigContext{
		RigName:      rigName,
		Rig:          owner,
		Registry:     entry,
		External:     external,
		BeadsWorkDir: beadsWorkDir,
		AuthorityGit: mqSubmitAuthorityGit(owner),
	}, nil
}

func mqSubmitPathWithin(base, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// mqSubmitAuthorityGit selects a managed rig clone for authoritative remote
// checks. The rig root is preferred, followed by the standard Refinery and
// Mayor clones. No per-rig config value is used as a remote authority.
func mqSubmitAuthorityGit(owner *rig.Rig) *git.Git {
	if owner == nil {
		return nil
	}
	candidates := []string{
		owner.Path,
		filepath.Join(owner.Path, "refinery", "rig"),
		filepath.Join(owner.Path, "mayor", "rig"),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(filepath.Join(candidate, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return git.NewGit(candidate)
		}
	}
	return nil
}

// registeredMQSubmitPushTarget returns only the registered target. GitURL is
// the intentional fallback when PushURL is absent; rig-local config is not an
// authority because it can silently disagree with the town registry.
func registeredMQSubmitPushTarget(ctx *mqSubmitRigContext) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("owning rig context is missing")
	}
	if target := strings.TrimSpace(ctx.Registry.PushURL); target != "" {
		return target, nil
	}
	if target := strings.TrimSpace(ctx.Registry.GitURL); target != "" {
		return target, nil
	}
	return "", fmt.Errorf("rig %q has no registered GitURL or PushURL", ctx.RigName)
}

func validateRegisteredMQSubmitPushTarget(authority *git.Git, registered string) error {
	registered = strings.TrimSpace(registered)
	if registered == "" {
		return fmt.Errorf("registered push target is empty")
	}
	if authority == nil {
		return fmt.Errorf("registered push target has no owning-rig repository")
	}
	configured, err := authority.GetPushURL("origin")
	if err != nil {
		return fmt.Errorf("reading owning-rig push target: %w", err)
	}
	if normalizeMQSubmitRemoteURL(configured) != normalizeMQSubmitRemoteURL(registered) {
		return fmt.Errorf("owning-rig push target %q does not match registered target %q", configured, registered)
	}
	return nil
}

// normalizeMQSubmitRemoteURL removes only syntax that is proven equivalent:
// URL case in the host, a trailing slash, and a trailing .git suffix. It keeps
// the scheme, host, port, and path distinct so HTTPS/SSH or different hosts and
// repositories cannot be treated as the same push endpoint.
func normalizeMQSubmitRemoteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	// Git's scp-like SSH spelling is equivalent to ssh://host/path, but not to
	// HTTPS or another transport. Ignore the login name; the endpoint identity
	// is represented by the SSH transport, host, and repository path.
	if !strings.Contains(raw, "://") {
		if colon := strings.IndexByte(raw, ':'); colon > 0 && strings.Contains(raw[:colon], "@") {
			loginHost := raw[:colon]
			pathPart := raw[colon+1:]
			if at := strings.LastIndexByte(loginHost, '@'); at >= 0 && at+1 < len(loginHost) {
				host := strings.ToLower(loginHost[at+1:])
				return "ssh://" + host + normalizeMQSubmitRemotePath("/"+pathPart)
			}
		}
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" {
		// Malformed or scheme-less values are compared byte-for-byte after the
		// minimal suffix normalization, which is fail-closed for authority use.
		return strings.TrimSuffix(strings.TrimSuffix(raw, "/"), ".git")
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	if port := parsed.Port(); port != "" {
		host += ":" + port
	}
	remotePath := parsed.EscapedPath()
	if remotePath == "" {
		remotePath = "/"
	}
	remotePath = normalizeMQSubmitRemotePath(remotePath)
	result := scheme + "://"
	if host != "" {
		result += host
	}
	result += remotePath
	if parsed.RawQuery != "" {
		result += "?" + parsed.RawQuery
	}
	if parsed.Fragment != "" {
		result += "#" + parsed.Fragment
	}
	return result
}

func normalizeMQSubmitRemotePath(remotePath string) string {
	remotePath = strings.TrimRight(remotePath, "/")
	remotePath = strings.TrimSuffix(remotePath, ".git")
	if remotePath == "" {
		return "/"
	}
	if !strings.HasPrefix(remotePath, "/") {
		return "/" + remotePath
	}
	return remotePath
}

func validateMQSubmitTargetName(target string) error {
	if target != strings.TrimSpace(target) {
		return fmt.Errorf("invalid target branch %q", target)
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return fmt.Errorf("target branch is required")
	}
	if err := validateBranchName(target); err != nil {
		return fmt.Errorf("invalid target branch %q: %w", target, err)
	}
	if target == "HEAD" || strings.HasPrefix(target, "refs/") || strings.HasPrefix(target, "origin/") || strings.HasPrefix(target, "upstream/") || strings.HasPrefix(target, "-") {
		return fmt.Errorf("invalid target branch %q", target)
	}
	for _, r := range target {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("invalid target branch %q", target)
		}
	}
	for _, part := range strings.Split(target, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return fmt.Errorf("invalid target branch %q", target)
		}
	}
	return nil
}

func mqSubmitExplicitTarget(explicit, configured string) string {
	if target := strings.TrimSpace(explicit); target != "" {
		return target
	}
	return strings.TrimSpace(configured)
}

func validateMQSubmitSourceTarget(source, target string) error {
	if strings.TrimSpace(source) == strings.TrimSpace(target) {
		return fmt.Errorf("source branch %q cannot be the target branch", source)
	}
	return nil
}

func validateMQSubmitExistingMR(mr *beads.Issue, branch, target, rigName, commitSHA string) error {
	if mr == nil {
		return fmt.Errorf("merge request is missing")
	}
	fields := beads.ParseMRFields(mr)
	if fields == nil {
		return fmt.Errorf("merge request %s has no structured routing fields", mr.ID)
	}
	if strings.TrimSpace(fields.Branch) != strings.TrimSpace(branch) {
		return fmt.Errorf("merge request %s source branch %q does not match %q", mr.ID, fields.Branch, branch)
	}
	if strings.TrimSpace(fields.Target) != strings.TrimSpace(target) {
		return fmt.Errorf("merge request %s target %q does not match %q", mr.ID, fields.Target, target)
	}
	if strings.TrimSpace(fields.Rig) != strings.TrimSpace(rigName) {
		return fmt.Errorf("merge request %s rig %q does not match %q", mr.ID, fields.Rig, rigName)
	}
	if strings.TrimSpace(fields.CommitSHA) != strings.TrimSpace(commitSHA) {
		return fmt.Errorf("merge request %s commit_sha %q does not match %q", mr.ID, fields.CommitSHA, commitSHA)
	}
	return nil
}

func validateMQSubmitTarget(authority *git.Git, target string) error {
	if err := validateMQSubmitTargetName(target); err != nil {
		return err
	}
	if authority == nil {
		return fmt.Errorf("cannot validate target %q without owning-rig repository", target)
	}
	exists, err := authority.PushRemoteBranchExists("origin", target)
	if err != nil {
		return fmt.Errorf("validate target branch %q on registered push target: %w", target, err)
	}
	if !exists {
		return fmt.Errorf("target branch %q does not exist on registered push target", target)
	}
	return nil
}

// issuePattern matches issue IDs in branch names (e.g., "gt-xyz" or "gt-abc.1")
var issuePattern = regexp.MustCompile(`([a-z]+-[a-z0-9]+(?:\.[0-9]+)?)`)

// parseBranchName extracts issue ID and worker from a branch name.
// Supports formats:
//   - polecat/<worker>/<issue>[+|@]<suffix>  → issue=<issue>, worker=<worker>
//   - polecat/<worker>/<issue>  → issue=<issue>, worker=<worker>
//   - polecat/<worker>-<suffix>  → issue="", worker=<worker>
//   - <issue>                   → issue=<issue>, worker=""
func parseBranchName(branch string) branchInfo {
	info := branchInfo{Branch: branch}

	if meta, ok := polecat.ParseBranchName(branch); ok {
		info.Worker = meta.Polecat
		info.Issue = meta.Issue
		return info
	}
	if strings.HasPrefix(branch, "polecat/") {
		return info
	}

	// Try to find an issue ID pattern in the branch name
	// Common patterns: prefix-xxx, prefix-xxx.n (subtask)
	if matches := issuePattern.FindStringSubmatch(branch); len(matches) > 1 {
		info.Issue = matches[1]
	}

	return info
}

func runMqSubmit(cmd *cobra.Command, args []string) error {
	// Find workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Read the source worktree before resolving the owning rig. An external
	// worktree is allowed only when --rig explicitly selects a registered rig.
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting current directory: %w", err)
	}
	explicitRig := strings.TrimSpace(mqSubmitRig)
	discoveredRig := explicitRig
	if explicitRig == "" {
		// Preserve the town-root shell-alias behavior, but reject paths outside
		// the town rather than accepting filepath.Rel's ".." as a rig name.
		discoveredRig, _, err = findCurrentRig(townRoot)
		if err != nil {
			return err
		}
	}

	// When gt is invoked via shell alias (cd ~/gt && gt), cwd is the town
	// root, not the polecat's worktree. Reconstruct actual path.
	if cwd == townRoot {
		// Gate polecat cwd switch on GT_ROLE: coordinators may have stale GT_POLECAT.
		isPolecat := false
		if role := os.Getenv("GT_ROLE"); role != "" {
			parsedRole, _, _ := parseRoleString(role)
			isPolecat = parsedRole == RolePolecat
		} else {
			isPolecat = os.Getenv("GT_POLECAT") != ""
		}
		if polecatName := os.Getenv("GT_POLECAT"); polecatName != "" && discoveredRig != "" && isPolecat {
			polecatClone := filepath.Join(townRoot, discoveredRig, "polecats", polecatName, discoveredRig)
			if _, err := os.Stat(polecatClone); err == nil {
				cwd = polecatClone
			} else {
				polecatClone = filepath.Join(townRoot, discoveredRig, "polecats", polecatName)
				if _, err := os.Stat(filepath.Join(polecatClone, ".git")); err == nil {
					cwd = polecatClone
				}
			}
		} else if crewName := os.Getenv("GT_CREW"); crewName != "" && discoveredRig != "" {
			crewClone := filepath.Join(townRoot, discoveredRig, "crew", crewName)
			if _, err := os.Stat(crewClone); err == nil {
				cwd = crewClone
			}
		}
	}

	// Resolve the registered owner after shell-alias cwd reconstruction. The
	// source Git wrapper remains bound to cwd, while owner Beads and push/target
	// verification are bound to this context for external candidates.
	submitRig, err := resolveMQSubmitRig(townRoot, cwd, explicitRig)
	if err != nil {
		return err
	}
	rigName := submitRig.RigName
	g := git.NewGit(cwd)
	verificationGit := g
	if submitRig.External || explicitRig != "" {
		registeredTarget, targetErr := registeredMQSubmitPushTarget(submitRig)
		if targetErr != nil {
			return targetErr
		}
		if targetErr := validateRegisteredMQSubmitPushTarget(submitRig.AuthorityGit, registeredTarget); targetErr != nil {
			return targetErr
		}
		verificationGit = submitRig.AuthorityGit
	}

	// Get current branch
	branch := mqSubmitBranch
	if branch == "" {
		branch, err = g.CurrentBranch()
		if err != nil {
			return fmt.Errorf("getting current branch: %w", err)
		}
	}

	// Get configured default branch for this rig
	defaultBranch := "main" // fallback
	if rigCfg, err := rig.LoadRigConfig(filepath.Join(townRoot, rigName)); err == nil && rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}

	if branch == defaultBranch || branch == "master" {
		return fmt.Errorf("cannot submit %s/master branch to merge queue", defaultBranch)
	}

	// Parse branch info
	info := parseBranchName(branch)

	// Override with explicit flags
	issueID := mqSubmitIssue
	if issueID == "" {
		issueID = info.Issue
	}
	worker := info.Worker

	if issueID == "" {
		return fmt.Errorf("cannot determine source issue from branch '%s'; use --issue to specify", branch)
	}

	// Initialize the owning-rig Beads context for external/explicit routing,
	// then resolve the source through town-level routing for source-owned ops.
	beadsWorkDir := cwd
	if submitRig.External || explicitRig != "" {
		beadsWorkDir = submitRig.BeadsWorkDir
	}
	bd := beads.New(beadsWorkDir)
	sourceInfo, err := resolveSubmitSourceIssue(beadsWorkDir, issueID)
	if err != nil {
		return fmt.Errorf("source issue validation failed: %w", err)
	}
	sourceBD := sourceInfo.BD
	sourceIssue := sourceInfo.Issue

	// Determine target branch. Priority: explicit --target > --epic >
	// formula_vars base_branch > integration branch auto-detect > rig default.
	target := mqSubmitExplicitTarget(mqSubmitTarget, defaultBranch)
	if strings.TrimSpace(mqSubmitTarget) == "" && mqSubmitEpic != "" {
		// Explicit --epic flag: read stored branch name, fall back to template
		rigPath := filepath.Join(townRoot, rigName)
		target = resolveIntegrationBranchName(sourceBD, rigPath, mqSubmitEpic)
	} else if strings.TrimSpace(mqSubmitTarget) == "" {
		// Check for explicit --base-branch override in formula vars on the source issue.
		// When gt sling dispatches with --base-branch, the value is persisted in
		// the bead's formula_vars field. Without this check, MRs created via
		// gt mq submit always target the rig's default branch (usually main),
		// even when the polecat was working against a feature branch.
		if af := beads.ParseAttachmentFields(sourceIssue); af != nil {
			if bb := extractFormulaVar(af.FormulaVars, "base_branch"); bb != "" && bb != defaultBranch {
				target = bb
				fmt.Printf("  Target branch override: %s (from formula_vars)\n", target)
			}
		}

		// Auto-detect: check if source issue has a parent epic with an integration branch
		// Only if no explicit base_branch was found above
		if target == defaultBranch {
			refineryEnabled := true
			rigPath := filepath.Join(townRoot, rigName)
			settingsPath := filepath.Join(rigPath, "settings", "config.json")
			if settings, err := config.LoadRigSettings(settingsPath); err == nil && settings.MergeQueue != nil {
				refineryEnabled = settings.MergeQueue.IsRefineryIntegrationEnabled()
			}
			if refineryEnabled {
				autoTarget, err := beads.DetectIntegrationBranch(sourceBD, verificationGit, issueID)
				if err != nil {
					// Non-fatal: log and continue with default branch as target
					fmt.Printf("  %s\n", style.Dim.Render(fmt.Sprintf("(note: %v)", err)))
				} else if autoTarget != "" {
					target = autoTarget
				}
			}
		}
	}

	if err := validateMQSubmitSourceTarget(branch, target); err != nil {
		return err
	}
	if err := validateMQSubmitTarget(verificationGit, target); err != nil {
		return err
	}

	// Get source issue for priority inheritance and dependency check
	var priority int
	if mqSubmitPriority >= 0 {
		priority = mqSubmitPriority
	} else {
		priority = sourceIssue.Priority
	}

	// Enforce molecule step dependencies before allowing submit.
	// If the source issue has an attached molecule, verify that prerequisite
	// steps are complete. This prevents polecats from skipping steps like
	// self-review, build-check, or state-update.
	if !mqSubmitSkipDeps && !mqSubmitResubmit && sourceIssue != nil {
		if err := checkMoleculeStepDeps(sourceBD, sourceIssue); err != nil {
			return err
		}
	}

	// GH#3032/wa-skj: resolve the submitted branch tip for MR dedup and
	// verification. With --branch this can differ from the checked-out HEAD.
	commitSHA, err := resolveMQSubmitCommitSHA(g, branch)
	if err != nil {
		return fmt.Errorf("could not resolve submitted branch SHA: %w", err)
	}

	// Build MR bead title and description
	title := fmt.Sprintf("Merge: %s", issueID)
	description := fmt.Sprintf("branch: %s\ntarget: %s\nsource_issue: %s\nrig: %s",
		branch, target, issueID, rigName)
	if commitSHA != "" {
		description += fmt.Sprintf("\ncommit_sha: %s", commitSHA)
	}
	if worker != "" {
		description += fmt.Sprintf("\nworker: %s", worker)
	}

	// Verify before either an idempotent success or a new MR registration.
	// Refinery's later branch check is local-ref based, so missing/stale pushes
	// must fail here instead of producing a delayed refinery rejection.
	if err := verifyMQSubmitPushedBranch(verificationGit, branch, commitSHA); err != nil {
		return err
	}

	// Check if MR bead already exists for this branch+SHA (idempotency)
	var mrIssue *beads.Issue
	var existingMR *beads.Issue
	existingMR, err = bd.FindMRForBranchAndSHA(branch, commitSHA)
	if err != nil {
		return fmt.Errorf("checking for existing merge request: %w", err)
	}

	if existingMR != nil {
		if err := validateMergeRequestSource(existingMR, issueID, sourceIssue); err != nil {
			return fmt.Errorf("existing merge request validation failed: %w", err)
		}
		if err := validateMQSubmitExistingMR(existingMR, branch, target, rigName, commitSHA); err != nil {
			return fmt.Errorf("existing merge request routing failed: %w", err)
		}
		mrIssue = existingMR
		fmt.Printf("%s MR already exists (idempotent)\n", style.Bold.Render("✓"))
	} else {
		// Create MR bead (ephemeral wisp - will be cleaned up after merge)
		mrIssue, err = bd.Create(beads.CreateOptions{
			Title:       title,
			Labels:      []string{"gt:merge-request"},
			Priority:    priority,
			Description: description,
			Ephemeral:   true,
			Rig:         rigName, // Ensure MR bead is created in the rig's database (gt-7y7)
		})
		if err != nil {
			return fmt.Errorf("creating merge request bead: %w", err)
		}

		// gt-gpy: Validate MR bead landed in the rig's database (warning only).
		if prefixErr := beads.ValidateRigPrefix(townRoot, rigName, mrIssue.ID); prefixErr != nil {
			style.PrintWarning("MR bead prefix mismatch: %v\nThe refinery may not find this MR — check 'gt mq list %s'", prefixErr, rigName)
		}

		// Nudge refinery to pick up the new MR
		nudgeRefinery(rigName, "MERGE_READY received - check inbox for pending work")

		// GH#2599: Back-link source issue to MR bead for discoverability.
		if issueID != "" {
			comment := fmt.Sprintf("MR created: %s", mrIssue.ID)
			if err := sourceBD.AddComment(issueID, comment); err != nil {
				style.PrintWarning("could not back-link source issue %s to MR %s: %v", issueID, mrIssue.ID, err)
			}
		}

		// Supersede older open MRs for the same source issue.
		// When a new polecat reattempts an issue, the old MR (different branch)
		// is orphaned. Close it so the queue and GitHub PRs stay clean.
		if issueID != "" {
			if oldMRs, err := bd.FindOpenMRsForIssue(issueID); err == nil {
				for _, old := range oldMRs {
					if old.ID == mrIssue.ID {
						continue // skip the one we just created
					}
					reason := fmt.Sprintf("superseded by %s", mrIssue.ID)
					if err := bd.CloseWithReason(reason, old.ID); err != nil {
						style.PrintWarning("could not supersede old MR %s: %v", old.ID, err)
						continue
					}
					fmt.Printf("  %s Superseded old MR: %s\n", style.Dim.Render("○"), old.ID)

					// Leave superseded remote branches intact. Branch deletion belongs to
					// verified post-merge cleanup, not submit-time queue maintenance.
				}
			}
		}
	}

	// Success output
	fmt.Printf("%s Submitted to merge queue\n", style.Bold.Render("✓"))
	fmt.Printf("  MR ID: %s\n", style.Bold.Render(mrIssue.ID))
	fmt.Printf("  Source: %s\n", branch)
	fmt.Printf("  Target: %s\n", target)
	fmt.Printf("  Issue: %s\n", issueID)
	if worker != "" {
		fmt.Printf("  Worker: %s\n", worker)
	}
	fmt.Printf("  Priority: P%d\n", priority)

	// Auto-cleanup for polecats: if this is a polecat branch and cleanup not disabled,
	// send lifecycle request and wait for termination
	if worker != "" && !mqSubmitNoCleanup {
		fmt.Println()
		fmt.Printf("%s Auto-cleanup: polecat work submitted\n", style.Bold.Render("✓"))
		if err := polecatCleanup(rigName, worker, townRoot); err != nil {
			// Non-fatal: warn but return success (MR was created)
			style.PrintWarning("Could not auto-cleanup: %v", err)
			fmt.Println(style.Dim.Render("  You may need to run 'gt handoff --shutdown' manually"))
			return nil
		}
		// polecatCleanup may timeout while waiting, but MR was already created
	}

	return nil
}

func resolveMQSubmitCommitSHA(g *git.Git, branch string) (string, error) {
	return g.Rev(fmt.Sprintf("refs/heads/%s^{commit}", branch))
}

func verifyMQSubmitPushedBranch(g *git.Git, branch, commitSHA string) error {
	if commitSHA != "" {
		if err := g.VerifyPushedCommit("origin", branch, commitSHA); err != nil {
			return fmt.Errorf("%w\n\nHint: run 'git push origin %s' first (or 'gt done'), then re-run 'gt mq submit'", err, branch)
		}
		return nil
	}

	exists, err := g.PushRemoteBranchExists("origin", branch)
	if err != nil {
		return fmt.Errorf("verify branch on origin: %w\n\nHint: run 'git push origin %s' first (or 'gt done'), then re-run 'gt mq submit'", err, branch)
	}
	if !exists {
		return fmt.Errorf("branch %q not found on origin\n\nHint: run 'git push origin %s' first (or 'gt done'), then re-run 'gt mq submit'", branch, branch)
	}
	return nil
}

// checkMoleculeStepDeps verifies that all prerequisite molecule steps are closed
// before allowing submission to the merge queue. Returns an error listing
// incomplete steps if any prerequisites are not yet done.
func checkMoleculeStepDeps(bd *beads.Beads, sourceIssue *beads.Issue) error {
	// Check if issue has an attached molecule
	fields := beads.ParseAttachmentFields(sourceIssue)
	if fields == nil || fields.AttachedMolecule == "" {
		return nil // No molecule attached — no enforcement needed
	}

	moleculeID := fields.AttachedMolecule

	// List all molecule steps (children of the molecule)
	children, err := bd.List(beads.ListOptions{
		Parent:   moleculeID,
		Status:   "all",
		Priority: -1,
	})
	if err != nil {
		// If we can't list steps, warn but don't block submission
		style.PrintWarning("could not check molecule steps for %s: %v", moleculeID, err)
		return nil
	}

	return validateMoleculePrereqs(children)
}

// validateMoleculePrereqs checks that all molecule steps that are prerequisites
// of the submit step are closed. Returns an error listing incomplete steps.
// Extracted for testability — accepts step data directly.
func validateMoleculePrereqs(children []*beads.Issue) error {
	if len(children) == 0 {
		return nil // No steps to check
	}

	// Find the submit step — it's the step whose title contains "submit"
	// (case-insensitive). All steps that come before it in the dependency
	// chain must be closed.
	submitSeq := 999999
	for _, child := range children {
		titleLower := strings.ToLower(child.Title)
		if strings.Contains(titleLower, "submit") {
			seq := extractStepSequence(child.ID)
			if seq < submitSeq {
				submitSeq = seq
			}
			break
		}
	}

	// Collect incomplete prerequisite steps.
	// A prerequisite is any step sequenced before the submit step (by step
	// number suffix) that is not closed. Steps at or after the submit step
	// are post-submit (await-verdict, self-clean) and don't need to be done.
	var incompleteSteps []*beads.Issue
	for _, child := range children {
		seq := extractStepSequence(child.ID)
		if seq >= submitSeq {
			continue // This is the submit step or a post-submit step
		}
		if child.Status != "closed" {
			incompleteSteps = append(incompleteSteps, child)
		}
	}

	if len(incompleteSteps) == 0 {
		return nil // All prerequisites are closed
	}

	// Sort by sequence for readable output
	sortStepsBySequence(incompleteSteps)

	// Build error message listing incomplete steps
	var sb strings.Builder
	sb.WriteString("molecule step dependencies not met — incomplete prerequisite steps:\n")
	for _, step := range incompleteSteps {
		sb.WriteString(fmt.Sprintf("  ✗ %s: %s [%s]\n", step.ID, step.Title, step.Status))
	}
	sb.WriteString(fmt.Sprintf("\nComplete these steps before submitting, or use --skip-deps to override."))

	return fmt.Errorf("%s", sb.String())
}

// polecatCleanup sends a lifecycle shutdown request to the witness and waits for termination.
// This is called after a polecat successfully submits an MR.
func polecatCleanup(rigName, worker, townRoot string) error {
	// Send lifecycle request to witness
	manager := rigName + "/witness"
	subject := fmt.Sprintf("LIFECYCLE: polecat-%s requesting shutdown", worker)
	body := fmt.Sprintf(`Lifecycle request from polecat %s.

Action: shutdown
Reason: MR submitted to merge queue
Time: %s

Please verify state and execute lifecycle action.
`, worker, time.Now().Format(time.RFC3339))

	// Send via gt mail
	cmd := exec.Command("gt", "mail", "send", manager,
		"-s", subject,
		"-m", body,
	)
	cmd.Dir = townRoot

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sending lifecycle request: %w: %s", err, string(out))
	}
	fmt.Printf("%s Sent shutdown request to %s\n", style.Bold.Render("✓"), manager)

	// Wait for retirement with periodic status
	fmt.Println()
	fmt.Printf("%s Waiting for retirement...\n", style.Dim.Render("◌"))
	fmt.Println(style.Dim.Render("(Witness will terminate this session)"))

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Timeout after 5 minutes to prevent indefinite blocking
	const maxCleanupWait = 5 * time.Minute
	timeout := time.After(maxCleanupWait)

	waitStart := time.Now()
	for {
		select {
		case <-ticker.C:
			elapsed := time.Since(waitStart).Round(time.Second)
			fmt.Printf("%s Still waiting (%v elapsed)...\n", style.Dim.Render("◌"), elapsed)
			if elapsed >= 2*time.Minute {
				fmt.Println(style.Dim.Render("  Hint: If witness isn't responding, you may need to:"))
				fmt.Println(style.Dim.Render("  - Check if witness is running: gt rig status"))
				fmt.Println(style.Dim.Render("  - Use Ctrl+C to abort and manually exit"))
			}
		case <-timeout:
			fmt.Printf("%s Timeout waiting for polecat retirement\n", style.WarningPrefix)
			fmt.Println(style.Dim.Render("  The polecat may have already terminated, or witness is unresponsive."))
			fmt.Println(style.Dim.Render("  You can verify with: gt polecat status"))
			return nil // Don't fail the MR submission just because cleanup timed out
		}
	}
}
