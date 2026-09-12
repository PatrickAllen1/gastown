package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/util"
)

// Convoy represents a convoy's status for the dashboard
type Convoy struct {
	ID         string               `json:"id"`
	Title      string               `json:"title"`
	Status     string               `json:"status"`
	Completed  int                  `json:"completed"`
	Total      int                  `json:"total"`
	CreatedAt  time.Time            `json:"created_at"`
	ClosedAt   time.Time            `json:"closed_at,omitempty"`
	Tracked    []ConvoyTrackedIssue `json:"tracked"`
	Owned      bool                 `json:"owned"`
	OwnedKnown bool                 `json:"-"`
	Lifecycle  string               `json:"lifecycle,omitempty"`
	MetadataOK bool                 `json:"-"`
}

// ConvoyTrackedIssue is the native list command's routed child projection.
// Status is intentionally retained so the dashboard can summarize hidden
// system-managed wrappers without re-querying another beads database.
type ConvoyTrackedIssue struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status"`
}

// MQEntry represents a single merge request in the merge queue
type MQEntry struct {
	ID      string // Bead ID (e.g., "gt-mr-abc")
	Branch  string // Source branch name
	Status  string // queued, merging, merged, failed
	Polecat string // Polecat that submitted (e.g., "nux")
	Rig     string // Which rig this MR belongs to
}

// ConvoyState holds all convoy data for the panel
type ConvoyState struct {
	InProgress []Convoy
	Landed     []Convoy
	All        []Convoy
	MQEntries  []MQEntry
	LastUpdate time.Time
}

// FetchConvoys retrieves convoy status from town-level beads
func FetchConvoys(townRoot string) (*ConvoyState, error) {
	return newNativeConvoySource(townRoot, nil).Fetch(context.Background())
}

func sortConvoyState(state *ConvoyState) {
	// Keep all slices deterministic for a stable TUI and replayable tests.
	sort.SliceStable(state.InProgress, func(i, j int) bool {
		return state.InProgress[i].CreatedAt.Before(state.InProgress[j].CreatedAt)
	})
	sort.SliceStable(state.Landed, func(i, j int) bool {
		return state.Landed[i].ClosedAt.After(state.Landed[j].ClosedAt)
	})
}

// Convoy panel styles
var (
	ConvoyPanelStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorDim).
				Padding(0, 1)

	ConvoyTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorPrimary)

	ConvoySectionStyle = lipgloss.NewStyle().
				Foreground(colorDim).
				Bold(true)

	ConvoyIDStyle = lipgloss.NewStyle().
			Foreground(colorHighlight)

	ConvoyNameStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15"))

	ConvoyProgressStyle = lipgloss.NewStyle().
				Foreground(colorSuccess)

	ConvoyLandedStyle = lipgloss.NewStyle().
				Foreground(colorSuccess).
				Bold(true)

	ConvoyAgeStyle = lipgloss.NewStyle().
			Foreground(colorDim)
)

// renderConvoyPanel renders the convoy status panel
func (m *Model) renderConvoyPanel() string {
	style := ConvoyPanelStyle
	if m.focusedPanel == PanelConvoy {
		style = FocusedBorderStyle
	}
	// Add title before content
	title := ConvoyTitleStyle.Render("🚚 Convoys")
	content := title + "\n" + m.convoyViewport.View()
	return style.Width(m.width - 2).Render(content)
}

// renderConvoys renders the convoy panel content
// renderConvoys renders the convoy status content.
// Caller must hold m.mu.
func (m *Model) renderConvoys() string {
	if m.convoyState == nil {
		if m.convoyError != nil {
			return EventFailStyle.Render("Convoy refresh error: " + m.convoyError.Error())
		}
		return AgentIdleStyle.Render("Loading convoys...")
	}

	var lines []string
	inProgressWrappers := systemWrapperSummary(m.convoyState.InProgress)
	landedWrappers := systemWrapperSummary(m.convoyState.Landed)

	// In Progress section
	lines = append(lines, ConvoySectionStyle.Render("IN PROGRESS"))
	if m.showSystemWrappers {
		if len(m.convoyState.InProgress) == 0 {
			lines = append(lines, "  "+AgentIdleStyle.Render("No active convoys"))
		} else {
			for _, c := range m.convoyState.InProgress {
				lines = appendConvoyRender(lines, c, false, true)
			}
		}
	} else {
		normal := make([]Convoy, 0, len(m.convoyState.InProgress))
		for _, c := range m.convoyState.InProgress {
			if !isVerifiedSystemWrapper(c) {
				normal = append(normal, c)
			}
		}
		if len(normal) == 0 {
			lines = append(lines, "  "+AgentIdleStyle.Render("No active convoys"))
		} else {
			for _, c := range normal {
				lines = appendConvoyRender(lines, c, false, false)
			}
		}
		if inProgressWrappers.count > 0 {
			lines = append(lines, "  "+renderSystemWrapperSummary(inProgressWrappers))
		}
	}

	if m.convoyError != nil {
		stale := "STALE convoy data: " + m.convoyError.Error()
		if !m.lastConvoyGood.IsZero() {
			stale += " (last good " + formatAge(time.Since(m.lastConvoyGood)) + " ago)"
		}
		lines = append(lines, "  "+EventFailStyle.Render(stale))
	}

	lines = append(lines, "")

	// Recently Landed section
	lines = append(lines, ConvoySectionStyle.Render("RECENTLY LANDED (24h)"))
	if m.showSystemWrappers {
		if len(m.convoyState.Landed) == 0 {
			lines = append(lines, "  "+AgentIdleStyle.Render("No recent landings"))
		} else {
			for _, c := range m.convoyState.Landed {
				lines = appendConvoyRender(lines, c, true, true)
			}
		}
	} else {
		normal := make([]Convoy, 0, len(m.convoyState.Landed))
		for _, c := range m.convoyState.Landed {
			if !isVerifiedSystemWrapper(c) {
				normal = append(normal, c)
			}
		}
		if len(normal) == 0 {
			lines = append(lines, "  "+AgentIdleStyle.Render("No recent landings"))
		} else {
			for _, c := range normal {
				lines = appendConvoyRender(lines, c, true, false)
			}
		}
		if landedWrappers.count > 0 {
			lines = append(lines, "  "+renderSystemWrapperSummary(landedWrappers))
		}
	}

	// Toggle help is context-sensitive and remains visible in the short help
	// bar when the convoy panel has focus.
	lines = append(lines, "")
	if m.focusedPanel == PanelConvoy {
		verb := "show wrappers"
		if m.showSystemWrappers {
			verb = "hide wrappers"
		}
		lines = append(lines, ConvoyAgeStyle.Render("  o: "+verb))
	}

	// Merge Queue section
	lines = append(lines, "")
	lines = append(lines, MQTitleStyle.Render("⚙ Merge Queue"))
	if len(m.convoyState.MQEntries) == 0 {
		lines = append(lines, "  "+AgentIdleStyle.Render("No pending merges"))
	} else {
		for _, entry := range m.convoyState.MQEntries {
			lines = append(lines, renderMQLine(entry))
		}
	}

	return strings.Join(lines, "\n")
}

func appendConvoyRender(lines []string, convoy Convoy, landed, detail bool) []string {
	lines = append(lines, renderConvoyLine(convoy, landed))
	if detail && isVerifiedSystemWrapper(convoy) {
		for _, tracked := range convoy.Tracked {
			lines = append(lines, fmt.Sprintf("    ↳ %s [%s]", tracked.ID, tracked.Status))
		}
	}
	return lines
}

func isVerifiedSystemWrapper(convoy Convoy) bool {
	return convoy.MetadataOK && convoy.OwnedKnown && !convoy.Owned &&
		convoy.Lifecycle == "system-managed" && convoy.Total == 1 && len(convoy.Tracked) == 1
}

type systemWrapperTotals struct {
	count     int
	completed int
	total     int
	statuses  map[string]int
}

func systemWrapperSummary(rows []Convoy) systemWrapperTotals {
	totals := systemWrapperTotals{statuses: make(map[string]int)}
	for _, convoy := range rows {
		if !isVerifiedSystemWrapper(convoy) {
			continue
		}
		totals.count++
		totals.completed += convoy.Completed
		totals.total += convoy.Total
		for _, tracked := range convoy.Tracked {
			totals.statuses[tracked.Status]++
		}
	}
	return totals
}

func renderSystemWrapperSummary(totals systemWrapperTotals) string {
	statuses := make([]string, 0, len(totals.statuses))
	for status := range totals.statuses {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	parts := make([]string, 0, len(statuses))
	for _, status := range statuses {
		parts = append(parts, fmt.Sprintf("%s:%d", status, totals.statuses[status]))
	}
	return fmt.Sprintf("SYSTEM TASKS (%d hidden) %d/%d complete [%s]", totals.count, totals.completed, totals.total, strings.Join(parts, " "))
}

// renderConvoyLine renders a single convoy status line
func renderConvoyLine(c Convoy, landed bool) string {
	// Format: "  hq-xyz  Title       2/4 ●●○○" or "  hq-xyz  Title       ✓ 2h ago"
	id := ConvoyIDStyle.Render(c.ID)

	// Truncate title if too long (rune-safe to avoid splitting multi-byte UTF-8)
	title := c.Title
	if utf8.RuneCountInString(title) > 20 {
		runes := []rune(title)
		title = string(runes[:17]) + "..."
	}
	title = ConvoyNameStyle.Render(title)
	if !c.MetadataOK {
		title += " " + EventFailStyle.Render("[metadata unknown]")
	}

	if landed {
		// Show checkmark and time since landing
		age := formatAge(time.Since(c.ClosedAt))
		status := ConvoyLandedStyle.Render("✓") + " " + ConvoyAgeStyle.Render(age+" ago")
		return fmt.Sprintf("  %s  %-20s  %s", id, title, status)
	}
	// Show progress bar
	progress := renderProgressBar(c.Completed, c.Total)
	count := ConvoyProgressStyle.Render(fmt.Sprintf("%d/%d", c.Completed, c.Total))
	return fmt.Sprintf("  %s  %-20s  %s %s", id, title, count, progress)
}

// renderMQLine renders a single merge queue entry
func renderMQLine(entry MQEntry) string {
	// Format: "  ⚙ polecat/nux  branch-name       merging"
	var statusStyle lipgloss.Style
	var statusIcon string
	switch entry.Status {
	case "merging":
		statusStyle = MQStatusMerging
		statusIcon = "⚙"
	case "queued":
		statusStyle = MQStatusQueued
		statusIcon = "○"
	case "merged":
		statusStyle = MQStatusMerged
		statusIcon = "✓"
	case "failed":
		statusStyle = MQStatusFailed
		statusIcon = "✗"
	default:
		statusStyle = MQStatusQueued
		statusIcon = "?"
	}

	// Truncate branch name if too long (rune-safe)
	branch := entry.Branch
	if utf8.RuneCountInString(branch) > 30 {
		runes := []rune(branch)
		branch = string(runes[:27]) + "..."
	}

	// Build the line
	status := statusStyle.Render(statusIcon + " " + entry.Status)
	branchPart := MQBranchStyle.Render(branch)

	polecatPart := ""
	if entry.Polecat != "" {
		polecatPart = MQPolecatStyle.Render(entry.Polecat)
	}

	return fmt.Sprintf("  %s  %-30s  %s", status, branchPart, polecatPart)
}

// MQ panel styles
var (
	MQTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorPrimary)

	MQStatusQueued = lipgloss.NewStyle().
			Foreground(colorDim)

	MQStatusMerging = lipgloss.NewStyle().
			Foreground(colorPrimary)

	MQStatusMerged = lipgloss.NewStyle().
			Foreground(colorSuccess).
			Bold(true)

	MQStatusFailed = lipgloss.NewStyle().
			Foreground(colorError).
			Bold(true)

	MQBranchStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15"))

	MQPolecatStyle = lipgloss.NewStyle().
			Foreground(colorAccent)
)

// mqListItem represents a raw MR bead from bd list --json output
type mqListItem struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	CreatedBy string `json:"created_by,omitempty"`
	Assignee  string `json:"assignee,omitempty"`
}

// fetchMQEntries queries all rigs for merge-request beads
func fetchMQEntries(townRoot string) []MQEntry {
	// Load rigs config to discover rigs
	rigsConfigPath := constants.MayorRigsPath(townRoot)
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return nil
	}

	var entries []MQEntry
	for rigName := range rigsConfig.Rigs {
		rigPath := filepath.Join(townRoot, rigName)
		// Check rig directory exists
		if _, err := os.Stat(rigPath); err != nil {
			continue
		}

		// Fetch open and in-progress MRs
		for _, status := range []string{"open", "in_progress"} {
			items := listMQBeads(rigPath, status)
			for _, item := range items {
				entry := mqItemToEntry(item, rigName)
				entries = append(entries, entry)
			}
		}
	}

	// Sort: in-progress (merging) first, then open (queued)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Status != entries[j].Status {
			return entries[i].Status == "merging"
		}
		return entries[i].ID < entries[j].ID
	})

	return entries
}

// listMQBeads queries bd for merge-request beads with given status
func listMQBeads(rigPath, status string) []mqListItem {
	ctx, cancel := context.WithTimeout(context.Background(), constants.BdSubprocessTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bd", "list",
		"--label=gt:merge-request",
		"--status="+status,
		"--json",
	)
	util.SetDetachedProcessGroup(cmd)
	cmd.Dir = rigPath
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return nil
	}

	var items []mqListItem
	if err := json.Unmarshal(stdout.Bytes(), &items); err != nil {
		return nil
	}
	return items
}

// mqItemToEntry converts a raw MQ bead to an MQEntry with display-friendly fields
func mqItemToEntry(item mqListItem, rigName string) MQEntry {
	entry := MQEntry{
		ID:  item.ID,
		Rig: rigName,
	}

	// Map bead status to display status
	switch item.Status {
	case "in_progress":
		entry.Status = "merging"
	case "open":
		entry.Status = "queued"
	case "closed":
		entry.Status = "merged"
	default:
		entry.Status = item.Status
	}

	// Extract branch name from title (MR beads typically titled with branch name)
	entry.Branch = item.Title
	if entry.Branch == "" {
		entry.Branch = item.ID
	}

	// Extract polecat name from assignee or created_by
	polecat := item.Assignee
	if polecat == "" {
		polecat = item.CreatedBy
	}
	// Shorten: "gastown/polecats/nux" -> "nux", "gastown/nux" -> "nux"
	if parts := strings.Split(polecat, "/"); len(parts) > 0 {
		polecat = parts[len(parts)-1]
	}
	entry.Polecat = polecat

	return entry
}

// renderProgressBar creates a simple progress bar: ●●○○
func renderProgressBar(completed, total int) string {
	if total == 0 {
		return ""
	}

	// Cap at 5 dots for display
	displayTotal := total
	if displayTotal > 5 {
		displayTotal = 5
	}

	filled := (completed * displayTotal) / total
	if filled > displayTotal {
		filled = displayTotal
	}

	bar := strings.Repeat("●", filled) + strings.Repeat("○", displayTotal-filled)
	return ConvoyProgressStyle.Render(bar)
}
