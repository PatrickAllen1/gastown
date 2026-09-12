package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery"
)

type fakeMQPostMergeManager struct {
	mr              *refinery.MergeRequest
	findErr         error
	postMergeErr    error
	postMergeCalled bool
	postMergeMR     *refinery.MergeRequest
}

func (m *fakeMQPostMergeManager) FindMRForPostMerge(string) (*refinery.MergeRequest, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	return m.mr, nil
}

func (m *fakeMQPostMergeManager) PostMergeMR(mr *refinery.MergeRequest) (*refinery.PostMergeResult, error) {
	m.postMergeCalled = true
	m.postMergeMR = mr
	if m.postMergeErr != nil {
		return nil, m.postMergeErr
	}
	return &refinery.PostMergeResult{MR: m.mr, MRClosed: true, SourceIssueClosed: true, SourceIssueID: m.mr.IssueID}, nil
}

type fakeMQPostMergeGit struct {
	verifyErr    error
	verifyErrs   map[string]error
	openPR       bool
	deleteErr    error
	remoteTip    string
	localHead    string
	tipErr       error
	patchErr     error
	patchSource  string
	patchTarget  string
	patchCommit  string
	lookupErr    error
	lookupRef    git.PullRequestRef
	lookupResult *git.PullRequestInfo

	verifiedCommits []string
	deletedBranches []string
	deletedHeads    []string
	localDeleted    []string
}

func (g *fakeMQPostMergeGit) VerifyPushedCommitReachableFromPushTarget(_, _, commit string) error {
	g.verifiedCommits = append(g.verifiedCommits, commit)
	if err, ok := g.verifyErrs[commit]; ok {
		return err
	}
	return g.verifyErr
}

func (g *fakeMQPostMergeGit) VerifyPushedCommitPatchEquivalentFromPushTarget(_, sourceBranch, targetBranch, commit string) error {
	g.patchSource = sourceBranch
	g.patchTarget = targetBranch
	g.patchCommit = commit
	return g.patchErr
}

func (g *fakeMQPostMergeGit) LookupPullRequest(ref git.PullRequestRef) (*git.PullRequestInfo, error) {
	g.lookupRef = ref
	if g.lookupErr != nil {
		return nil, g.lookupErr
	}
	return g.lookupResult, nil
}

func (g *fakeMQPostMergeGit) HasOpenPullRequest(git.PullRequestRef) bool {
	return g.openPR
}

func (g *fakeMQPostMergeGit) PushRemoteBranchTip(_, _ string) (string, error) {
	return g.remoteTip, g.tipErr
}

func (g *fakeMQPostMergeGit) Rev(string) (string, error) {
	return g.localHead, nil
}

func (g *fakeMQPostMergeGit) DeleteRemoteBranchIfAt(_, branch, expectedHash string) error {
	g.deletedBranches = append(g.deletedBranches, branch)
	g.deletedHeads = append(g.deletedHeads, expectedHash)
	return g.deleteErr
}

func (g *fakeMQPostMergeGit) DeleteBranch(branch string, _ bool) error {
	g.localDeleted = append(g.localDeleted, branch)
	return nil
}

func testMQPostMergeMR() *refinery.MergeRequest {
	return &refinery.MergeRequest{
		ID:           "gt-mr-proof",
		Branch:       "polecat/test/gt-proof+proof123",
		Worker:       "polecats/test",
		AgentBead:    "gt-gastown-polecat-test",
		IssueID:      "gt-proof",
		TargetBranch: "main",
		CommitSHA:    "abc123def456",
		Status:       refinery.MROpen,
	}
}

type bareMQPostMergeManager struct {
	mr              *refinery.MergeRequest
	postMergeCalled bool
}

func (m *bareMQPostMergeManager) FindMRForPostMerge(string) (*refinery.MergeRequest, error) {
	return m.mr, nil
}

func (m *bareMQPostMergeManager) PostMergeMR(mr *refinery.MergeRequest) (*refinery.PostMergeResult, error) {
	m.postMergeCalled = true
	return &refinery.PostMergeResult{MR: mr, MRClosed: true, SourceIssueClosed: true, SourceIssueID: mr.IssueID}, nil
}

func TestRunVerifiedMQPostMerge_ConsistentForgedSourceIssueWithoutDurableOwnershipFailsClosed(t *testing.T) {
	mr := testMQPostMergeMR()
	sourceAssignee := "gastown/polecats/test"
	source := &beads.Issue{ID: mr.IssueID, Type: "task", Status: string(beads.StatusOpen), Assignee: sourceAssignee}
	agent := completeMQSourceAgent(mr, sourceAssignee)
	rigPath := installMQSourceAuthorityStub(t, source, nil, agent)
	mgr := &bareMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{
		verifyErrs: map[string]error{mr.CommitSHA: errors.New("candidate is not an ancestor")},
		remoteTip:  mr.CommitSHA,
		localHead:  mr.CommitSHA,
	}

	_, _, err := runVerifiedMQPostMerge(mgr, rigPath, rigGit, mr.ID, false)
	if err == nil || !strings.Contains(err.Error(), "source issue") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want durable source ownership failure", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called for a source issue with no durable backlink")
	}
	if rigGit.patchSource != "" || rigGit.lookupRef != (git.PullRequestRef{}) {
		t.Fatalf("fallback acceptance attempted for forged source issue: patch=%q lookup=%+v", rigGit.patchSource, rigGit.lookupRef)
	}
	if len(rigGit.deletedBranches) != 0 || len(rigGit.localDeleted) != 0 {
		t.Fatalf("branch cleanup occurred for forged source issue: remote=%v local=%v", rigGit.deletedBranches, rigGit.localDeleted)
	}
}

func TestVerifyMQPostMergeSourceIssue_AcceptsExactGeneratedLifecycleOwnership(t *testing.T) {
	mr := testMQPostMergeMR()
	sourceAssignee := "gastown/polecats/test"
	source := &beads.Issue{ID: mr.IssueID, Type: "task", Status: string(beads.StatusOpen), Assignee: sourceAssignee}
	agent := completeMQSourceAgent(mr, sourceAssignee)
	rigPath := installMQSourceAuthorityStub(t, source, []beads.Comment{{IssueID: source.ID, Author: sourceAssignee, Text: "MR created: " + mr.ID}}, agent)

	if err := verifyMQPostMergeSourceIssue(rigPath, nil, mr); err != nil {
		t.Fatalf("verifyMQPostMergeSourceIssue: %v", err)
	}
}

func TestVerifyMQPostMergeSourceIssue_AcceptsExactCustomBranchBacklink(t *testing.T) {
	mr := testMQPostMergeMR()
	mr.Branch = "feature/custom-branch"
	mr.AgentBead = ""
	sourceAssignee := "gastown/polecats/test"
	source := &beads.Issue{ID: mr.IssueID, Type: "task", Status: string(beads.StatusOpen), Assignee: sourceAssignee}
	rigPath := installMQSourceAuthorityStub(t, source, []beads.Comment{{IssueID: source.ID, Author: sourceAssignee, Text: "MR created: " + mr.ID}}, nil)

	if err := verifyMQPostMergeSourceIssue(rigPath, nil, mr); err != nil {
		t.Fatalf("verifyMQPostMergeSourceIssue: %v", err)
	}
}

func TestVerifyMQPostMergeSourceIssue_RejectsMissingBacklink(t *testing.T) {
	mr := testMQPostMergeMR()
	mr.Branch = "feature/custom-branch"
	mr.AgentBead = ""
	source := &beads.Issue{ID: mr.IssueID, Type: "task", Status: string(beads.StatusOpen), Assignee: "gastown/polecats/test"}
	rigPath := installMQSourceAuthorityStub(t, source, []beads.Comment{{IssueID: "other-source", Author: source.Assignee, Text: "MR created: " + mr.ID}}, nil)

	err := verifyMQPostMergeSourceIssue(rigPath, nil, mr)
	if err == nil || !strings.Contains(err.Error(), "ownership record") {
		t.Fatalf("verifyMQPostMergeSourceIssue error = %v, want source-owned backlink failure", err)
	}
}

func TestVerifyMQPostMergeSourceIssue_RejectsAmbiguousBacklinkAuthors(t *testing.T) {
	mr := testMQPostMergeMR()
	mr.Branch = "feature/custom-branch"
	mr.AgentBead = ""
	sourceAssignee := "gastown/polecats/test"
	source := &beads.Issue{ID: mr.IssueID, Type: "task", Status: string(beads.StatusOpen), Assignee: sourceAssignee}
	comments := []beads.Comment{
		{IssueID: source.ID, Author: sourceAssignee, Text: "MR created: " + mr.ID},
		{IssueID: source.ID, Author: "gastown/polecats/victim", Text: "MR created: " + mr.ID},
	}
	rigPath := installMQSourceAuthorityStub(t, source, comments, nil)

	err := verifyMQPostMergeSourceIssue(rigPath, nil, mr)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("verifyMQPostMergeSourceIssue error = %v, want ambiguous backlink failure", err)
	}
}

func TestVerifyMQPostMergeSourceIssue_RejectsUngeneratedPolecatBranch(t *testing.T) {
	mr := testMQPostMergeMR()
	mr.Branch = "polecat/test/gt-proof"
	sourceAssignee := "gastown/polecats/test"
	source := &beads.Issue{ID: mr.IssueID, Type: "task", Status: string(beads.StatusOpen), Assignee: sourceAssignee}
	agent := completeMQSourceAgent(mr, sourceAssignee)
	rigPath := installMQSourceAuthorityStub(t, source, []beads.Comment{{IssueID: source.ID, Author: sourceAssignee, Text: "MR created: " + mr.ID}}, agent)

	err := verifyMQPostMergeSourceIssue(rigPath, nil, mr)
	if err == nil || !strings.Contains(err.Error(), "generated") {
		t.Fatalf("verifyMQPostMergeSourceIssue error = %v, want ungenerated branch failure", err)
	}
}

func TestVerifyMQPostMergeSourceIssue_RejectsAgentMRBranchOrSourceDrift(t *testing.T) {
	mr := testMQPostMergeMR()
	sourceAssignee := "gastown/polecats/test"
	source := &beads.Issue{ID: mr.IssueID, Type: "task", Status: string(beads.StatusOpen), Assignee: sourceAssignee}
	agent := completeMQSourceAgent(mr, sourceAssignee)
	agent.Description = beads.FormatAgentDescription("agent", &beads.AgentFields{
		RoleType:        "polecat",
		Rig:             "gastown",
		ExitType:        "COMPLETED",
		MRID:            "gt-other-mr",
		Branch:          mr.Branch,
		LastSourceIssue: mr.IssueID,
	})
	rigPath := installMQSourceAuthorityStub(t, source, []beads.Comment{{IssueID: source.ID, Author: sourceAssignee, Text: "MR created: " + mr.ID}}, agent)

	err := verifyMQPostMergeSourceIssue(rigPath, nil, mr)
	if err == nil || !strings.Contains(err.Error(), "MRID") {
		t.Fatalf("verifyMQPostMergeSourceIssue error = %v, want agent MR drift failure", err)
	}
}

func TestVerifyMQPostMergeSourceIssue_RejectsMissingLegacyLifecycle(t *testing.T) {
	mr := testMQPostMergeMR()
	sourceAssignee := "gastown/polecats/test"
	source := &beads.Issue{ID: mr.IssueID, Type: "task", Status: string(beads.StatusOpen), Assignee: sourceAssignee}
	agent := &beads.Issue{ID: mr.AgentBead, Type: "agent", Labels: []string{"gt:agent"}, Description: "role_type: polecat\nrig: gastown\nagent_state: done"}
	rigPath := installMQSourceAuthorityStub(t, source, []beads.Comment{{IssueID: source.ID, Author: sourceAssignee, Text: "MR created: " + mr.ID}}, agent)

	err := verifyMQPostMergeSourceIssue(rigPath, nil, mr)
	if err == nil || !strings.Contains(err.Error(), "lifecycle") {
		t.Fatalf("verifyMQPostMergeSourceIssue error = %v, want missing lifecycle failure", err)
	}
}

func completeMQSourceAgent(mr *refinery.MergeRequest, assignee string) *beads.Issue {
	return &beads.Issue{
		ID:       mr.AgentBead,
		Type:     "agent",
		Labels:   []string{"gt:agent"},
		Assignee: assignee,
		Status:   string(beads.StatusOpen),
		Description: beads.FormatAgentDescription("agent", &beads.AgentFields{
			RoleType:        "polecat",
			Rig:             "gastown",
			AgentState:      "done",
			ActiveMR:        mr.ID,
			ExitType:        "COMPLETED",
			MRID:            mr.ID,
			Branch:          mr.Branch,
			LastSourceIssue: mr.IssueID,
		}),
	}
}

func setupCompleteMQSourceForTest(t *testing.T, mr *refinery.MergeRequest) string {
	t.Helper()
	const assignee = "gastown/polecats/test"
	source := &beads.Issue{ID: mr.IssueID, Type: "task", Status: string(beads.StatusOpen), Assignee: assignee}
	comments := []beads.Comment{{IssueID: source.ID, Author: assignee, Text: "MR created: " + mr.ID}}
	return installMQSourceAuthorityStub(t, source, comments, completeMQSourceAgent(mr, assignee))
}

func installMQSourceAuthorityStub(t *testing.T, source *beads.Issue, comments []beads.Comment, agent *beads.Issue) string {
	t.Helper()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "gastown")
	for _, dir := range []string{filepath.Join(townRoot, "mayor"), filepath.Join(townRoot, ".beads"), filepath.Join(rigPath, ".beads")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write town sentinel: %v", err)
	}
	jsonFile := func(name string, value any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		path := filepath.Join(townRoot, name+".json")
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	sourcePath := jsonFile("source", []*beads.Issue{source})
	commentsPath := jsonFile("comments", comments)
	agentPath := ""
	if agent != nil {
		agentPath = jsonFile("agent", []*beads.Issue{agent})
	}
	binDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--allow-stale" ]; then shift; fi
if [ "$1" = "version" ]; then echo "bd test"; exit 0; fi
if [ "$1" = "show" ] && [ "$2" = %q ]; then cat %q; exit 0; fi
if [ "$1" = "show" ] && [ "$2" = %q ]; then cat %q; exit 0; fi
if [ "$1" = "comments" ] && [ "$2" = %q ]; then cat %q; exit 0; fi
printf '[]\n'
`, source.ID, sourcePath, func() string {
		if agent == nil {
			return "__no_agent__"
		}
		return agent.ID
	}(), func() string {
		if agent == nil {
			return sourcePath
		}
		return agentPath
	}(), source.ID, commentsPath)
	bdPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)
	return rigPath
}

func TestRunVerifiedMQPostMerge_ProofFailurePreservesRecordsAndBranch(t *testing.T) {
	mr := testMQPostMergeMR()
	rigPath := setupCompleteMQSourceForTest(t, mr)
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{
		verifyErr: errors.New("not reachable"),
		patchErr:  errors.New("not preserved"),
	}

	_, _, err := runVerifiedMQPostMerge(mgr, rigPath, rigGit, mgr.mr.ID, false)
	if err == nil || !strings.Contains(err.Error(), "merge proof failed") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want merge proof failure", err)
	}
	if !strings.Contains(err.Error(), mgr.mr.CommitSHA) {
		t.Fatalf("proof error %q does not mention submitted head %s", err, mgr.mr.CommitSHA)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called after failed proof")
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("remote branch deleted after failed proof: %v", rigGit.deletedBranches)
	}
	if len(rigGit.localDeleted) != 0 {
		t.Fatalf("local branch deleted after failed proof: %v", rigGit.localDeleted)
	}
}

func TestRunVerifiedMQPostMerge_VerifiedHeadClosesAndLeaseDeletes(t *testing.T) {
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{remoteTip: mgr.mr.CommitSHA, localHead: mgr.mr.CommitSHA}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after successful proof")
	}
	if mgr.postMergeMR != mgr.mr {
		t.Fatal("PostMerge did not use the verified MR snapshot")
	}
	if len(rigGit.verifiedCommits) != 1 || rigGit.verifiedCommits[0] != mgr.mr.CommitSHA {
		t.Fatalf("verified commits = %v, want [%s]", rigGit.verifiedCommits, mgr.mr.CommitSHA)
	}
	if !cleanup.RemoteDeleted || len(rigGit.deletedBranches) != 1 || rigGit.deletedBranches[0] != mgr.mr.Branch {
		t.Fatalf("remote delete = cleanup=%+v branches=%v", cleanup, rigGit.deletedBranches)
	}
	if len(rigGit.deletedHeads) != 1 || rigGit.deletedHeads[0] != mgr.mr.CommitSHA {
		t.Fatalf("deleted heads = %v, want [%s]", rigGit.deletedHeads, mgr.mr.CommitSHA)
	}
	if !cleanup.LocalDeleted || len(rigGit.localDeleted) != 1 || rigGit.localDeleted[0] != mgr.mr.Branch {
		t.Fatalf("local delete = cleanup=%+v local=%v", cleanup, rigGit.localDeleted)
	}
}

func TestRunVerifiedMQPostMerge_SkipBranchDeleteStillRequiresProof(t *testing.T) {
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, true)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after successful proof")
	}
	if len(rigGit.verifiedCommits) != 1 || rigGit.verifiedCommits[0] != mgr.mr.CommitSHA {
		t.Fatalf("verified commits = %v, want [%s]", rigGit.verifiedCommits, mgr.mr.CommitSHA)
	}
	if !cleanup.Skipped {
		t.Fatalf("cleanup.Skipped = false, cleanup=%+v", cleanup)
	}
	if len(rigGit.deletedBranches) != 0 || len(rigGit.localDeleted) != 0 {
		t.Fatalf("branch deleted despite skip: remote=%v local=%v", rigGit.deletedBranches, rigGit.localDeleted)
	}
}

func TestRunVerifiedMQPostMerge_OpenPRSkipsRemoteDeleteAfterProof(t *testing.T) {
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{openPR: true, localHead: mgr.mr.CommitSHA}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after successful proof")
	}
	if !cleanup.OpenPR {
		t.Fatalf("cleanup.OpenPR = false, cleanup=%+v", cleanup)
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("remote branch deleted despite open PR: %v", rigGit.deletedBranches)
	}
	if len(rigGit.localDeleted) != 1 || rigGit.localDeleted[0] != mgr.mr.Branch {
		t.Fatalf("local branch cleanup = %v, want [%s]", rigGit.localDeleted, mgr.mr.Branch)
	}
}

func TestRunVerifiedMQPostMerge_LeaseDeleteFailureReturnsAfterPostMerge(t *testing.T) {
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{remoteTip: mgr.mr.CommitSHA, deleteErr: errors.New("stale info")}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false)
	if err == nil || !strings.Contains(err.Error(), "remote branch delete") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want remote branch delete failure", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after successful proof")
	}
	if len(rigGit.deletedBranches) != 1 || rigGit.deletedBranches[0] != mgr.mr.Branch {
		t.Fatalf("remote delete attempts = %v, want [%s]", rigGit.deletedBranches, mgr.mr.Branch)
	}
	if len(rigGit.deletedHeads) != 1 || rigGit.deletedHeads[0] != mgr.mr.CommitSHA {
		t.Fatalf("delete lease heads = %v, want [%s]", rigGit.deletedHeads, mgr.mr.CommitSHA)
	}
	if len(rigGit.localDeleted) != 0 {
		t.Fatalf("local branch deleted after remote lease failure: %v", rigGit.localDeleted)
	}
}

func TestRunVerifiedMQPostMerge_MissingRemoteBranchIsIdempotentAfterProof(t *testing.T) {
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{localHead: mgr.mr.CommitSHA}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after successful proof")
	}
	if !cleanup.AlreadyGone {
		t.Fatalf("cleanup.AlreadyGone = false, cleanup=%+v", cleanup)
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("remote branch delete attempted for missing branch: %v", rigGit.deletedBranches)
	}
}

func TestRunVerifiedMQPostMerge_MissingSubmittedHeadFailsClosed(t *testing.T) {
	mr := testMQPostMergeMR()
	mr.CommitSHA = ""
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false)
	if err == nil || !strings.Contains(err.Error(), "missing submitted commit_sha") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want missing submitted head", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called with missing submitted head")
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("branch deleted with missing submitted head: %v", rigGit.deletedBranches)
	}
}

func TestRunVerifiedMQPostMerge_SourceTargetBranchFailsClosed(t *testing.T) {
	mr := testMQPostMergeMR()
	mr.Branch = "main"
	mr.TargetBranch = "main"
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false)
	if err == nil || !strings.Contains(err.Error(), "matches target branch") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want source/target failure", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called when source branch matched target")
	}
	if len(rigGit.deletedBranches) != 0 {
		t.Fatalf("branch deleted when source matched target: %v", rigGit.deletedBranches)
	}
}

func TestRunVerifiedMQPostMerge_AcceptsAuthenticatedPatchTransplant(t *testing.T) {
	mr := testMQPostMergeMR()
	mr.MergeCommit = "landing987"
	rigPath := setupCompleteMQSourceForTest(t, mr)
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{
		verifyErrs: map[string]error{mr.CommitSHA: errors.New("candidate is not an ancestor")},
		remoteTip:  mr.CommitSHA,
		localHead:  mr.CommitSHA,
	}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, rigPath, rigGit, mr.ID, false)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after authenticated patch-transplant proof")
	}
	if len(rigGit.verifiedCommits) != 2 || rigGit.verifiedCommits[0] != mr.CommitSHA || rigGit.verifiedCommits[1] != mr.MergeCommit {
		t.Fatalf("verified commits = %v, want [%s %s]", rigGit.verifiedCommits, mr.CommitSHA, mr.MergeCommit)
	}
	if rigGit.patchSource != mr.Branch || rigGit.patchTarget != mr.TargetBranch || rigGit.patchCommit != mr.CommitSHA {
		t.Fatalf("patch proof = source %q target %q commit %q, want %q %q %q", rigGit.patchSource, rigGit.patchTarget, rigGit.patchCommit, mr.Branch, mr.TargetBranch, mr.CommitSHA)
	}
	if !cleanup.RemoteDeleted || !cleanup.LocalDeleted {
		t.Fatalf("cleanup = %+v, want lease-safe branch deletion", cleanup)
	}
}

func TestRunVerifiedMQPostMerge_SourceBoundProofDoesNotRequireLandingField(t *testing.T) {
	mr := testMQPostMergeMR()
	rigPath := setupCompleteMQSourceForTest(t, mr)
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{
		verifyErrs: map[string]error{mr.CommitSHA: errors.New("candidate is not an ancestor")},
		remoteTip:  mr.CommitSHA,
		localHead:  mr.CommitSHA,
	}

	_, _, err := runVerifiedMQPostMerge(mgr, rigPath, rigGit, mr.ID, true)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after source-bound patch proof")
	}
	if len(rigGit.verifiedCommits) != 1 || rigGit.verifiedCommits[0] != mr.CommitSHA {
		t.Fatalf("verified commits = %v, want [%s]", rigGit.verifiedCommits, mr.CommitSHA)
	}
}

func TestRunVerifiedMQPostMerge_PatchTransplantDivergenceFailsClosed(t *testing.T) {
	mr := testMQPostMergeMR()
	mr.MergeCommit = "landing987"
	rigPath := setupCompleteMQSourceForTest(t, mr)
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{
		verifyErrs: map[string]error{mr.CommitSHA: errors.New("candidate is not an ancestor")},
		patchErr:   errors.New("submitted patch is not preserved"),
	}

	_, _, err := runVerifiedMQPostMerge(mgr, rigPath, rigGit, mr.ID, false)
	if err == nil || !strings.Contains(err.Error(), "merge proof failed") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want merge proof failure", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called for divergent patch transplant")
	}
	if len(rigGit.deletedBranches) != 0 || len(rigGit.localDeleted) != 0 {
		t.Fatalf("branch deleted after failed patch proof: remote=%v local=%v", rigGit.deletedBranches, rigGit.localDeleted)
	}
}

func TestRunVerifiedMQPostMerge_ForgedSourceIssueFailsClosed(t *testing.T) {
	mr := testMQPostMergeMR()
	mr.IssueID = "gt-victim"
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{
		verifyErrs: map[string]error{mr.CommitSHA: errors.New("candidate is not an ancestor")},
		remoteTip:  mr.CommitSHA,
		localHead:  mr.CommitSHA,
	}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mr.ID, false)
	if err == nil || !strings.Contains(err.Error(), "source issue") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want source issue authentication failure", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called for forged source issue")
	}
	if len(rigGit.deletedBranches) != 0 || len(rigGit.localDeleted) != 0 {
		t.Fatalf("branch deleted for forged source issue: remote=%v local=%v", rigGit.deletedBranches, rigGit.localDeleted)
	}
}

func TestRunVerifiedMQPostMerge_AcceptsRecordedMergedPRLanding(t *testing.T) {
	mr := testMQPostMergeMR()
	mr.PRURL = "https://github.com/upstream/repo/pull/42"
	mr.PRNumber = 42
	rigPath := setupCompleteMQSourceForTest(t, mr)
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{
		verifyErrs: map[string]error{mr.CommitSHA: errors.New("candidate is not an ancestor")},
		patchErr:   errors.New("patch proof not used"),
		lookupResult: &git.PullRequestInfo{
			Number:         mr.PRNumber,
			URL:            mr.PRURL,
			State:          "MERGED",
			HeadRefName:    mr.Branch,
			HeadSHA:        mr.CommitSHA,
			BaseRefName:    mr.TargetBranch,
			MergeCommitSHA: "landing987",
		},
		remoteTip: mr.CommitSHA,
		localHead: mr.CommitSHA,
	}

	_, _, err := runVerifiedMQPostMerge(mgr, rigPath, rigGit, mr.ID, true)
	if err != nil {
		t.Fatalf("runVerifiedMQPostMerge: %v", err)
	}
	if !mgr.postMergeCalled {
		t.Fatal("PostMerge was not called after recorded merged PR proof")
	}
	if mr.MergeCommit != "landing987" {
		t.Fatalf("MR merge commit = %q, want provider landing", mr.MergeCommit)
	}
	if rigGit.lookupRef.URL != mr.PRURL || rigGit.lookupRef.Number != mr.PRNumber || rigGit.lookupRef.HeadSHA != mr.CommitSHA {
		t.Fatalf("PR lookup ref = %+v, want recorded identity and submitted head", rigGit.lookupRef)
	}
	if len(rigGit.verifiedCommits) != 2 || rigGit.verifiedCommits[1] != "landing987" {
		t.Fatalf("verified commits = %v, want candidate and provider landing", rigGit.verifiedCommits)
	}
}

func TestRunVerifiedMQPostMerge_MergedPRMissingProviderLandingFailsClosed(t *testing.T) {
	mr := testMQPostMergeMR()
	mr.MergeCommit = "recorded-landing"
	mr.PRURL = "https://github.com/upstream/repo/pull/42"
	mr.PRNumber = 42
	rigPath := setupCompleteMQSourceForTest(t, mr)
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{
		verifyErrs: map[string]error{mr.CommitSHA: errors.New("candidate is not an ancestor")},
		patchErr:   errors.New("patch proof not used"),
		lookupResult: &git.PullRequestInfo{
			Number:         mr.PRNumber,
			URL:            mr.PRURL,
			State:          "MERGED",
			HeadRefName:    mr.Branch,
			HeadSHA:        mr.CommitSHA,
			BaseRefName:    mr.TargetBranch,
			MergeCommitSHA: "",
		},
	}

	_, _, err := runVerifiedMQPostMerge(mgr, rigPath, rigGit, mr.ID, false)
	if err == nil || !strings.Contains(err.Error(), "provider landing") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want missing provider landing failure", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called without provider landing SHA")
	}
	if mr.MergeCommit != "recorded-landing" {
		t.Fatalf("MR merge commit persisted after missing provider metadata: %q", mr.MergeCommit)
	}
	if len(rigGit.deletedBranches) != 0 || len(rigGit.localDeleted) != 0 {
		t.Fatalf("branch deleted without provider landing SHA: remote=%v local=%v", rigGit.deletedBranches, rigGit.localDeleted)
	}
}

func TestRunVerifiedMQPostMerge_MergedPRConflictingLandingFailsClosed(t *testing.T) {
	mr := testMQPostMergeMR()
	mr.MergeCommit = "recorded-landing"
	mr.PRURL = "https://github.com/upstream/repo/pull/42"
	mr.PRNumber = 42
	rigPath := setupCompleteMQSourceForTest(t, mr)
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{
		verifyErrs: map[string]error{
			mr.CommitSHA:   errors.New("candidate is not an ancestor"),
			mr.MergeCommit: errors.New("recorded landing is not on target"),
		},
		patchErr: errors.New("patch proof not used"),
		lookupResult: &git.PullRequestInfo{
			Number:         mr.PRNumber,
			URL:            mr.PRURL,
			State:          "MERGED",
			HeadRefName:    mr.Branch,
			HeadSHA:        mr.CommitSHA,
			BaseRefName:    mr.TargetBranch,
			MergeCommitSHA: "provider-landing",
		},
	}

	_, _, err := runVerifiedMQPostMerge(mgr, rigPath, rigGit, mr.ID, false)
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want conflicting landing failure", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called with conflicting landing SHAs")
	}
	if mr.MergeCommit != "recorded-landing" {
		t.Fatalf("MR merge commit mutated after conflicting provider metadata: %q", mr.MergeCommit)
	}
	if len(rigGit.deletedBranches) != 0 || len(rigGit.localDeleted) != 0 {
		t.Fatalf("branch deleted with conflicting landing SHAs: remote=%v local=%v", rigGit.deletedBranches, rigGit.localDeleted)
	}
}

func TestRunVerifiedMQPostMerge_MergedPRUnreachableProviderLandingFailsClosed(t *testing.T) {
	mr := testMQPostMergeMR()
	mr.PRURL = "https://github.com/upstream/repo/pull/42"
	mr.PRNumber = 42
	rigPath := setupCompleteMQSourceForTest(t, mr)
	const providerLanding = "provider-landing"
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{
		verifyErrs: map[string]error{
			mr.CommitSHA:    errors.New("candidate is not an ancestor"),
			providerLanding: errors.New("provider landing is unreachable"),
		},
		patchErr: errors.New("patch proof not used"),
		lookupResult: &git.PullRequestInfo{
			Number:         mr.PRNumber,
			URL:            mr.PRURL,
			State:          "MERGED",
			HeadRefName:    mr.Branch,
			HeadSHA:        mr.CommitSHA,
			BaseRefName:    mr.TargetBranch,
			MergeCommitSHA: providerLanding,
		},
	}

	_, _, err := runVerifiedMQPostMerge(mgr, rigPath, rigGit, mr.ID, false)
	if err == nil || !strings.Contains(err.Error(), "not on target") {
		t.Fatalf("runVerifiedMQPostMerge error = %v, want unreachable provider landing failure", err)
	}
	if mgr.postMergeCalled {
		t.Fatal("PostMerge called for unreachable provider landing")
	}
	if mr.MergeCommit != "" {
		t.Fatalf("MR merge commit persisted after unreachable provider metadata: %q", mr.MergeCommit)
	}
	if len(rigGit.deletedBranches) != 0 || len(rigGit.localDeleted) != 0 {
		t.Fatalf("branch deleted for unreachable provider landing: remote=%v local=%v", rigGit.deletedBranches, rigGit.localDeleted)
	}
}
