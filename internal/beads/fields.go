// Package beads provides field parsing utilities for structured issue descriptions.
package beads

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Note: AgentFields, ParseAgentFields, FormatAgentDescription, and CreateAgentBead are in beads.go

// AttachmentFields holds the attachment info for pinned beads.
// These fields track which molecule is attached to a handoff/pinned bead.
type AttachmentFields struct {
	AttachedMolecule string   // Root issue ID of the attached molecule
	AttachedFormula  string   // Formula name (e.g., "mol-polecat-work") for inline step display
	AttachedAt       string   // ISO 8601 timestamp when attached
	AttachedArgs     string   // Natural language args passed via gt sling --args (no-tmux mode)
	AttachedVars     []string // Formula variables passed via gt sling --var
	DispatchedBy     string   // Agent ID that dispatched this work (for completion notification)
	NoMerge          bool     // If true, gt done skips merge queue (for upstream PRs/human review)
	ReviewOnly       bool     // If true, assignee must evaluate and report back — no merge/commit/push
	Mode             string   // Execution mode: "" (normal) or "ralph" (Ralph Wiggum loop)
	ConvoyID         string   // Convoy bead ID tracking this issue (e.g., "hq-cv-abc")
	MergeStrategy    string   // Convoy merge strategy: "direct", "mr", "local", or "" (default = mr)
	ConvoyOwned      bool     // If true, convoy has gt:owned label (caller-managed lifecycle)
	FormulaVars      string   // Newline-separated key=value pairs for formula template substitution
}

// ParseAttachmentFields extracts attachment fields from an issue's description.
// Fields are expected as "key: value" lines. Returns nil if no attachment fields found.
func ParseAttachmentFields(issue *Issue) *AttachmentFields {
	if issue == nil || issue.Description == "" {
		return nil
	}

	fields := &AttachmentFields{}
	hasFields := false
	var formulaVars []string

	for _, line := range strings.Split(issue.Description, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Look for "key: value" pattern
		colonIdx := strings.Index(line, ":")
		if colonIdx == -1 {
			continue
		}

		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])
		if value == "" {
			continue
		}

		// Map keys to fields (case-insensitive)
		switch strings.ToLower(key) {
		case "attached_molecule", "attached-molecule", "attachedmolecule":
			fields.AttachedMolecule = value
			hasFields = true
		case "attached_formula", "attached-formula", "attachedformula":
			fields.AttachedFormula = value
			hasFields = true
		case "attached_at", "attached-at", "attachedat":
			fields.AttachedAt = value
			hasFields = true
		case "attached_args", "attached-args", "attachedargs":
			fields.AttachedArgs = value
			hasFields = true
		case "attached_vars", "attached-vars", "attachedvars":
			fields.AttachedVars = parseAttachedVars(value)
			hasFields = true
		case "dispatched_by", "dispatched-by", "dispatchedby":
			fields.DispatchedBy = value
			hasFields = true
		case "no_merge", "no-merge", "nomerge":
			fields.NoMerge = strings.ToLower(value) == "true"
			hasFields = true
		case "review_only", "review-only", "reviewonly":
			fields.ReviewOnly = strings.ToLower(value) == "true"
			hasFields = true
		case "mode":
			fields.Mode = value
			hasFields = true
		case "convoy_id", "convoy-id", "convoyid", "convoy":
			fields.ConvoyID = value
			hasFields = true
		case "merge_strategy", "merge-strategy", "mergestrategy":
			fields.MergeStrategy = value
			hasFields = true
		case "convoy_owned", "convoy-owned", "convoyowned":
			fields.ConvoyOwned = strings.ToLower(value) == "true"
			hasFields = true
		case "formula_vars", "formula-vars", "formulavars":
			formulaVars = append(formulaVars, splitFormulaVars(parseFormulaVars(value))...)
			hasFields = true
		}
	}
	if len(formulaVars) > 0 {
		fields.FormulaVars = strings.Join(formulaVars, "\n")
	}

	if !hasFields {
		return nil
	}
	return fields
}

// FormatAttachmentFields formats AttachmentFields as a string suitable for an issue description.
// Only non-empty fields are included.
func FormatAttachmentFields(fields *AttachmentFields) string {
	if fields == nil {
		return ""
	}

	var lines []string

	if fields.AttachedMolecule != "" {
		lines = append(lines, "attached_molecule: "+fields.AttachedMolecule)
	}
	if fields.AttachedFormula != "" {
		lines = append(lines, "attached_formula: "+fields.AttachedFormula)
	}
	if fields.AttachedAt != "" {
		lines = append(lines, "attached_at: "+fields.AttachedAt)
	}
	if fields.AttachedArgs != "" {
		lines = append(lines, "attached_args: "+fields.AttachedArgs)
	}
	if len(fields.AttachedVars) > 0 {
		lines = append(lines, "attached_vars: "+formatAttachedVars(fields.AttachedVars))
	}
	if fields.DispatchedBy != "" {
		lines = append(lines, "dispatched_by: "+fields.DispatchedBy)
	}
	if fields.NoMerge {
		lines = append(lines, "no_merge: true")
	}
	if fields.ReviewOnly {
		lines = append(lines, "review_only: true")
	}
	if fields.Mode != "" {
		lines = append(lines, "mode: "+fields.Mode)
	}
	if fields.ConvoyID != "" {
		lines = append(lines, "convoy_id: "+fields.ConvoyID)
	}
	if fields.MergeStrategy != "" {
		lines = append(lines, "merge_strategy: "+fields.MergeStrategy)
	}
	if fields.ConvoyOwned {
		lines = append(lines, "convoy_owned: true")
	}
	if fields.FormulaVars != "" {
		if formatted := formatFormulaVars(fields.FormulaVars); formatted != "" {
			lines = append(lines, "formula_vars: "+formatted)
		}
	}

	return strings.Join(lines, "\n")
}

// SetAttachmentFields updates an issue's description with the given attachment fields.
// Existing attachment field lines are replaced; other content is preserved.
// Returns the new description string.
func SetAttachmentFields(issue *Issue, fields *AttachmentFields) string {
	// Known attachment field keys (lowercase)
	attachmentKeys := map[string]bool{
		"attached_molecule": true,
		"attached-molecule": true,
		"attachedmolecule":  true,
		"attached_formula":  true,
		"attached-formula":  true,
		"attachedformula":   true,
		"attached_at":       true,
		"attached-at":       true,
		"attachedat":        true,
		"attached_args":     true,
		"attached-args":     true,
		"attachedargs":      true,
		"attached_vars":     true,
		"attached-vars":     true,
		"attachedvars":      true,
		"dispatched_by":     true,
		"dispatched-by":     true,
		"dispatchedby":      true,
		"no_merge":          true,
		"no-merge":          true,
		"nomerge":           true,
		"review_only":       true,
		"review-only":       true,
		"reviewonly":        true,
		"mode":              true,
		"convoy_id":         true,
		"convoy-id":         true,
		"convoyid":          true,
		"convoy":            true,
		"merge_strategy":    true,
		"merge-strategy":    true,
		"mergestrategy":     true,
		"convoy_owned":      true,
		"convoy-owned":      true,
		"convoyowned":       true,
		"formula_vars":      true,
		"formula-vars":      true,
		"formulavars":       true,
	}

	// Collect non-attachment lines from existing description
	var otherLines []string
	if issue != nil && issue.Description != "" {
		for _, line := range strings.Split(issue.Description, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				// Preserve blank lines in content
				otherLines = append(otherLines, line)
				continue
			}

			// Check if this is an attachment field line
			colonIdx := strings.Index(trimmed, ":")
			if colonIdx == -1 {
				otherLines = append(otherLines, line)
				continue
			}

			key := strings.ToLower(strings.TrimSpace(trimmed[:colonIdx]))
			if !attachmentKeys[key] {
				otherLines = append(otherLines, line)
			}
			// Skip attachment field lines - they'll be replaced
		}
	}

	// Build new description: attachment fields first, then other content
	formatted := FormatAttachmentFields(fields)

	// Trim trailing blank lines from other content
	for len(otherLines) > 0 && strings.TrimSpace(otherLines[len(otherLines)-1]) == "" {
		otherLines = otherLines[:len(otherLines)-1]
	}
	// Trim leading blank lines from other content
	for len(otherLines) > 0 && strings.TrimSpace(otherLines[0]) == "" {
		otherLines = otherLines[1:]
	}

	if formatted == "" {
		return strings.Join(otherLines, "\n")
	}
	if len(otherLines) == 0 {
		return formatted
	}

	return formatted + "\n\n" + strings.Join(otherLines, "\n")
}

// ConvoyFields holds the structured fields for a convoy bead.
// These fields are stored as key: value lines in the issue description.
type ConvoyFields struct {
	Owner                string // Convoy owner address (e.g., "mayor/")
	Notify               string // Additional notification address
	Molecule             string // Associated molecule/swarm ID
	Merge                string // Merge strategy
	BaseBranch           string // Target branch for polecats (e.g., "feat/extraction-review")
	Watchers             string // Comma-separated mail notification addresses (added via gt convoy watch)
	NudgeWatchers        string // Comma-separated nudge notification addresses (added via gt convoy watch --nudge)
	CompletionNotifiedAt string // RFC3339 timestamp when completion notifications were claimed/sent
}

// ParseConvoyFields extracts convoy fields from an issue's description.
// Returns nil if no convoy fields found.
func ParseConvoyFields(issue *Issue) *ConvoyFields {
	if issue == nil || issue.Description == "" {
		return nil
	}

	fields := &ConvoyFields{}
	hasFields := false

	for _, line := range strings.Split(issue.Description, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		colonIdx := strings.Index(line, ":")
		if colonIdx == -1 {
			continue
		}

		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])
		if value == "" {
			continue
		}

		switch strings.ToLower(key) {
		case "owner":
			fields.Owner = value
			hasFields = true
		case "notify":
			fields.Notify = value
			hasFields = true
		case "molecule":
			fields.Molecule = value
			hasFields = true
		case "merge":
			fields.Merge = value
			hasFields = true
		case "base_branch", "base-branch", "basebranch":
			fields.BaseBranch = value
			hasFields = true
		case "watchers":
			fields.Watchers = value
			hasFields = true
		case "nudge_watchers", "nudge-watchers", "nudgewatchers":
			fields.NudgeWatchers = value
			hasFields = true
		case "completion_notified_at", "completion-notified-at", "completionnotifiedat":
			fields.CompletionNotifiedAt = value
			hasFields = true
		}
	}

	if !hasFields {
		return nil
	}
	return fields
}

// NotificationAddresses returns deduplicated mail notification addresses from convoy fields.
// Includes Owner, Notify, and all Watchers addresses.
func (f *ConvoyFields) NotificationAddresses() []string {
	if f == nil {
		return nil
	}
	seen := make(map[string]bool)
	var addrs []string
	for _, addr := range []string{f.Owner, f.Notify} {
		if addr != "" && !seen[addr] {
			addrs = append(addrs, addr)
			seen[addr] = true
		}
	}
	for _, addr := range splitWatchers(f.Watchers) {
		if addr != "" && !seen[addr] {
			addrs = append(addrs, addr)
			seen[addr] = true
		}
	}
	return addrs
}

// NudgeNotificationAddresses returns deduplicated nudge addresses from convoy fields.
func (f *ConvoyFields) NudgeNotificationAddresses() []string {
	if f == nil {
		return nil
	}
	seen := make(map[string]bool)
	var addrs []string
	for _, addr := range splitWatchers(f.NudgeWatchers) {
		if addr != "" && !seen[addr] {
			addrs = append(addrs, addr)
			seen[addr] = true
		}
	}
	return addrs
}

// AddWatcher adds a mail watcher address to the comma-separated Watchers field.
// Returns true if the address was added (false if already present).
func (f *ConvoyFields) AddWatcher(addr string) bool {
	existing := splitWatchers(f.Watchers)
	for _, w := range existing {
		if w == addr {
			return false
		}
	}
	existing = append(existing, addr)
	f.Watchers = strings.Join(existing, ",")
	return true
}

// AddNudgeWatcher adds a nudge watcher address to the comma-separated NudgeWatchers field.
// Returns true if the address was added (false if already present).
func (f *ConvoyFields) AddNudgeWatcher(addr string) bool {
	existing := splitWatchers(f.NudgeWatchers)
	for _, w := range existing {
		if w == addr {
			return false
		}
	}
	existing = append(existing, addr)
	f.NudgeWatchers = strings.Join(existing, ",")
	return true
}

// RemoveWatcher removes a mail watcher address. Returns true if it was present.
func (f *ConvoyFields) RemoveWatcher(addr string) bool {
	existing := splitWatchers(f.Watchers)
	var remaining []string
	found := false
	for _, w := range existing {
		if w == addr {
			found = true
		} else {
			remaining = append(remaining, w)
		}
	}
	if found {
		f.Watchers = strings.Join(remaining, ",")
	}
	return found
}

// RemoveNudgeWatcher removes a nudge watcher address. Returns true if it was present.
func (f *ConvoyFields) RemoveNudgeWatcher(addr string) bool {
	existing := splitWatchers(f.NudgeWatchers)
	var remaining []string
	found := false
	for _, w := range existing {
		if w == addr {
			found = true
		} else {
			remaining = append(remaining, w)
		}
	}
	if found {
		f.NudgeWatchers = strings.Join(remaining, ",")
	}
	return found
}

// splitWatchers splits a comma-separated watcher string into trimmed, non-empty addresses.
func splitWatchers(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// FormatConvoyFields formats ConvoyFields as a string suitable for an issue description.
// Only non-empty fields are included.
func FormatConvoyFields(fields *ConvoyFields) string {
	if fields == nil {
		return ""
	}

	var lines []string
	if fields.Owner != "" {
		lines = append(lines, "Owner: "+fields.Owner)
	}
	if fields.Notify != "" {
		lines = append(lines, "Notify: "+fields.Notify)
	}
	if fields.Merge != "" {
		lines = append(lines, "Merge: "+fields.Merge)
	}
	if fields.Molecule != "" {
		lines = append(lines, "Molecule: "+fields.Molecule)
	}
	if fields.BaseBranch != "" {
		lines = append(lines, "base_branch: "+fields.BaseBranch)
	}
	if fields.Watchers != "" {
		lines = append(lines, "Watchers: "+fields.Watchers)
	}
	if fields.NudgeWatchers != "" {
		lines = append(lines, "nudge_watchers: "+fields.NudgeWatchers)
	}
	if fields.CompletionNotifiedAt != "" {
		lines = append(lines, "completion_notified_at: "+fields.CompletionNotifiedAt)
	}

	return strings.Join(lines, "\n")
}

func formatAttachedVars(vars []string) string {
	if len(vars) == 0 {
		return ""
	}
	encoded, err := json.Marshal(vars)
	if err != nil {
		return strings.Join(vars, ", ")
	}
	return string(encoded)
}

func parseAttachedVars(raw string) []string {
	if raw == "" {
		return nil
	}
	var vars []string
	if strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &vars); err == nil {
			return vars
		}
	}
	return []string{raw}
}

func formatFormulaVars(raw string) string {
	return formatAttachedVars(splitFormulaVars(raw))
}

func parseFormulaVars(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "[") {
		if vars := parseAttachedVars(raw); len(vars) > 0 {
			return strings.Join(vars, "\n")
		}
		return ""
	}
	return strings.Join(splitFormulaVars(raw), "\n")
}

func splitFormulaVars(raw string) []string {
	if raw == "" {
		return nil
	}
	vars := strings.Split(raw, "\n")
	out := vars[:0]
	for _, variable := range vars {
		variable = strings.TrimSpace(variable)
		if variable != "" {
			out = append(out, variable)
		}
	}
	return out
}

// SetConvoyFields updates an issue's description with the given convoy fields.
// Existing convoy field lines are replaced; other content is preserved.
// Returns the new description string.
func SetConvoyFields(issue *Issue, fields *ConvoyFields) string {
	if issue == nil {
		return FormatConvoyFields(fields)
	}

	// Known convoy field keys (lowercase)
	convoyKeys := map[string]bool{
		"owner":                  true,
		"notify":                 true,
		"merge":                  true,
		"molecule":               true,
		"base_branch":            true,
		"base-branch":            true,
		"basebranch":             true,
		"watchers":               true,
		"nudge_watchers":         true,
		"nudge-watchers":         true,
		"nudgewatchers":          true,
		"completion_notified_at": true,
		"completion-notified-at": true,
		"completionnotifiedat":   true,
	}

	// Collect non-convoy lines from existing description
	var otherLines []string
	if issue.Description != "" {
		for _, line := range strings.Split(issue.Description, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				otherLines = append(otherLines, line)
				continue
			}

			colonIdx := strings.Index(trimmed, ":")
			if colonIdx == -1 {
				otherLines = append(otherLines, line)
				continue
			}

			key := strings.ToLower(strings.TrimSpace(trimmed[:colonIdx]))
			if !convoyKeys[key] {
				otherLines = append(otherLines, line)
			}
		}
	}

	// Build new description: other content first, then convoy fields
	formatted := FormatConvoyFields(fields)

	// Trim trailing blank lines from other content
	for len(otherLines) > 0 && strings.TrimSpace(otherLines[len(otherLines)-1]) == "" {
		otherLines = otherLines[:len(otherLines)-1]
	}
	// Trim leading blank lines from other content
	for len(otherLines) > 0 && strings.TrimSpace(otherLines[0]) == "" {
		otherLines = otherLines[1:]
	}

	if len(otherLines) == 0 {
		return formatted
	}
	if formatted == "" {
		return strings.Join(otherLines, "\n")
	}

	return strings.Join(otherLines, "\n") + "\n" + formatted
}

// MRFields holds the structured fields for a merge-request issue.
// These fields are stored as key: value lines in the issue description.
type MRFields struct {
	Branch      string // Source branch name (e.g., "polecat/Nux/gt-xyz")
	Target      string // Target branch (e.g., "main" or "integration/gt-epic")
	SourceIssue string // The work item being merged (e.g., "gt-xyz")
	Worker      string // Who did the work
	Rig         string // Which rig
	CommitSHA   string // HEAD commit SHA at submission time (GH#3032: dedup key)
	PRURL       string // Recorded pull request URL, if one exists for this MR
	PRNumber    int    // Recorded pull request number, scoped to the target repo
	MergeCommit string // SHA of merge commit (set on close)
	CloseReason string // Reason for closing: merged, rejected, conflict, superseded
	AgentBead   string // Agent bead ID that created this MR (for traceability)

	// Conflict resolution fields (for priority scoring)
	RetryCount      int    // Number of conflict-resolution cycles
	LastConflictSHA string // SHA of main when conflict occurred
	ConflictTaskID  string // Link to conflict-resolution task (if any)

	// Convoy tracking (for priority scoring - convoy starvation prevention)
	ConvoyID        string // Parent convoy ID if part of a convoy
	ConvoyCreatedAt string // Convoy creation time (ISO 8601) for starvation prevention

	// Pre-verification fields (Phase 3: polecat-owned rebasing)
	// When a polecat rebases onto the target and runs gates before submission,
	// these fields allow the refinery to fast-path merge without re-running gates.
	PreVerified     bool   // Polecat ran full gates after rebasing onto target
	PreVerifiedAt   string // ISO 8601 timestamp when verification completed
	PreVerifiedBase string // Target branch SHA at verification time
}

// ParseMRFields extracts structured merge-request fields from an issue's description.
// Fields are expected as "key: value" lines, with optional prose text mixed in.
// Returns nil if no MR fields are found.
func ParseMRFields(issue *Issue) *MRFields {
	if issue == nil || issue.Description == "" {
		return nil
	}

	fields := &MRFields{}
	hasFields := false

	for _, line := range strings.Split(issue.Description, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Look for "key: value" pattern
		colonIdx := strings.Index(line, ":")
		if colonIdx == -1 {
			continue
		}

		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])
		if value == "" || strings.EqualFold(value, "null") {
			continue
		}

		// Map keys to fields (case-insensitive)
		switch strings.ToLower(key) {
		case "branch":
			fields.Branch = value
			hasFields = true
		case "target":
			fields.Target = value
			hasFields = true
		case "source_issue", "source-issue", "sourceissue":
			fields.SourceIssue = value
			hasFields = true
		case "worker":
			fields.Worker = value
			hasFields = true
		case "rig":
			fields.Rig = value
			hasFields = true
		case "commit_sha", "commit-sha", "commitsha":
			fields.CommitSHA = value
			hasFields = true
		case "pr_url", "pr-url", "prurl":
			fields.PRURL = value
			hasFields = true
		case "pr_number", "pr-number", "prnumber":
			if n, err := parseIntField(value); err == nil {
				fields.PRNumber = n
				hasFields = true
			}
		case "merge_commit", "merge-commit", "mergecommit":
			fields.MergeCommit = value
			hasFields = true
		case "close_reason", "close-reason", "closereason":
			fields.CloseReason = value
			hasFields = true
		case "agent_bead", "agent-bead", "agentbead":
			fields.AgentBead = value
			hasFields = true
		case "retry_count", "retry-count", "retrycount":
			if n, err := parseIntField(value); err == nil {
				fields.RetryCount = n
				hasFields = true
			}
		case "last_conflict_sha", "last-conflict-sha", "lastconflictsha":
			fields.LastConflictSHA = value
			hasFields = true
		case "conflict_task_id", "conflict-task-id", "conflicttaskid":
			fields.ConflictTaskID = value
			hasFields = true
		case "convoy_id", "convoy-id", "convoyid", "convoy":
			fields.ConvoyID = value
			hasFields = true
		case "convoy_created_at", "convoy-created-at", "convoycreatedat":
			fields.ConvoyCreatedAt = value
			hasFields = true
		case "pre_verified", "pre-verified", "preverified":
			fields.PreVerified = strings.ToLower(value) == "true"
			hasFields = true
		case "pre_verified_at", "pre-verified-at", "preverifiedat":
			fields.PreVerifiedAt = value
			hasFields = true
		case "pre_verified_base", "pre-verified-base", "preverifiedbase":
			fields.PreVerifiedBase = value
			hasFields = true
		}
	}

	if !hasFields {
		return nil
	}
	return fields
}

// parseIntField parses an integer from a string, returning 0 on error.
func parseIntField(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// FormatMRFields formats MRFields as a string suitable for an issue description.
// Only non-empty fields are included.
func FormatMRFields(fields *MRFields) string {
	if fields == nil {
		return ""
	}

	var lines []string

	if fields.Branch != "" {
		lines = append(lines, "branch: "+fields.Branch)
	}
	if fields.Target != "" {
		lines = append(lines, "target: "+fields.Target)
	}
	if fields.SourceIssue != "" {
		lines = append(lines, "source_issue: "+fields.SourceIssue)
	}
	if fields.Worker != "" {
		lines = append(lines, "worker: "+fields.Worker)
	}
	if fields.Rig != "" {
		lines = append(lines, "rig: "+fields.Rig)
	}
	if fields.CommitSHA != "" {
		lines = append(lines, "commit_sha: "+fields.CommitSHA)
	}
	if fields.PRURL != "" {
		lines = append(lines, "pr_url: "+fields.PRURL)
	}
	if fields.PRNumber > 0 {
		lines = append(lines, fmt.Sprintf("pr_number: %d", fields.PRNumber))
	}
	if fields.MergeCommit != "" {
		lines = append(lines, "merge_commit: "+fields.MergeCommit)
	}
	if fields.CloseReason != "" {
		lines = append(lines, "close_reason: "+fields.CloseReason)
	}
	if fields.AgentBead != "" {
		lines = append(lines, "agent_bead: "+fields.AgentBead)
	}
	if fields.RetryCount > 0 {
		lines = append(lines, fmt.Sprintf("retry_count: %d", fields.RetryCount))
	}
	if fields.LastConflictSHA != "" {
		lines = append(lines, "last_conflict_sha: "+fields.LastConflictSHA)
	}
	if fields.ConflictTaskID != "" {
		lines = append(lines, "conflict_task_id: "+fields.ConflictTaskID)
	}
	if fields.ConvoyID != "" {
		lines = append(lines, "convoy_id: "+fields.ConvoyID)
	}
	if fields.ConvoyCreatedAt != "" {
		lines = append(lines, "convoy_created_at: "+fields.ConvoyCreatedAt)
	}
	if fields.PreVerified {
		lines = append(lines, "pre_verified: true")
	}
	if fields.PreVerifiedAt != "" {
		lines = append(lines, "pre_verified_at: "+fields.PreVerifiedAt)
	}
	if fields.PreVerifiedBase != "" {
		lines = append(lines, "pre_verified_base: "+fields.PreVerifiedBase)
	}

	return strings.Join(lines, "\n")
}

// SetMRFields updates an issue's description with the given MR fields.
// Existing MR field lines are replaced; other content is preserved.
// Returns the new description string.
func SetMRFields(issue *Issue, fields *MRFields) string {
	if issue == nil {
		return FormatMRFields(fields)
	}

	// Known MR field keys (lowercase)
	mrKeys := map[string]bool{
		"branch":            true,
		"target":            true,
		"source_issue":      true,
		"source-issue":      true,
		"sourceissue":       true,
		"worker":            true,
		"rig":               true,
		"commit_sha":        true,
		"commit-sha":        true,
		"commitsha":         true,
		"pr_url":            true,
		"pr-url":            true,
		"prurl":             true,
		"pr_number":         true,
		"pr-number":         true,
		"prnumber":          true,
		"merge_commit":      true,
		"merge-commit":      true,
		"mergecommit":       true,
		"close_reason":      true,
		"close-reason":      true,
		"closereason":       true,
		"agent_bead":        true,
		"agent-bead":        true,
		"agentbead":         true,
		"retry_count":       true,
		"retry-count":       true,
		"retrycount":        true,
		"last_conflict_sha": true,
		"last-conflict-sha": true,
		"lastconflictsha":   true,
		"conflict_task_id":  true,
		"conflict-task-id":  true,
		"conflicttaskid":    true,
		"convoy_id":         true,
		"convoy-id":         true,
		"convoyid":          true,
		"convoy":            true,
		"convoy_created_at": true,
		"convoy-created-at": true,
		"convoycreatedat":   true,
		"pre_verified":      true,
		"pre-verified":      true,
		"preverified":       true,
		"pre_verified_at":   true,
		"pre-verified-at":   true,
		"preverifiedat":     true,
		"pre_verified_base": true,
		"pre-verified-base": true,
		"preverifiedbase":   true,
	}

	// Collect non-MR lines from existing description
	var otherLines []string
	if issue.Description != "" {
		for _, line := range strings.Split(issue.Description, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				// Preserve blank lines in content
				otherLines = append(otherLines, line)
				continue
			}

			// Check if this is an MR field line
			colonIdx := strings.Index(trimmed, ":")
			if colonIdx == -1 {
				otherLines = append(otherLines, line)
				continue
			}

			key := strings.ToLower(strings.TrimSpace(trimmed[:colonIdx]))
			if !mrKeys[key] {
				otherLines = append(otherLines, line)
			}
			// Skip MR field lines - they'll be replaced
		}
	}

	// Build new description: MR fields first, then other content
	formatted := FormatMRFields(fields)

	// Trim trailing blank lines from other content
	for len(otherLines) > 0 && strings.TrimSpace(otherLines[len(otherLines)-1]) == "" {
		otherLines = otherLines[:len(otherLines)-1]
	}
	// Trim leading blank lines from other content
	for len(otherLines) > 0 && strings.TrimSpace(otherLines[0]) == "" {
		otherLines = otherLines[1:]
	}

	if formatted == "" {
		return strings.Join(otherLines, "\n")
	}
	if len(otherLines) == 0 {
		return formatted
	}

	return formatted + "\n\n" + strings.Join(otherLines, "\n")
}

// RoleConfig holds structured lifecycle configuration for role beads.
// These fields are stored as "key: value" lines in the role bead description.
// This enables agents to self-register their lifecycle configuration,
// replacing hardcoded identity string parsing in the daemon.
type RoleConfig struct {
	// SessionPattern defines how to derive tmux session name.
	// Supports placeholders: {rig}, {name}, {role}
	// Examples: "hq-mayor", "hq-deacon", "gt-{rig}-{role}", "gt-{rig}-{name}"
	SessionPattern string

	// WorkDirPattern defines the working directory relative to town root.
	// Supports placeholders: {town}, {rig}, {name}, {role}
	// Examples: "{town}", "{town}/{rig}", "{town}/{rig}/polecats/{name}"
	WorkDirPattern string

	// NeedsPreSync indicates whether workspace needs git sync before starting.
	// True for agents with persistent clones (refinery, crew, polecat).
	NeedsPreSync bool

	// StartCommand is the command to run after creating the session.
	// Default: "exec claude --dangerously-skip-permissions"
	StartCommand string

	// EnvVars are additional environment variables to set in the session.
	// Stored as "key=value" pairs.
	EnvVars map[string]string

	// Health check thresholds - per ZFC, agents control their own stuck detection.
	// These allow the Deacon's patrol config to be agent-defined rather than hardcoded.

	// PingTimeout is how long to wait for a health check response.
	// Format: duration string (e.g., "30s", "1m"). Default: 30s.
	PingTimeout string

	// ConsecutiveFailures is how many failed health checks before force-kill.
	// Default: 3.
	ConsecutiveFailures int

	// KillCooldown is the minimum time between force-kills of the same agent.
	// Format: duration string (e.g., "5m", "10m"). Default: 5m.
	KillCooldown string

	// StuckThreshold is how long a wisp can be in_progress before considered stuck.
	// Format: duration string (e.g., "1h", "30m"). Default: 1h.
	StuckThreshold string

	// WispTTLs maps wisp types to their TTL duration strings.
	// Stored as "wisp_ttl_<type>: <duration>" in the role bead description.
	// Examples: wisp_ttl_patrol: 48h, wisp_ttl_error: 336h, wisp_ttl_gc_report: 24h
	// These override rig config and hardcoded defaults for compaction policy.
	WispTTLs map[string]string
}

// ParseRoleConfig extracts RoleConfig from a role bead's description.
// Fields are expected as "key: value" lines. Returns nil if no config found.
func ParseRoleConfig(description string) *RoleConfig {
	config := &RoleConfig{
		EnvVars:  make(map[string]string),
		WispTTLs: make(map[string]string),
	}
	hasFields := false

	for _, line := range strings.Split(description, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		colonIdx := strings.Index(line, ":")
		if colonIdx == -1 {
			continue
		}

		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])
		if value == "" || value == "null" {
			continue
		}

		switch strings.ToLower(key) {
		case "session_pattern", "session-pattern", "sessionpattern":
			config.SessionPattern = value
			hasFields = true
		case "work_dir_pattern", "work-dir-pattern", "workdirpattern", "workdir_pattern":
			config.WorkDirPattern = value
			hasFields = true
		case "needs_pre_sync", "needs-pre-sync", "needspresync":
			config.NeedsPreSync = strings.ToLower(value) == "true"
			hasFields = true
		case "start_command", "start-command", "startcommand":
			config.StartCommand = value
			hasFields = true
		case "env_var", "env-var", "envvar":
			// Format: "env_var: KEY=VALUE"
			if eqIdx := strings.Index(value, "="); eqIdx != -1 {
				envKey := strings.TrimSpace(value[:eqIdx])
				envVal := strings.TrimSpace(value[eqIdx+1:])
				config.EnvVars[envKey] = envVal
				hasFields = true
			}
		// Health check threshold fields (ZFC: agent-controlled)
		case "ping_timeout", "ping-timeout", "pingtimeout":
			config.PingTimeout = value
			hasFields = true
		case "consecutive_failures", "consecutive-failures", "consecutivefailures":
			if n, err := parseIntField(value); err == nil {
				config.ConsecutiveFailures = n
				hasFields = true
			}
		case "kill_cooldown", "kill-cooldown", "killcooldown":
			config.KillCooldown = value
			hasFields = true
		case "stuck_threshold", "stuck-threshold", "stuckthreshold":
			config.StuckThreshold = value
			hasFields = true
		default:
			// Check for wisp_ttl_* pattern (e.g., wisp_ttl_patrol, wisp-ttl-error)
			lowerKey := strings.ToLower(key)
			if wispType, ok := ParseWispTTLKey(lowerKey); ok {
				config.WispTTLs[wispType] = value
				hasFields = true
			}
		}
	}

	if !hasFields {
		return nil
	}
	return config
}

// ParseWispTTLKey checks if a lowercase key matches the wisp_ttl_* pattern
// and returns the wisp type suffix. Supports underscore, hyphen, and camelCase variants.
// Examples: "wisp_ttl_patrol" → "patrol", "wisp-ttl-gc_report" → "gc_report"
func ParseWispTTLKey(key string) (string, bool) {
	for _, prefix := range []string{"wisp_ttl_", "wisp-ttl-", "wispttl"} {
		if strings.HasPrefix(key, prefix) {
			wispType := key[len(prefix):]
			if wispType != "" {
				return wispType, true
			}
		}
	}
	return "", false
}

// FormatRoleConfig formats RoleConfig as a string suitable for a role bead description.
// Only non-empty/non-default fields are included.
func FormatRoleConfig(config *RoleConfig) string {
	if config == nil {
		return ""
	}

	var lines []string

	if config.SessionPattern != "" {
		lines = append(lines, "session_pattern: "+config.SessionPattern)
	}
	if config.WorkDirPattern != "" {
		lines = append(lines, "work_dir_pattern: "+config.WorkDirPattern)
	}
	if config.NeedsPreSync {
		lines = append(lines, "needs_pre_sync: true")
	}
	if config.StartCommand != "" {
		lines = append(lines, "start_command: "+config.StartCommand)
	}
	for k, v := range config.EnvVars {
		lines = append(lines, "env_var: "+k+"="+v)
	}
	// Sort wisp TTL keys for deterministic output
	wispTypes := make([]string, 0, len(config.WispTTLs))
	for k := range config.WispTTLs {
		wispTypes = append(wispTypes, k)
	}
	sort.Strings(wispTypes)
	for _, wt := range wispTypes {
		lines = append(lines, "wisp_ttl_"+wt+": "+config.WispTTLs[wt])
	}

	return strings.Join(lines, "\n")
}

// ExpandRolePattern expands placeholders in a pattern string.
// Supported placeholders: {town}, {rig}, {name}, {role}, {prefix}
func ExpandRolePattern(pattern, townRoot, rig, name, role, prefix string) string {
	result := pattern
	result = strings.ReplaceAll(result, "{town}", townRoot)
	result = strings.ReplaceAll(result, "{rig}", rig)
	result = strings.ReplaceAll(result, "{name}", name)
	result = strings.ReplaceAll(result, "{role}", role)
	result = strings.ReplaceAll(result, "{prefix}", prefix)
	return result
}

// ReviewReceiptV1Schema identifies the only review receipt wire format that
// this package accepts. Review receipts deliberately live in ordinary Beads
// comments: the comment's stored Author, CreatedAt, and ID are the authority
// for reviewer identity, server time, and receipt identity respectively.
const ReviewReceiptV1Schema = "ReviewReceiptV1"

const (
	// ReviewReceiptV1Version is the integer version carried by a receipt body.
	ReviewReceiptV1Version = 1
	// ReviewReceiptVersionV1 is a descriptive alias for callers that prefer the
	// version-first naming used by other typed contracts.
	ReviewReceiptVersionV1 = ReviewReceiptV1Version
)

// ReviewReceiptVerdict is the closed set of review outcomes that can be
// persisted in a ReviewReceiptV1 comment.
type ReviewReceiptVerdict string

const (
	ReviewReceiptVerdictFinalPass       ReviewReceiptVerdict = "FINAL_PASS"
	ReviewReceiptVerdictChangesRequired ReviewReceiptVerdict = "CHANGES_REQUIRED"
	// Short aliases keep call sites readable while retaining one typed verdict.
	ReviewVerdictFinalPass       = ReviewReceiptVerdictFinalPass
	ReviewVerdictChangesRequired = ReviewReceiptVerdictChangesRequired
)

// ReviewReceiptV1 is the typed payload stored in a dedicated review-child
// Beads comment. Reviewer, server time, comment ID, and Text are derived
// values; they are intentionally never serialized into the comment body.
//
// The *ID, *SHA, Version, ReviewVerdict, Supersedes, Target, Model, and Profile
// fields are compatibility aliases for callers migrating from prose receipts.
// The formatter rejects conflicting aliases and emits only the canonical
// fields below.
type ReviewReceiptV1 struct {
	ReceiptVersion   int
	SourceIssue      string
	ReviewChildIssue string
	CandidateCommit  string
	CandidateTree    string
	TargetRef        string
	Verdict          ReviewReceiptVerdict
	ReviewModel      string
	ReviewProfile    string
	// SupersedesReceiptID is empty only for the first receipt; every replacement
	// names the exact prior Comment.ID so selection can reject detached history.
	SupersedesReceiptID string

	// Canonical comment authority, populated only by ParseReviewReceiptV1.
	Schema          string
	Reviewer        string
	ServerTime      time.Time
	ServerTimestamp time.Time
	Timestamp       time.Time
	CommentID       string
	ReceiptID       string
	Text            string

	// Input aliases. They are not additional wire fields.
	Version            int
	SourceIssueID      string
	ReviewChildIssueID string
	ReviewChild        string
	CandidateCommitSHA string
	CandidateTreeSHA   string
	Target             string
	ReviewVerdict      ReviewReceiptVerdict
	Model              string
	Profile            string
	Supersedes         string
}

// ReviewReceiptValidationContext binds a receipt to the immutable review
// situation that it claims to describe. A context is intentionally explicit:
// validation without source/child/candidate/tree/target/freeze authority is
// rejected rather than guessing from caller text.
type ReviewReceiptValidationContext struct {
	SourceIssue      *Issue
	ReviewChildIssue *Issue

	SourceIssueID      string
	ReviewChildIssueID string
	ReviewChild        string
	CandidateCommit    string
	CandidateTree      string
	TargetRef          string
	ExpectedTargetRef  string
	CandidateCommitSHA string
	CandidateTreeSHA   string

	FrozenAt          time.Time
	FreezeAt          time.Time
	CandidateFrozenAt time.Time
	FreezeTimestamp   string

	// Author is the implementation author. Coauthors and excluded reviewers
	// are all disallowed as the stored Comment.Author.
	Author               string
	AuthorIdentity       string
	ImplementationAuthor string
	Coauthors            []string
	Authors              []string
	AuthorIdentities     []string
	ExcludedReviewers    []string
	Excluded             []string
	ExclusionSet         []string

	// If any reachability set is supplied, the exact candidate must occur in
	// every supplied set. This prevents an unrelated target-reachable SHA from
	// being accepted as the reviewed candidate.
	ReachableCommits       []string
	TargetReachableCommits []string
	Reachable              map[string]bool
	TargetReachable        map[string]bool
	ReachableCommitSet     map[string]bool
	CommitTrees            map[string]string
}

// ReviewReceiptContext and ReviewReceiptSelectionContext are aliases for the
// same source-bound contract. Aliases avoid parallel, subtly different stores.
type ReviewReceiptContext = ReviewReceiptValidationContext
type ReviewReceiptSelectionContext = ReviewReceiptValidationContext

// CanonicalText returns the deterministic wire representation of the payload.
// Derived comment authority is intentionally excluded.
func (r ReviewReceiptV1) CanonicalText() string {
	return FormatReviewReceiptV1(r)
}

// IsFinalPass reports whether the typed verdict is FINAL_PASS.
func (r ReviewReceiptV1) IsFinalPass() bool {
	payload, err := r.canonicalPayload()
	return err == nil && payload.Verdict == ReviewReceiptVerdictFinalPass
}

// IsChangesRequired reports whether the typed verdict is CHANGES_REQUIRED.
func (r ReviewReceiptV1) IsChangesRequired() bool {
	payload, err := r.canonicalPayload()
	return err == nil && payload.Verdict == ReviewReceiptVerdictChangesRequired
}

// canonicalPayload resolves compatibility aliases into one payload and
// rejects conflicting duplicate representations before formatting or
// validation can proceed.
func (r ReviewReceiptV1) canonicalPayload() (ReviewReceiptV1, error) {
	var err error
	canonical := r
	canonical.Schema, err = reconcileReceiptString("schema", r.Schema, "")
	if err != nil {
		return ReviewReceiptV1{}, err
	}
	if canonical.Schema == "" {
		canonical.Schema = ReviewReceiptV1Schema
	}
	canonical.SourceIssue, err = reconcileReceiptString("source_issue", r.SourceIssue, r.SourceIssueID)
	if err != nil {
		return ReviewReceiptV1{}, err
	}
	canonical.ReviewChildIssue, err = reconcileReceiptString("review_child_issue", r.ReviewChildIssue, r.ReviewChildIssueID)
	if err != nil {
		return ReviewReceiptV1{}, err
	}
	canonical.ReviewChildIssue, err = reconcileReceiptString("review_child_issue", canonical.ReviewChildIssue, r.ReviewChild)
	if err != nil {
		return ReviewReceiptV1{}, err
	}
	canonical.CandidateCommit, err = reconcileReceiptString("candidate_commit", r.CandidateCommit, r.CandidateCommitSHA)
	if err != nil {
		return ReviewReceiptV1{}, err
	}
	canonical.CandidateTree, err = reconcileReceiptString("candidate_tree", r.CandidateTree, r.CandidateTreeSHA)
	if err != nil {
		return ReviewReceiptV1{}, err
	}
	canonical.TargetRef, err = reconcileReceiptString("target_ref", r.TargetRef, r.Target)
	if err != nil {
		return ReviewReceiptV1{}, err
	}
	verdict := string(r.Verdict)
	aliasVerdict := string(r.ReviewVerdict)
	verdict, err = reconcileReceiptString("verdict", verdict, aliasVerdict)
	if err != nil {
		return ReviewReceiptV1{}, err
	}
	canonical.Verdict = ReviewReceiptVerdict(verdict)
	canonical.ReviewModel, err = reconcileReceiptString("review_model", r.ReviewModel, r.Model)
	if err != nil {
		return ReviewReceiptV1{}, err
	}
	canonical.ReviewProfile, err = reconcileReceiptString("review_profile", r.ReviewProfile, r.Profile)
	if err != nil {
		return ReviewReceiptV1{}, err
	}
	canonical.SupersedesReceiptID, err = reconcileReceiptString("supersedes_receipt_id", r.SupersedesReceiptID, r.Supersedes)
	if err != nil {
		return ReviewReceiptV1{}, err
	}
	if r.ReceiptVersion != 0 && r.Version != 0 && r.ReceiptVersion != r.Version {
		return ReviewReceiptV1{}, fmt.Errorf("review receipt version aliases conflict")
	}
	canonical.ReceiptVersion = r.ReceiptVersion
	if canonical.ReceiptVersion == 0 {
		canonical.ReceiptVersion = r.Version
	}
	if canonical.ReceiptVersion == 0 {
		canonical.ReceiptVersion = ReviewReceiptV1Version
	}
	canonical.Version = canonical.ReceiptVersion
	return canonical, nil
}

func reconcileReceiptString(field, primary, alias string) (string, error) {
	if primary != "" && alias != "" && primary != alias {
		return "", fmt.Errorf("review receipt %s aliases conflict", field)
	}
	if primary != "" {
		return primary, nil
	}
	return alias, nil
}

// FormatReviewReceiptV1 emits the only accepted canonical comment body. It
// returns an empty string for an incomplete or conflicting payload; callers
// must not use it to smuggle arbitrary metadata into a review child.
func FormatReviewReceiptV1(input interface{}) string {
	receipt, ok := reviewReceiptValue(input)
	if !ok {
		return ""
	}
	payload, err := receipt.canonicalPayload()
	if err != nil || validateReviewReceiptPayload(payload) != nil {
		return ""
	}
	lines := []string{
		"schema: " + ReviewReceiptV1Schema,
		"receipt_version: " + strconv.Itoa(payload.ReceiptVersion),
		"source_issue: " + payload.SourceIssue,
		"review_child_issue: " + payload.ReviewChildIssue,
		"candidate_commit: " + payload.CandidateCommit,
		"candidate_tree: " + payload.CandidateTree,
		"target_ref: " + payload.TargetRef,
		"verdict: " + string(payload.Verdict),
		"review_model: " + payload.ReviewModel,
		"review_profile: " + payload.ReviewProfile,
	}
	if payload.SupersedesReceiptID != "" {
		lines = append(lines, "supersedes_receipt_id: "+payload.SupersedesReceiptID)
	}
	return strings.Join(lines, "\n")
}

// ParseReviewReceiptV1 parses one stored Beads comment. The reviewer and
// server time are read from Comment.Author and Comment.CreatedAt; body fields
// pretending to carry either value are unknown and rejected.
func ParseReviewReceiptV1(input interface{}) (*ReviewReceiptV1, error) {
	comment, err := reviewReceiptComment(input)
	if err != nil {
		return nil, err
	}
	if comment.ID == "" {
		return nil, fmt.Errorf("review receipt comment ID is missing")
	}
	if err := validReviewReceiptID(comment.ID); err != nil {
		return nil, fmt.Errorf("review receipt comment ID: %w", err)
	}
	if strings.TrimSpace(comment.Author) == "" || comment.Author != strings.TrimSpace(comment.Author) {
		return nil, fmt.Errorf("review receipt comment has invalid stored author")
	}
	if !utf8.ValidString(comment.Text) || comment.Text == "" {
		return nil, fmt.Errorf("review receipt comment has invalid body")
	}
	if comment.Text != strings.TrimSpace(comment.Text) || strings.Contains(comment.Text, "\r") {
		return nil, fmt.Errorf("review receipt body is not canonical whitespace")
	}
	serverTime, err := parseReviewReceiptTime(comment.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("review receipt comment timestamp: %w", err)
	}

	values := make(map[string]string, 11)
	for lineNumber, line := range strings.Split(comment.Text, "\n") {
		if line == "" || line != strings.TrimSpace(line) {
			return nil, fmt.Errorf("review receipt line %d is not canonical", lineNumber+1)
		}
		separator := strings.Index(line, ": ")
		if separator <= 0 || separator+2 >= len(line) || strings.Contains(line[:separator], " ") {
			return nil, fmt.Errorf("review receipt line %d is malformed", lineNumber+1)
		}
		key := line[:separator]
		value := line[separator+2:]
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("review receipt field %q is duplicated", key)
		}
		switch key {
		case "schema", "receipt_version", "source_issue", "review_child_issue", "candidate_commit", "candidate_tree", "target_ref", "verdict", "review_model", "review_profile", "supersedes_receipt_id":
			values[key] = value
		default:
			return nil, fmt.Errorf("review receipt field %q is unknown", key)
		}
		if err := validReviewReceiptScalar(value, key); err != nil {
			return nil, err
		}
	}
	for _, required := range []string{"schema", "receipt_version", "source_issue", "review_child_issue", "candidate_commit", "candidate_tree", "target_ref", "verdict", "review_model", "review_profile"} {
		if values[required] == "" {
			return nil, fmt.Errorf("review receipt field %q is missing", required)
		}
	}
	if values["schema"] != ReviewReceiptV1Schema {
		return nil, fmt.Errorf("review receipt schema %q is unsupported", values["schema"])
	}
	version, err := strconv.Atoi(values["receipt_version"])
	if err != nil || strconv.Itoa(version) != values["receipt_version"] || version != ReviewReceiptV1Version {
		return nil, fmt.Errorf("review receipt version %q is unsupported", values["receipt_version"])
	}
	receipt := ReviewReceiptV1{
		Schema:              values["schema"],
		ReceiptVersion:      version,
		Version:             version,
		SourceIssue:         values["source_issue"],
		SourceIssueID:       values["source_issue"],
		ReviewChildIssue:    values["review_child_issue"],
		ReviewChildIssueID:  values["review_child_issue"],
		ReviewChild:         values["review_child_issue"],
		CandidateCommit:     values["candidate_commit"],
		CandidateCommitSHA:  values["candidate_commit"],
		CandidateTree:       values["candidate_tree"],
		CandidateTreeSHA:    values["candidate_tree"],
		TargetRef:           values["target_ref"],
		Target:              values["target_ref"],
		Verdict:             ReviewReceiptVerdict(values["verdict"]),
		ReviewVerdict:       ReviewReceiptVerdict(values["verdict"]),
		ReviewModel:         values["review_model"],
		Model:               values["review_model"],
		ReviewProfile:       values["review_profile"],
		Profile:             values["review_profile"],
		SupersedesReceiptID: values["supersedes_receipt_id"],
		Supersedes:          values["supersedes_receipt_id"],
		Reviewer:            comment.Author,
		ServerTime:          serverTime,
		ServerTimestamp:     serverTime,
		Timestamp:           serverTime,
		CommentID:           comment.ID,
		ReceiptID:           comment.ID,
	}
	if err := validateReviewReceiptPayload(receipt); err != nil {
		return nil, err
	}
	if canonical := FormatReviewReceiptV1(receipt); canonical != comment.Text {
		return nil, fmt.Errorf("review receipt body is not canonical")
	}
	return &receipt, nil
}

func parseReviewReceiptTime(raw string) (time.Time, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return time.Time{}, fmt.Errorf("timestamp is empty or padded")
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, err
	}
	return parsed, nil
}

func validReviewReceiptScalar(value, field string) error {
	if value == "" || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return fmt.Errorf("review receipt field %q has invalid value", field)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.IsSpace(char) {
			return fmt.Errorf("review receipt field %q contains whitespace or control data", field)
		}
	}
	return nil
}

func validateReviewReceiptPayload(receipt ReviewReceiptV1) error {
	payload, err := receipt.canonicalPayload()
	if err != nil {
		return err
	}
	if payload.Schema != ReviewReceiptV1Schema || payload.ReceiptVersion != ReviewReceiptV1Version {
		return fmt.Errorf("review receipt version/schema is unsupported")
	}
	for name, value := range map[string]string{
		"source_issue":       payload.SourceIssue,
		"review_child_issue": payload.ReviewChildIssue,
		"candidate_commit":   payload.CandidateCommit,
		"candidate_tree":     payload.CandidateTree,
		"target_ref":         payload.TargetRef,
		"review_model":       payload.ReviewModel,
		"review_profile":     payload.ReviewProfile,
	} {
		if err := validReviewReceiptScalar(value, name); err != nil {
			return err
		}
		if strings.Contains(value, ":") {
			return fmt.Errorf("review receipt field %q contains a forbidden separator", name)
		}
	}
	if !isReviewReceiptSHA(payload.CandidateCommit) || !isReviewReceiptSHA(payload.CandidateTree) {
		return fmt.Errorf("review receipt candidate commit/tree must be lowercase Git SHA-1 values")
	}
	if err := validateReviewReceiptTargetRef(payload.TargetRef); err != nil {
		return err
	}
	if payload.Verdict != ReviewReceiptVerdictFinalPass && payload.Verdict != ReviewReceiptVerdictChangesRequired {
		return fmt.Errorf("review receipt verdict %q is unsupported", payload.Verdict)
	}
	if !isIndependentReviewModel(payload.ReviewModel, payload.ReviewProfile) {
		return fmt.Errorf("review receipt model/profile is not codex-sol-medium/medium")
	}
	if payload.SupersedesReceiptID != "" {
		if err := validReviewReceiptID(payload.SupersedesReceiptID); err != nil {
			return fmt.Errorf("review receipt supersession: %w", err)
		}
	}
	return nil
}

func isReviewReceiptSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func isReviewReceiptID(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) || char == ' ' || char == '\t' || char == '\r' || char == '\n' || char == ':' {
			return false
		}
	}
	return true
}

func validReviewReceiptID(value string) error {
	if !isReviewReceiptID(value) {
		return fmt.Errorf("receipt ID %q is malformed", value)
	}
	return nil
}

func validateReviewReceiptTargetRef(target string) error {
	if err := validReviewReceiptScalar(target, "target_ref"); err != nil {
		return err
	}
	if strings.HasPrefix(target, "/") || strings.HasSuffix(target, "/") || strings.Contains(target, "..") || strings.Contains(target, "//") || strings.Contains(target, "@{") {
		return fmt.Errorf("review receipt target ref %q is malformed", target)
	}
	for _, char := range target {
		switch char {
		case '~', '^', ':', '?', '*', '[', '\\', ' ':
			return fmt.Errorf("review receipt target ref %q is malformed", target)
		}
	}
	for _, component := range strings.Split(target, "/") {
		if component == "" || component == "." || component == ".." || strings.HasSuffix(component, ".") || strings.HasSuffix(component, ".lock") {
			return fmt.Errorf("review receipt target ref %q is malformed", target)
		}
	}
	return nil
}

func reviewReceiptValue(input interface{}) (ReviewReceiptV1, bool) {
	switch value := input.(type) {
	case ReviewReceiptV1:
		return value, true
	case *ReviewReceiptV1:
		if value != nil {
			return *value, true
		}
	}
	return ReviewReceiptV1{}, false
}

func reviewReceiptComment(input interface{}) (Comment, error) {
	switch value := input.(type) {
	case Comment:
		return value, nil
	case *Comment:
		if value != nil {
			return *value, nil
		}
	}
	return Comment{}, fmt.Errorf("review receipt requires a stored Beads Comment")
}

func reviewReceiptPayloadEqual(left, right ReviewReceiptV1) bool {
	leftPayload, leftErr := left.canonicalPayload()
	rightPayload, rightErr := right.canonicalPayload()
	if leftErr != nil || rightErr != nil {
		return false
	}
	return leftPayload.Schema == rightPayload.Schema &&
		leftPayload.ReceiptVersion == rightPayload.ReceiptVersion &&
		leftPayload.SourceIssue == rightPayload.SourceIssue &&
		leftPayload.ReviewChildIssue == rightPayload.ReviewChildIssue &&
		leftPayload.CandidateCommit == rightPayload.CandidateCommit &&
		leftPayload.CandidateTree == rightPayload.CandidateTree &&
		leftPayload.TargetRef == rightPayload.TargetRef &&
		leftPayload.Verdict == rightPayload.Verdict &&
		leftPayload.ReviewModel == rightPayload.ReviewModel &&
		leftPayload.ReviewProfile == rightPayload.ReviewProfile &&
		leftPayload.SupersedesReceiptID == rightPayload.SupersedesReceiptID
}

// ValidateReviewReceiptV1 validates a parsed payload against its stored
// comment and source-bound context. The variadic form accepts either
// (Comment, ReviewReceiptValidationContext), (*Comment, *Context), or a
// context paired with a Comment; omitting the stored Comment fails closed.
func ValidateReviewReceiptV1(input interface{}, args ...interface{}) error {
	receipt, ok := reviewReceiptValue(input)
	if !ok {
		comment, err := reviewReceiptComment(input)
		if err != nil {
			return fmt.Errorf("review receipt payload is missing")
		}
		parsed, err := ParseReviewReceiptV1(comment)
		if err != nil {
			return err
		}
		receipt = *parsed
		args = append([]interface{}{comment}, args...)
	}
	if receipt.ReceiptVersion == 0 && receipt.Version == 0 {
		return fmt.Errorf("review receipt version is missing")
	}
	comment, context, err := reviewReceiptValidationArgs(args)
	if err != nil {
		return err
	}
	parsed, err := ParseReviewReceiptV1(comment)
	if err != nil {
		return err
	}
	if !reviewReceiptPayloadEqual(receipt, *parsed) {
		return fmt.Errorf("review receipt payload does not match stored comment")
	}
	if receipt.CommentID != "" && receipt.CommentID != parsed.CommentID {
		return fmt.Errorf("review receipt comment ID is not authoritative")
	}
	if receipt.ReceiptID != "" && receipt.ReceiptID != parsed.ReceiptID {
		return fmt.Errorf("review receipt ID is not authoritative")
	}
	if receipt.Reviewer != "" && receipt.Reviewer != parsed.Reviewer {
		return fmt.Errorf("review receipt reviewer is not authoritative")
	}
	for _, suppliedTime := range []time.Time{receipt.ServerTime, receipt.ServerTimestamp, receipt.Timestamp} {
		if !suppliedTime.IsZero() && !suppliedTime.Equal(parsed.ServerTime) {
			return fmt.Errorf("review receipt server time is not authoritative")
		}
	}
	if receipt.Text != "" {
		return fmt.Errorf("review receipt text is derived from the stored Comment")
	}
	// Continue with the parser's authority-bearing value. A caller may submit
	// only the typed payload for convenience, but zero or forged derived fields
	// must never bypass freshness or reviewer-independence checks.
	return validateReviewReceiptAgainstContext(*parsed, comment, context)
}

func reviewReceiptValidationArgs(args []interface{}) (Comment, ReviewReceiptValidationContext, error) {
	var comment Comment
	var haveComment bool
	var context ReviewReceiptValidationContext
	var haveContext bool
	for _, arg := range args {
		switch value := arg.(type) {
		case Comment:
			if haveComment {
				return Comment{}, ReviewReceiptValidationContext{}, fmt.Errorf("review receipt stored Comment authority is duplicated")
			}
			comment, haveComment = value, true
		case *Comment:
			if value == nil {
				return Comment{}, ReviewReceiptValidationContext{}, fmt.Errorf("review receipt comment is nil")
			}
			if haveComment {
				return Comment{}, ReviewReceiptValidationContext{}, fmt.Errorf("review receipt stored Comment authority is duplicated")
			}
			comment, haveComment = *value, true
		case ReviewReceiptValidationContext:
			if haveContext {
				return Comment{}, ReviewReceiptValidationContext{}, fmt.Errorf("review receipt validation context is duplicated")
			}
			context, haveContext = value, true
		case *ReviewReceiptValidationContext:
			if value == nil {
				return Comment{}, ReviewReceiptValidationContext{}, fmt.Errorf("review receipt validation context is nil")
			}
			if haveContext {
				return Comment{}, ReviewReceiptValidationContext{}, fmt.Errorf("review receipt validation context is duplicated")
			}
			context, haveContext = *value, true
		default:
			return Comment{}, ReviewReceiptValidationContext{}, fmt.Errorf("unsupported review receipt validation argument %T", arg)
		}
	}
	if !haveComment {
		return Comment{}, ReviewReceiptValidationContext{}, fmt.Errorf("review receipt validation requires stored Comment authority")
	}
	if !haveContext {
		return Comment{}, ReviewReceiptValidationContext{}, fmt.Errorf("review receipt validation context is missing")
	}
	return comment, context, nil
}

// ValidateReviewReceiptCommentV1 parses and validates one stored comment in a
// single operation, retaining the derived identity/time fields.
func ValidateReviewReceiptCommentV1(comment Comment, context ReviewReceiptValidationContext) (*ReviewReceiptV1, error) {
	receipt, err := ParseReviewReceiptV1(comment)
	if err != nil {
		return nil, err
	}
	if err := ValidateReviewReceiptV1(receipt, comment, context); err != nil {
		return nil, err
	}
	return receipt, nil
}

func validateReviewReceiptAgainstContext(receipt ReviewReceiptV1, comment Comment, context ReviewReceiptValidationContext) error {
	sourceID, childID, candidate, tree, target, frozenAt, err := normalizeReviewReceiptContext(context)
	if err != nil {
		return err
	}
	if comment.ID == "" {
		return fmt.Errorf("review receipt comment ID is missing")
	}
	if comment.IssueID != childID {
		return fmt.Errorf("review receipt comment belongs to %q, want review child %q", comment.IssueID, childID)
	}
	if receipt.SourceIssue != sourceID || receipt.ReviewChildIssue != childID {
		return fmt.Errorf("review receipt source/review-child binding mismatch")
	}
	if receipt.CandidateCommit != candidate || receipt.CandidateTree != tree || receipt.TargetRef != target {
		return fmt.Errorf("review receipt candidate/tree/target binding mismatch")
	}
	if receipt.ServerTime.Before(frozenAt) {
		return fmt.Errorf("review receipt comment predates immutable candidate freeze")
	}
	if receipt.SupersedesReceiptID != "" && receipt.SupersedesReceiptID == receipt.ReceiptID {
		return fmt.Errorf("review receipt cannot supersede itself")
	}
	if !isIndependentReviewModel(receipt.ReviewModel, receipt.ReviewProfile) {
		return fmt.Errorf("review receipt model/profile is not an independent Sol-medium review")
	}
	if reviewReceiptAuthorExcluded(receipt.Reviewer, context, sourceID) {
		return fmt.Errorf("review receipt reviewer is an author/coauthor/excluded identity")
	}
	if len(context.ReachableCommits) > 0 || context.ReachableCommits != nil || len(context.TargetReachableCommits) > 0 || context.TargetReachableCommits != nil || context.Reachable != nil || context.TargetReachable != nil || context.ReachableCommitSet != nil {
		if !reviewReceiptReachableInAllSets(receipt.CandidateCommit, context) {
			return fmt.Errorf("review receipt candidate is not the exact target-reachable SHA")
		}
	}
	for _, treeByCommit := range []map[string]string{context.CommitTrees} {
		if treeByCommit != nil {
			boundTree, ok := treeByCommit[receipt.CandidateCommit]
			if !ok || boundTree != receipt.CandidateTree {
				return fmt.Errorf("review receipt candidate/tree is not the authenticated commit mapping")
			}
		}
	}
	child := context.ReviewChildIssue
	source := context.SourceIssue
	if source == nil || child == nil {
		return fmt.Errorf("review receipt requires source and dedicated review-child issues")
	}
	if source.ID != sourceID || child.ID != childID || child.Parent != sourceID {
		return fmt.Errorf("review receipt source/review-child parent binding mismatch")
	}
	if source.ID == child.ID {
		return fmt.Errorf("review receipt source and review child must differ")
	}
	if !IssueStatus(child.Status).IsTerminal() {
		return fmt.Errorf("review receipt child is not terminal")
	}
	if err := validateReviewReceiptMetadata(child.Metadata); err != nil {
		return fmt.Errorf("review receipt child metadata: %w", err)
	}
	if child.Comments != nil {
		for _, stored := range child.Comments {
			if _, err := ParseReviewReceiptV1(stored); err != nil {
				return fmt.Errorf("review receipt child has arbitrary or malformed comment: %w", err)
			}
		}
	}
	return nil
}

func normalizeReviewReceiptContext(context ReviewReceiptValidationContext) (string, string, string, string, string, time.Time, error) {
	if context.SourceIssue == nil || context.ReviewChildIssue == nil {
		return "", "", "", "", "", time.Time{}, fmt.Errorf("review receipt context requires source and review-child issues")
	}
	sourceID := context.SourceIssue.ID
	if context.SourceIssueID != "" && context.SourceIssueID != sourceID {
		return "", "", "", "", "", time.Time{}, fmt.Errorf("review receipt source issue aliases conflict")
	}
	childID := context.ReviewChildIssue.ID
	for _, alias := range []string{context.ReviewChildIssueID, context.ReviewChild} {
		if alias != "" && alias != childID {
			return "", "", "", "", "", time.Time{}, fmt.Errorf("review receipt review-child aliases conflict")
		}
	}
	candidate, err := reconcileReceiptString("candidate_commit", context.CandidateCommit, context.CandidateCommitSHA)
	if err != nil {
		return "", "", "", "", "", time.Time{}, err
	}
	tree, err := reconcileReceiptString("candidate_tree", context.CandidateTree, context.CandidateTreeSHA)
	if err != nil {
		return "", "", "", "", "", time.Time{}, err
	}
	target, err := reconcileReceiptString("target_ref", context.TargetRef, context.ExpectedTargetRef)
	if err != nil {
		return "", "", "", "", "", time.Time{}, err
	}
	frozenAt, err := reconcileReviewReceiptTimes(context.FrozenAt, context.FreezeAt, context.CandidateFrozenAt)
	if err != nil {
		return "", "", "", "", "", time.Time{}, err
	}
	if context.FreezeTimestamp != "" {
		parsed, parseErr := parseReviewReceiptTime(context.FreezeTimestamp)
		if parseErr != nil {
			return "", "", "", "", "", time.Time{}, fmt.Errorf("review receipt freeze timestamp: %w", parseErr)
		}
		if !frozenAt.IsZero() && !frozenAt.Equal(parsed) {
			return "", "", "", "", "", time.Time{}, fmt.Errorf("review receipt freeze timestamp aliases conflict")
		}
		frozenAt = parsed
	}
	if sourceID == "" || childID == "" || candidate == "" || tree == "" || target == "" || frozenAt.IsZero() {
		return "", "", "", "", "", time.Time{}, fmt.Errorf("review receipt context is incomplete")
	}
	return sourceID, childID, candidate, tree, target, frozenAt, nil
}

func reconcileReviewReceiptTimes(values ...time.Time) (time.Time, error) {
	var selected time.Time
	for _, value := range values {
		if value.IsZero() {
			continue
		}
		if !selected.IsZero() && !selected.Equal(value) {
			return time.Time{}, fmt.Errorf("review receipt freeze time aliases conflict")
		}
		selected = value
	}
	return selected, nil
}

func reviewReceiptAuthorExcluded(reviewer string, context ReviewReceiptValidationContext, sourceID string) bool {
	identities := make([]string, 0, 1+len(context.Coauthors)+len(context.Authors)+len(context.AuthorIdentities)+len(context.ExcludedReviewers)+len(context.Excluded)+len(context.ExclusionSet))
	identities = append(identities, context.Author, context.AuthorIdentity, context.ImplementationAuthor)
	identities = append(identities, context.Coauthors...)
	identities = append(identities, context.Authors...)
	identities = append(identities, context.AuthorIdentities...)
	identities = append(identities, context.ExcludedReviewers...)
	identities = append(identities, context.Excluded...)
	identities = append(identities, context.ExclusionSet...)
	if context.SourceIssue != nil && context.SourceIssue.ID == sourceID {
		identities = append(identities, context.SourceIssue.Assignee, context.SourceIssue.CreatedBy)
	}
	reviewer = strings.TrimSpace(reviewer)
	for _, identity := range identities {
		identity = strings.TrimSpace(identity)
		if identity != "" && strings.EqualFold(reviewer, identity) {
			return true
		}
	}
	return false
}

func isIndependentReviewModel(model, profile string) bool {
	return model == "codex-sol-medium" && profile == "medium"
}

func reviewReceiptReachableInAllSets(candidate string, context ReviewReceiptValidationContext) bool {
	for _, set := range [][]string{context.ReachableCommits, context.TargetReachableCommits} {
		if set == nil {
			continue
		}
		found := false
		for _, value := range set {
			if value == candidate {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, set := range []map[string]bool{context.Reachable, context.TargetReachable, context.ReachableCommitSet} {
		if set != nil && !set[candidate] {
			return false
		}
	}
	return true
}

type reviewReceiptMetadataFieldType uint8

const (
	reviewReceiptMetadataString reviewReceiptMetadataFieldType = iota + 1
	reviewReceiptMetadataInteger
	reviewReceiptMetadataBoolean
)

const reviewReceiptMetadataMaxStringBytes = 1024

// reviewReceiptMetadataAllowlist describes the inert orchestration metadata
// currently attached to authoritative review children. These fields are
// deliberately not copied into ReviewReceiptV1 and never participate in
// source, reviewer, verdict, freshness, or candidate binding.
var reviewReceiptMetadataAllowlist = map[string]reviewReceiptMetadataFieldType{
	"actual_handle":                     reviewReceiptMetadataString,
	"actual_handle_status":              reviewReceiptMetadataString,
	"actual_model":                      reviewReceiptMetadataString,
	"boundary_reproductions":            reviewReceiptMetadataString,
	"candidate_commit":                  reviewReceiptMetadataString,
	"candidate_diff_sha256":             reviewReceiptMetadataString,
	"candidate_parent":                  reviewReceiptMetadataString,
	"candidate_parent_tree":             reviewReceiptMetadataString,
	"candidate_paths_sha256":            reviewReceiptMetadataString,
	"candidate_tree":                    reviewReceiptMetadataString,
	"cumulative_base":                   reviewReceiptMetadataString,
	"cumulative_full_index_diff_sha256": reviewReceiptMetadataString,
	"current_main_overlap":              reviewReceiptMetadataString,
	"current_private_main":              reviewReceiptMetadataString,
	"current_private_main_tree":         reviewReceiptMetadataString,
	"dispatch_authorized":               reviewReceiptMetadataBoolean,
	"exact_path_count":                  reviewReceiptMetadataInteger,
	"exact_path_lease":                  reviewReceiptMetadataString,
	"gate_status":                       reviewReceiptMetadataString,
	"host_process_status":               reviewReceiptMetadataString,
	"landed":                            reviewReceiptMetadataBoolean,
	"landing_authorized":                reviewReceiptMetadataBoolean,
	"mutation_authorized":               reviewReceiptMetadataBoolean,
	"parent_full_index_diff_sha256":     reviewReceiptMetadataString,
	"phase":                             reviewReceiptMetadataString,
	"provider_action_authorized":        reviewReceiptMetadataBoolean,
	"reconciliation_receipt":            reviewReceiptMetadataString,
	"requested_model":                   reviewReceiptMetadataString,
	"review_handle":                     reviewReceiptMetadataString,
	"review_model":                      reviewReceiptMetadataString,
	"review_status":                     reviewReceiptMetadataString,
	"reviewer_active":                   reviewReceiptMetadataBoolean,
	"reviewer_identity":                 reviewReceiptMetadataString,
	"terminal_verdict":                  reviewReceiptMetadataString,
	"verified_candidate_commit":         reviewReceiptMetadataString,
	"verified_candidate_tree":           reviewReceiptMetadataString,
}

// validateReviewReceiptMetadata accepts only the inert metadata shape emitted
// on the real review child. Empty metadata and JSON null are valid because
// metadata is optional. Every nonempty object is parsed token-by-token so
// duplicate keys cannot be hidden by map decoding, and no value is consulted
// as receipt authority.
func validateReviewReceiptMetadata(metadata json.RawMessage) error {
	trimmed := bytes.TrimSpace(metadata)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if !utf8.Valid(trimmed) {
		return fmt.Errorf("metadata is not valid UTF-8")
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("metadata JSON is malformed: %w", err)
	}
	objectStart, ok := token.(json.Delim)
	if !ok || objectStart != '{' {
		return fmt.Errorf("metadata must be a JSON object")
	}

	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("metadata object is malformed: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("metadata key is not a string")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("metadata key %q is duplicated", key)
		}
		seen[key] = struct{}{}
		fieldType, allowed := reviewReceiptMetadataAllowlist[key]
		if !allowed {
			return fmt.Errorf("metadata key %q is unknown", key)
		}

		var rawValue json.RawMessage
		if err := decoder.Decode(&rawValue); err != nil {
			return fmt.Errorf("metadata value for %q is malformed: %w", key, err)
		}
		if err := validateReviewReceiptMetadataValue(key, fieldType, rawValue); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("metadata object is missing closing delimiter: %w", err)
	}
	if objectEnd, ok := closing.(json.Delim); !ok || objectEnd != '}' {
		return fmt.Errorf("metadata object has invalid closing delimiter")
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("metadata has trailing JSON data")
		}
		return fmt.Errorf("metadata has trailing malformed data: %w", err)
	}
	return nil
}

func validateReviewReceiptMetadataValue(key string, fieldType reviewReceiptMetadataFieldType, rawValue json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(rawValue), []byte("null")) {
		return fmt.Errorf("metadata value for %q must not be null", key)
	}
	switch fieldType {
	case reviewReceiptMetadataString:
		var value string
		if err := json.Unmarshal(rawValue, &value); err != nil {
			return fmt.Errorf("metadata value for %q must be a string", key)
		}
		if value == "" || len(value) > reviewReceiptMetadataMaxStringBytes || !utf8.ValidString(value) {
			return fmt.Errorf("metadata value for %q is empty, oversized, or invalid", key)
		}
		for _, char := range value {
			if unicode.IsControl(char) {
				return fmt.Errorf("metadata value for %q contains control data", key)
			}
		}
	case reviewReceiptMetadataInteger:
		var value int64
		if err := json.Unmarshal(rawValue, &value); err != nil || value < 0 {
			return fmt.Errorf("metadata value for %q must be a non-negative integer", key)
		}
	case reviewReceiptMetadataBoolean:
		var value bool
		if err := json.Unmarshal(rawValue, &value); err != nil {
			return fmt.Errorf("metadata value for %q must be a boolean", key)
		}
	default:
		return fmt.Errorf("metadata key %q has unsupported type", key)
	}
	return nil
}

// SelectCurrentReviewReceiptV1 parses and validates all comments in a
// dedicated review child, then follows the explicit supersession graph. Any
// malformed comment, duplicate ID, unknown supersession, cycle, or more than
// one unsuperseded leaf fails closed.
func SelectCurrentReviewReceiptV1(input interface{}, args ...interface{}) (*ReviewReceiptV1, error) {
	comments, err := reviewReceiptComments(input)
	if err != nil {
		return nil, err
	}
	_, context, err := reviewReceiptSelectionArgs(args)
	if err != nil {
		return nil, err
	}
	if len(comments) == 0 {
		return nil, fmt.Errorf("review receipt set is empty")
	}
	receipts := make(map[string]*ReviewReceiptV1, len(comments))
	for _, comment := range comments {
		receipt, parseErr := ParseReviewReceiptV1(comment)
		if parseErr != nil {
			return nil, parseErr
		}
		if err := ValidateReviewReceiptV1(receipt, comment, context); err != nil {
			return nil, err
		}
		if receipt.CommentID == "" {
			return nil, fmt.Errorf("review receipt comment ID is missing")
		}
		if _, exists := receipts[receipt.CommentID]; exists {
			return nil, fmt.Errorf("review receipt comment ID %q is duplicated", receipt.CommentID)
		}
		receipts[receipt.CommentID] = receipt
	}
	supersededBy := make(map[string]string, len(receipts))
	for id, receipt := range receipts {
		parent := receipt.SupersedesReceiptID
		if parent == "" {
			continue
		}
		if parent == id {
			return nil, fmt.Errorf("review receipt %q self-supersedes", id)
		}
		if _, exists := receipts[parent]; !exists {
			return nil, fmt.Errorf("review receipt %q supersedes unknown receipt %q", id, parent)
		}
		if !receipt.ServerTime.After(receipts[parent].ServerTime) {
			return nil, fmt.Errorf("review receipt %q is not newer than superseded receipt %q", id, parent)
		}
		if prior, exists := supersededBy[parent]; exists && prior != id {
			return nil, fmt.Errorf("review receipt %q has conflicting unsuperseded successors", parent)
		}
		supersededBy[parent] = id
	}
	for id := range receipts {
		visited := make(map[string]bool, len(receipts))
		current := id
		for current != "" {
			if visited[current] {
				return nil, fmt.Errorf("review receipt supersession cycle at %q", current)
			}
			visited[current] = true
			receipt := receipts[current]
			if receipt == nil {
				return nil, fmt.Errorf("review receipt graph contains a nil receipt")
			}
			current = receipt.SupersedesReceiptID
		}
	}
	var leaves []*ReviewReceiptV1
	for id, receipt := range receipts {
		if _, superseded := supersededBy[id]; !superseded {
			leaves = append(leaves, receipt)
		}
	}
	if len(leaves) != 1 {
		return nil, fmt.Errorf("review receipt set has %d current unsuperseded receipts; want exactly one", len(leaves))
	}
	return leaves[0], nil
}

func reviewReceiptSelectionArgs(args []interface{}) ([]Comment, ReviewReceiptValidationContext, error) {
	var context ReviewReceiptValidationContext
	var haveContext bool
	for _, arg := range args {
		switch value := arg.(type) {
		case ReviewReceiptValidationContext:
			if haveContext {
				return nil, ReviewReceiptValidationContext{}, fmt.Errorf("review receipt selection context is duplicated")
			}
			context, haveContext = value, true
		case *ReviewReceiptValidationContext:
			if value == nil {
				return nil, ReviewReceiptValidationContext{}, fmt.Errorf("review receipt validation context is nil")
			}
			if haveContext {
				return nil, ReviewReceiptValidationContext{}, fmt.Errorf("review receipt selection context is duplicated")
			}
			context, haveContext = *value, true
		default:
			return nil, ReviewReceiptValidationContext{}, fmt.Errorf("unsupported review receipt selection argument %T", arg)
		}
	}
	if !haveContext {
		return nil, ReviewReceiptValidationContext{}, fmt.Errorf("review receipt selection context is missing")
	}
	return nil, context, nil
}

func reviewReceiptComments(input interface{}) ([]Comment, error) {
	switch values := input.(type) {
	case []Comment:
		return values, nil
	case []*Comment:
		comments := make([]Comment, 0, len(values))
		for _, value := range values {
			if value == nil {
				return nil, fmt.Errorf("review receipt comment is nil")
			}
			comments = append(comments, *value)
		}
		return comments, nil
	case Comment:
		return []Comment{values}, nil
	case *Comment:
		if values == nil {
			return nil, fmt.Errorf("review receipt comment is nil")
		}
		return []Comment{*values}, nil
	default:
		return nil, fmt.Errorf("review receipt requires Beads comments, got %T", input)
	}
}

// SelectCurrentReviewReceiptV1FromIssue is the child-oriented convenience
// entry point used by MQ/Refinery consumers. The child comments are the only
// candidate receipt set; no aggregate GitHub or metadata state is consulted.
func SelectCurrentReviewReceiptV1FromIssue(context ReviewReceiptValidationContext) (*ReviewReceiptV1, error) {
	if context.ReviewChildIssue == nil {
		return nil, fmt.Errorf("review receipt review child is missing")
	}
	return SelectCurrentReviewReceiptV1(context.ReviewChildIssue.Comments, context)
}

// ParseReviewReceipt is a short compatibility alias for the versioned parser.
func ParseReviewReceipt(input interface{}) (*ReviewReceiptV1, error) {
	return ParseReviewReceiptV1(input)
}

// FormatReviewReceipt is a short compatibility alias for the versioned
// formatter.
func FormatReviewReceipt(input interface{}) string {
	return FormatReviewReceiptV1(input)
}

// SelectCurrentReviewReceipt is a short compatibility alias for the strict
// versioned selector.
func SelectCurrentReviewReceipt(input interface{}, args ...interface{}) (*ReviewReceiptV1, error) {
	return SelectCurrentReviewReceiptV1(input, args...)
}
