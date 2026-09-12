package refinery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
)

func TestTerminalReviewSelectionAcceptsFullGtVd345MetadataAsInert(t *testing.T) {
	receipt := beads.ReviewReceiptV1{
		ReceiptVersion:   1,
		SourceIssue:      "gt-source-terminal-vd345",
		ReviewChildIssue: "gt-review-terminal-vd345",
		CandidateCommit:  strings.Repeat("a", 40),
		CandidateTree:    strings.Repeat("b", 40),
		TargetRef:        "refs/heads/main",
		Verdict:          beads.ReviewReceiptVerdictChangesRequired,
		ReviewModel:      "codex-sol-medium",
		ReviewProfile:    "medium",
	}
	comment := beads.Comment{
		ID:        "comment-terminal-vd345",
		IssueID:   receipt.ReviewChildIssue,
		Author:    "sweetpea/",
		Text:      beads.FormatReviewReceiptV1(receipt),
		CreatedAt: "2026-09-12T10:00:00Z",
	}
	child := &beads.Issue{
		ID:     receipt.ReviewChildIssue,
		Parent: receipt.SourceIssue,
		Status: "closed",
		Metadata: json.RawMessage(`{
"actual_handle":"/root/gt_vd3_4_r4_sol_review_sweetpea",
"actual_handle_status":"TERMINAL_COMPLETED",
"actual_model":"codex-sol-medium",
"candidate_commit":"c3324c9dcfe189ed4a3d69987afa4c13ff76a3f9",
"candidate_diff_sha256":"9f7418629c7c811eeb234e8f493ad8bdfbe6ee5cb3acd2f196433d2bae068c23",
"candidate_parent":"ac9bff2052a47b38598d4e25a99bf339b4997441",
"candidate_paths_sha256":"64d0247caa6095b11dae9886be80a2053b3ae87fd80a2a27dfa77d22ec2a9684",
"candidate_tree":"1a80fc2555cd9268d2b844e52b6406f5af44cb83",
"current_private_main":"ac9bff2052a47b38598d4e25a99bf339b4997441",
"current_private_main_tree":"ef8956ba6387b551a7b5c6fa41ee408626bd7ecd",
"gate_status":"ALL_REQUIRED_GATES_PASS_ONE_BLOCKER",
"landing_authorized":false,
"mutation_authorized":false,
"phase":"TERMINAL_CHANGES_REQUIRED",
"review_handle":"/root/gt_vd3_4_r4_sol_review_sweetpea",
"review_model":"codex-sol-medium",
"review_status":"CHANGES_REQUIRED",
"reviewer_active":false,
"terminal_verdict":"CHANGES_REQUIRED"
}`),
		Comments: []beads.Comment{comment},
	}
	ctx := beads.ReviewReceiptValidationContext{
		SourceIssue:      &beads.Issue{ID: receipt.SourceIssue, Status: "open"},
		ReviewChildIssue: child,
		CandidateCommit:  receipt.CandidateCommit,
		CandidateTree:    receipt.CandidateTree,
		TargetRef:        receipt.TargetRef,
		FrozenAt:         mustTerminalReviewTime(t, "2026-09-12T09:00:00Z"),
		Author:           "author/",
	}
	current, err := beads.SelectCurrentReviewReceiptV1FromIssue(ctx)
	if err != nil {
		t.Fatalf("full gt-vd3.4.5 metadata rejected: %v", err)
	}
	if current.IsFinalPass() || current.Verdict != receipt.Verdict || current.CandidateCommit != receipt.CandidateCommit {
		t.Fatalf("selected receipt = %+v, want canonical typed Comment", current)
	}

	child.Metadata = json.RawMessage(`{"review_model":null}`)
	if _, err := beads.SelectCurrentReviewReceiptV1FromIssue(ctx); err == nil {
		t.Fatal("terminal review selection accepted malformed gt-vd3.4.5 metadata")
	}
}

func mustTerminalReviewTime(t *testing.T, raw string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("parse terminal review time: %v", err)
	}
	return parsed
}

func TestValidateTerminalMRCloseSnapshotRejectsDrift(t *testing.T) {
	expected := &MergeRequest{
		ID:           "gt-mr-proof",
		Branch:       "polecat/test/proof",
		IssueID:      "gt-proof",
		TargetBranch: "main",
		CommitSHA:    "abc123",
	}
	fields := &beads.MRFields{
		Branch:      "polecat/test/proof",
		SourceIssue: "gt-proof",
		Target:      "main",
		CommitSHA:   "def456",
	}

	err := validateTerminalMRCloseSnapshot(expected.ID, fields, expected)
	if err == nil || !strings.Contains(err.Error(), "changed after merge proof") {
		t.Fatalf("validateTerminalMRCloseSnapshot error = %v, want drift failure", err)
	}
}

func TestValidateTerminalMRCloseSnapshotAllowsMatchingSnapshot(t *testing.T) {
	expected := &MergeRequest{
		ID:           "gt-mr-proof",
		Branch:       "polecat/test/proof",
		IssueID:      "gt-proof",
		TargetBranch: "main",
		CommitSHA:    "abc123",
	}
	fields := &beads.MRFields{
		Branch:      "polecat/test/proof",
		SourceIssue: "gt-proof",
		Target:      "main",
		CommitSHA:   "abc123",
	}

	if err := validateTerminalMRCloseSnapshot(expected.ID, fields, expected); err != nil {
		t.Fatalf("validateTerminalMRCloseSnapshot: %v", err)
	}
}

func TestValidateTerminalMRCloseSnapshot_AllowsEmptyToVerifiedMergeCommit(t *testing.T) {
	expected := &MergeRequest{ID: "gt-mr-proof", MergeCommit: "provider-sha"}
	fields := &beads.MRFields{}
	if err := validateTerminalMRCloseSnapshot(expected.ID, fields, expected); err != nil {
		t.Fatalf("validateTerminalMRCloseSnapshot: %v", err)
	}
}

func TestValidateTerminalMRCloseSnapshot_AllowsEqualMergeCommit(t *testing.T) {
	expected := &MergeRequest{ID: "gt-mr-proof", MergeCommit: "provider-sha"}
	fields := &beads.MRFields{MergeCommit: "provider-sha"}
	if err := validateTerminalMRCloseSnapshot(expected.ID, fields, expected); err != nil {
		t.Fatalf("validateTerminalMRCloseSnapshot: %v", err)
	}
}

func TestValidateTerminalMRCloseSnapshot_RejectsDifferentMergeCommit(t *testing.T) {
	expected := &MergeRequest{ID: "gt-mr-proof", MergeCommit: "provider-sha"}
	fields := &beads.MRFields{MergeCommit: "other-sha"}
	err := validateTerminalMRCloseSnapshot(expected.ID, fields, expected)
	if err == nil || !strings.Contains(err.Error(), `merge_commit="other-sha", verified "provider-sha"`) {
		t.Fatalf("validateTerminalMRCloseSnapshot error = %v, want exact merge commit conflict", err)
	}
}

func TestCloseTerminalMR_ConcurrentMergeCommitConflictRollsBackBeforeLifecycle(t *testing.T) {
	const (
		mrID        = "gt-mr-terminal-race"
		sourceID    = "gt-source-terminal-race"
		agentID     = "gt-gastown-polecat-race"
		providerSHA = "provider-sha"
		conflictSHA = "concurrent-sha"
	)

	mrFields := &beads.MRFields{
		Branch:      "polecat/race/gt-source-terminal-race+proof123",
		Target:      "main",
		SourceIssue: sourceID,
		CommitSHA:   "submitted-sha",
		AgentBead:   agentID,
	}
	mrIssue := terminalTestIssue(mrID, beads.FormatMRFields(mrFields), "gt:merge-request")
	sourceIssue := terminalTestIssue(sourceID, "source", "gt:task")
	agentIssue := terminalTestIssue(agentID, beads.FormatAgentDescription("agent", &beads.AgentFields{
		RoleType:   "polecat",
		AgentState: "done",
		ActiveMR:   mrID,
	}), "gt:agent")
	store := newTerminalTestStore(mrIssue, sourceIssue, agentIssue)
	b := beads.NewWithStore(t.TempDir(), store)
	expected := &MergeRequest{
		ID:           mrID,
		Branch:       mrFields.Branch,
		IssueID:      sourceID,
		TargetBranch: mrFields.Target,
		CommitSHA:    mrFields.CommitSHA,
		MergeCommit:  providerSHA,
		AgentBead:    agentID,
	}
	var injectOnce sync.Once

	result, err := closeTerminalMR(b, mrID, terminalMRCloseOptions{
		Reason:        string(CloseReasonMerged),
		MergeCommit:   providerSHA,
		AgentBeadHint: agentID,
		ExpectedMR:    expected,
		afterReload: func() error {
			var injectedErr error
			injectOnce.Do(func() {
				injectedErr = store.RunInTransaction(context.Background(), "test concurrent merge metadata", func(tx beadsdk.Transaction) error {
					concurrent := beads.FormatMRFields(&beads.MRFields{
						Branch:      mrFields.Branch,
						Target:      mrFields.Target,
						SourceIssue: sourceID,
						CommitSHA:   mrFields.CommitSHA,
						AgentBead:   agentID,
						MergeCommit: conflictSHA,
					})
					return tx.UpdateIssue(context.Background(), mrID, map[string]interface{}{"description": concurrent}, "concurrent")
				})
			})
			return injectedErr
		},
	})
	if err == nil || !strings.Contains(err.Error(), `MR `+mrID+` changed after merge proof: merge_commit="`+conflictSHA+`", verified "`+providerSHA+`"`) {
		t.Fatalf("closeTerminalMR error = %v, want exact concurrent merge conflict", err)
	}
	if result.Closed || result.AgentActiveMRCleared {
		t.Fatalf("closeTerminalMR result = %+v, want lifecycle untouched", result)
	}

	gotMR := terminalTestStoreIssue(t, store, mrID)
	gotMRFields := beads.ParseMRFields(&beads.Issue{ID: gotMR.ID, Description: gotMR.Description})
	if gotMR.Status != beadsdk.StatusOpen {
		t.Fatalf("MR status = %q, want open", gotMR.Status)
	}
	if gotMRFields.MergeCommit != conflictSHA {
		t.Fatalf("recorded merge_commit = %q, want concurrent %s", gotMRFields.MergeCommit, conflictSHA)
	}
	if gotMRFields.MergeCommit == providerSHA {
		t.Fatal("provider merge SHA was persisted after concurrent conflict")
	}
	if gotSource := terminalTestStoreIssue(t, store, sourceID); gotSource.Status != beadsdk.StatusOpen {
		t.Fatalf("source status = %q, want open", gotSource.Status)
	}
	gotAgent := terminalTestStoreIssue(t, store, agentID)
	if fields := beads.ParseAgentFields(gotAgent.Description); fields.ActiveMR != mrID {
		t.Fatalf("agent active_mr = %q, want %s", fields.ActiveMR, mrID)
	}
}

type terminalTestStore struct {
	beadsdk.Storage
	mu      sync.Mutex
	issues  map[string]*beadsdk.Issue
	version uint64
}

type terminalTestTransaction struct {
	beadsdk.Transaction
	store  *terminalTestStore
	issues map[string]*beadsdk.Issue
}

func newTerminalTestStore(issues ...*beadsdk.Issue) *terminalTestStore {
	store := &terminalTestStore{issues: make(map[string]*beadsdk.Issue, len(issues))}
	for _, issue := range issues {
		store.issues[issue.ID] = cloneTerminalTestIssue(issue)
	}
	return store
}

func (s *terminalTestStore) GetIssue(_ context.Context, id string) (*beadsdk.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	issue, ok := s.issues[id]
	if !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return cloneTerminalTestIssue(issue), nil
}

func (s *terminalTestStore) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return updateTerminalTestIssue(s.issues, id, updates)
}

func (s *terminalTestStore) CloseIssue(_ context.Context, id, _ string, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	issue, ok := s.issues[id]
	if !ok {
		return fmt.Errorf("issue %s not found", id)
	}
	issue.Status = beadsdk.StatusClosed
	now := time.Now()
	issue.ClosedAt = &now
	issue.UpdatedAt = now
	s.version++
	return nil
}

func (s *terminalTestStore) RunInTransaction(_ context.Context, _ string, fn func(beadsdk.Transaction) error) error {
	for attempt := 0; attempt < 3; attempt++ {
		s.mu.Lock()
		baseVersion := s.version
		working := make(map[string]*beadsdk.Issue, len(s.issues))
		for id, issue := range s.issues {
			working[id] = cloneTerminalTestIssue(issue)
		}
		s.mu.Unlock()

		tx := &terminalTestTransaction{store: s, issues: working}
		if err := fn(tx); err != nil {
			return err
		}

		s.mu.Lock()
		if s.version != baseVersion {
			s.mu.Unlock()
			continue
		}
		s.issues = working
		s.version++
		s.mu.Unlock()
		return nil
	}
	return fmt.Errorf("terminal test transaction conflict after retries")
}

func (tx *terminalTestTransaction) GetIssue(_ context.Context, id string) (*beadsdk.Issue, error) {
	issue, ok := tx.issues[id]
	if !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return cloneTerminalTestIssue(issue), nil
}

func (tx *terminalTestTransaction) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
	return updateTerminalTestIssue(tx.issues, id, updates)
}

func (tx *terminalTestTransaction) CloseIssue(_ context.Context, id string, _ string, _, _ string) error {
	issue, ok := tx.issues[id]
	if !ok {
		return fmt.Errorf("issue %s not found", id)
	}
	issue.Status = beadsdk.StatusClosed
	now := time.Now()
	issue.ClosedAt = &now
	issue.UpdatedAt = now
	return nil
}

func updateTerminalTestIssue(issues map[string]*beadsdk.Issue, id string, updates map[string]interface{}) error {
	issue, ok := issues[id]
	if !ok {
		return fmt.Errorf("issue %s not found", id)
	}
	for key, value := range updates {
		switch key {
		case "description":
			issue.Description, _ = value.(string)
		case "status":
			status, _ := value.(string)
			issue.Status = beadsdk.Status(status)
		}
	}
	issue.UpdatedAt = time.Now()
	return nil
}

func terminalTestIssue(id, description, label string) *beadsdk.Issue {
	now := time.Now()
	return &beadsdk.Issue{
		ID:          id,
		Title:       id,
		Description: description,
		Status:      beadsdk.StatusOpen,
		IssueType:   beadsdk.IssueType("task"),
		Labels:      []string{label},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func cloneTerminalTestIssue(issue *beadsdk.Issue) *beadsdk.Issue {
	if issue == nil {
		return nil
	}
	copy := *issue
	copy.Labels = append([]string(nil), issue.Labels...)
	if issue.ClosedAt != nil {
		closedAt := *issue.ClosedAt
		copy.ClosedAt = &closedAt
	}
	return &copy
}

func terminalTestStoreIssue(t *testing.T, store *terminalTestStore, id string) *beadsdk.Issue {
	t.Helper()
	issue, err := store.GetIssue(context.Background(), id)
	if err != nil {
		t.Fatalf("get terminal test issue %s: %v", id, err)
	}
	return issue
}
