package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery"
)

type fakeMQPostMergeManager struct {
	mr              *refinery.MergeRequest
	findErr         error
	postMergeErr    error
	sourceErr       error
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

func (m *fakeMQPostMergeManager) VerifyPostMergeSourceIssue(mr *refinery.MergeRequest) error {
	if m.sourceErr != nil {
		return m.sourceErr
	}
	if mr == nil || strings.TrimSpace(mr.IssueID) == "" {
		return errors.New("missing source issue")
	}
	if branchIssue := strings.TrimSpace(parseBranchName(mr.Branch).Issue); branchIssue != "" && branchIssue != strings.TrimSpace(mr.IssueID) {
		return errors.New("source issue does not own branch")
	}
	return nil
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
		Branch:       "polecat/test/gt-proof",
		Worker:       "polecats/test",
		IssueID:      "gt-proof",
		TargetBranch: "main",
		CommitSHA:    "abc123def456",
	}
}

func TestRunVerifiedMQPostMerge_ProofFailurePreservesRecordsAndBranch(t *testing.T) {
	mgr := &fakeMQPostMergeManager{mr: testMQPostMergeMR()}
	rigGit := &fakeMQPostMergeGit{
		verifyErr: errors.New("not reachable"),
		patchErr:  errors.New("not preserved"),
	}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mgr.mr.ID, false)
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
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{
		verifyErrs: map[string]error{mr.CommitSHA: errors.New("candidate is not an ancestor")},
		remoteTip:  mr.CommitSHA,
		localHead:  mr.CommitSHA,
	}

	_, cleanup, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mr.ID, false)
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
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{
		verifyErrs: map[string]error{mr.CommitSHA: errors.New("candidate is not an ancestor")},
		remoteTip:  mr.CommitSHA,
		localHead:  mr.CommitSHA,
	}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mr.ID, true)
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
	mgr := &fakeMQPostMergeManager{mr: mr}
	rigGit := &fakeMQPostMergeGit{
		verifyErrs: map[string]error{mr.CommitSHA: errors.New("candidate is not an ancestor")},
		patchErr:   errors.New("submitted patch is not preserved"),
	}

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mr.ID, false)
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

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mr.ID, true)
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

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mr.ID, false)
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

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mr.ID, false)
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

	_, _, err := runVerifiedMQPostMerge(mgr, t.TempDir(), rigGit, mr.ID, false)
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
