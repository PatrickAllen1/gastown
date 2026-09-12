package refinery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/runtime"
)

type terminalMRCloseOptions struct {
	Reason        string
	MergeCommit   string
	AgentBeadHint string
	MissingOK     bool
	ExpectedMR    *MergeRequest
	// afterReload is a test-only seam used to inject a concurrent durable edit
	// after the transaction has reloaded the MR and before it persists changes.
	afterReload func() error
}

type terminalMRCloseResult struct {
	MRID                  string
	SourceIssue           string
	AgentBead             string
	Closed                bool
	AlreadyTerminal       bool
	AgentActiveMRCleared  bool
	AgentActiveMRClearErr error
}

func closeTerminalMR(b *beads.Beads, mrID string, opts terminalMRCloseOptions) (*terminalMRCloseResult, error) {
	mrID = strings.TrimSpace(mrID)
	result := &terminalMRCloseResult{MRID: mrID}
	if b == nil || mrID == "" {
		return result, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := b.Store()
	cleanupStore := func() {}
	if store == nil {
		var err error
		store, cleanupStore, err = b.OpenStore(ctx)
		if err != nil {
			// Manual rejection predates the in-process store and does not carry a
			// verified merge snapshot. Keep that legacy CLI-only path available
			// when no store can be opened; verified merge closes remain strictly
			// fail-closed on store-open or transaction errors.
			if opts.ExpectedMR == nil && strings.TrimSpace(opts.MergeCommit) == "" {
				return closeTerminalMRViaBeads(b, mrID, opts)
			}
			return result, fmt.Errorf("open beads store for terminal MR close: %w", err)
		}
	}
	defer cleanupStore()

	actor := os.Getenv("BD_ACTOR")
	sessionID := runtime.SessionIDFromEnv()
	var committedResult terminalMRCloseResult
	var transactionIssueMissing bool
	err, transactionPanicked := runTerminalMRTransaction(store, ctx, fmt.Sprintf("gt: close MR %s", mrID), func(tx beadsdk.Transaction) error {
		transactionIssueMissing = false
		attemptResult := terminalMRCloseResult{MRID: mrID}
		issue, err := tx.GetIssue(ctx, mrID)
		if err != nil {
			if opts.MissingOK && strings.Contains(strings.ToLower(err.Error()), "not found") {
				transactionIssueMissing = true
				return nil
			}
			return fmt.Errorf("fetch MR for close: %w", err)
		}
		if issue == nil {
			transactionIssueMissing = true
			return nil
		}
		if opts.afterReload != nil {
			if err := opts.afterReload(); err != nil {
				return fmt.Errorf("after terminal MR reload: %w", err)
			}
		}

		internalIssue := &beads.Issue{
			ID:          issue.ID,
			Description: issue.Description,
			Status:      string(issue.Status),
		}
		fields := beads.ParseMRFields(internalIssue)
		if fields == nil {
			fields = &beads.MRFields{}
		}
		attemptResult.SourceIssue = strings.TrimSpace(fields.SourceIssue)
		attemptResult.AgentBead = firstNonEmpty(opts.AgentBeadHint, fields.AgentBead)
		if err := validateTerminalMRCloseSnapshot(mrID, fields, opts.ExpectedMR); err != nil {
			return err
		}

		status := beads.IssueStatus(strings.TrimSpace(string(issue.Status)))
		verifiedMergeCommit := strings.TrimSpace(opts.MergeCommit)
		if verifiedMergeCommit != "" {
			storedMergeCommit := strings.TrimSpace(fields.MergeCommit)
			switch {
			case storedMergeCommit == "":
				fields.MergeCommit = verifiedMergeCommit
			case storedMergeCommit == verifiedMergeCommit:
			default:
				return fmt.Errorf("MR %s changed after merge proof: merge_commit=%q, verified %q", mrID, storedMergeCommit, verifiedMergeCommit)
			}
		}

		switch {
		case status == beads.StatusOpen:
			if closeReason := normalizedMRCloseReason(opts.Reason); closeReason != "" {
				fields.CloseReason = closeReason
			}
			if attemptResult.AgentBead != "" && strings.TrimSpace(fields.AgentBead) == "" {
				fields.AgentBead = attemptResult.AgentBead
			}

			newDesc := beads.SetMRFields(internalIssue, fields)
			if err := tx.UpdateIssue(ctx, mrID, map[string]interface{}{"description": newDesc}, actor); err != nil {
				return fmt.Errorf("record MR close metadata: %w", err)
			}
			if err := tx.CloseIssue(ctx, mrID, opts.Reason, actor, sessionID); err != nil {
				return fmt.Errorf("close MR: %w", err)
			}
			attemptResult.Closed = true
		case status.IsTerminal():
			newDesc := beads.SetMRFields(internalIssue, fields)
			if verifiedMergeCommit != "" && newDesc != internalIssue.Description {
				if err := tx.UpdateIssue(ctx, mrID, map[string]interface{}{"description": newDesc}, actor); err != nil {
					return fmt.Errorf("record MR close metadata: %w", err)
				}
			}
			attemptResult.AlreadyTerminal = true
		default:
			committedResult = attemptResult
			return nil
		}
		committedResult = attemptResult
		return nil
	})
	if err != nil {
		if transactionPanicked && opts.ExpectedMR == nil && strings.TrimSpace(opts.MergeCommit) == "" {
			return closeTerminalMRViaBeads(b, mrID, opts)
		}
		return result, err
	}
	if transactionIssueMissing && opts.ExpectedMR == nil && strings.TrimSpace(opts.MergeCommit) == "" {
		return closeTerminalMRViaBeads(b, mrID, opts)
	}
	*result = committedResult

	if result.AgentBead != "" {
		cleared, clearErr := b.ForAgentBead().ClearAgentActiveMRIfMatches(result.AgentBead, mrID)
		result.AgentActiveMRCleared = cleared
		result.AgentActiveMRClearErr = clearErr
	}
	return result, nil
}

func runTerminalMRTransaction(store beadsdk.Storage, ctx context.Context, commitMessage string, fn func(beadsdk.Transaction) error) (err error, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("terminal MR transaction failed: %v", recovered)
			panicked = true
		}
	}()
	return store.RunInTransaction(ctx, commitMessage, fn), false
}

// closeTerminalMRViaBeads handles unverified manual rejection when the
// configured beads directory has no transaction-capable store. Verified merge
// cleanup never uses this compatibility path.
func closeTerminalMRViaBeads(b *beads.Beads, mrID string, opts terminalMRCloseOptions) (*terminalMRCloseResult, error) {
	result := &terminalMRCloseResult{MRID: mrID}
	issue, err := b.Show(mrID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) && opts.MissingOK {
			return result, nil
		}
		return result, fmt.Errorf("fetch MR for close: %w", err)
	}
	if issue == nil {
		return result, nil
	}

	fields := beads.ParseMRFields(issue)
	if fields == nil {
		fields = &beads.MRFields{}
	}
	result.SourceIssue = strings.TrimSpace(fields.SourceIssue)
	result.AgentBead = firstNonEmpty(opts.AgentBeadHint, fields.AgentBead)
	if err := validateTerminalMRCloseSnapshot(mrID, fields, opts.ExpectedMR); err != nil {
		return result, err
	}

	status := beads.IssueStatus(strings.TrimSpace(issue.Status))
	switch {
	case status == beads.StatusOpen:
		if closeReason := normalizedMRCloseReason(opts.Reason); closeReason != "" {
			fields.CloseReason = closeReason
		}
		if result.AgentBead != "" && strings.TrimSpace(fields.AgentBead) == "" {
			fields.AgentBead = result.AgentBead
		}
		newDesc := beads.SetMRFields(issue, fields)
		if err := b.Update(mrID, beads.UpdateOptions{Description: &newDesc}); err != nil {
			return result, fmt.Errorf("record MR close metadata: %w", err)
		}
		if err := b.CloseWithReason(opts.Reason, mrID); err != nil {
			return result, fmt.Errorf("close MR: %w", err)
		}
		result.Closed = true
	case status.IsTerminal():
		result.AlreadyTerminal = true
	default:
		return result, nil
	}

	if result.AgentBead != "" {
		cleared, clearErr := b.ForAgentBead().ClearAgentActiveMRIfMatches(result.AgentBead, mrID)
		result.AgentActiveMRCleared = cleared
		result.AgentActiveMRClearErr = clearErr
	}
	return result, nil
}

func validateTerminalMRCloseSnapshot(mrID string, fields *beads.MRFields, expected *MergeRequest) error {
	if expected == nil || fields == nil {
		return nil
	}
	checks := []struct {
		name string
		got  string
		want string
	}{
		{name: "branch", got: fields.Branch, want: expected.Branch},
		{name: "source_issue", got: fields.SourceIssue, want: expected.IssueID},
		{name: "commit_sha", got: fields.CommitSHA, want: expected.CommitSHA},
	}
	if strings.TrimSpace(expected.TargetBranch) != "" {
		checks = append(checks, struct {
			name string
			got  string
			want string
		}{name: "target", got: fields.Target, want: expected.TargetBranch})
	}
	for _, check := range checks {
		got := strings.TrimSpace(check.got)
		want := strings.TrimSpace(check.want)
		if want != "" && got != want {
			return fmt.Errorf("MR %s changed after merge proof: %s=%q, verified %q", mrID, check.name, got, want)
		}
	}
	storedMergeCommit := strings.TrimSpace(fields.MergeCommit)
	verifiedMergeCommit := strings.TrimSpace(expected.MergeCommit)
	if verifiedMergeCommit != "" && storedMergeCommit != "" && storedMergeCommit != verifiedMergeCommit {
		return fmt.Errorf("MR %s changed after merge proof: merge_commit=%q, verified %q", mrID, storedMergeCommit, verifiedMergeCommit)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
