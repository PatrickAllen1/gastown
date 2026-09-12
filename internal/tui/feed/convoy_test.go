package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type nativeConvoyCall struct {
	executable string
	args       []string
	dir        string
	deadline   time.Time
}

type nativeConvoyFakeRunner struct {
	stdout []byte
	stderr []byte
	err    error
	calls  []nativeConvoyCall
}

func (r *nativeConvoyFakeRunner) Run(ctx context.Context, executable string, args []string, dir string) ([]byte, []byte, error) {
	deadline, _ := ctx.Deadline()
	r.calls = append(r.calls, nativeConvoyCall{
		executable: executable,
		args:       append([]string(nil), args...),
		dir:        dir,
		deadline:   deadline,
	})
	return r.stdout, r.stderr, r.err
}

func nativeConvoyJSON(id, title string, statuses ...string) []byte {
	tracked := make([]map[string]string, 0, len(statuses))
	completed := 0
	for i, status := range statuses {
		if status == "closed" {
			completed++
		}
		tracked = append(tracked, map[string]string{
			"id":     fmt.Sprintf("ws-tracked-%02d", i),
			"title":  fmt.Sprintf("tracked %02d", i),
			"status": status,
		})
	}
	payload := []map[string]any{{
		"id":         id,
		"title":      title,
		"status":     "open",
		"created_at": "2026-09-12T00:00:00Z",
		"tracked":    tracked,
		"completed":  completed,
		"total":      len(tracked),
		"owned":      false,
		"lifecycle":  "system-managed",
	}}
	out, _ := json.Marshal(payload)
	return out
}

func TestNativeConvoySourceCrossRigCountsNotZeroZero(t *testing.T) {
	statuses := append(make([]string, 11), make([]string, 5)...)
	for i := range statuses {
		statuses[i] = "open"
		if i < 11 {
			statuses[i] = "closed"
		}
	}
	runner := &nativeConvoyFakeRunner{
		stdout: nativeConvoyJSON("hq-cv-mgo6k", "Shared Work: campaign", statuses...),
	}
	townRoot := t.TempDir()
	source := newNativeConvoySource(townRoot, runner)
	state, err := source.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(state.InProgress) != 1 {
		t.Fatalf("expected one convoy, got %#v", state)
	}
	convoy := state.InProgress[0]
	if convoy.Completed != 11 || convoy.Total != 16 || len(convoy.Tracked) != 16 {
		t.Fatalf("cross-rig convoy counts = %d/%d with %d tracked, want 11/16 with 16 tracked", convoy.Completed, convoy.Total, len(convoy.Tracked))
	}
	for i, issue := range convoy.Tracked {
		want := fmt.Sprintf("ws-tracked-%02d", i)
		if issue.ID != want {
			t.Errorf("tracked[%d].ID = %q, want %q", i, issue.ID, want)
		}
	}
	if len(runner.calls) != 1 {
		t.Fatalf("native convoy source invoked runner %d times, want one", len(runner.calls))
	}
	call := runner.calls[0]
	if call.executable == "" || strings.Join(call.args, " ") != "convoy list --all --json" {
		t.Fatalf("native invocation = %q %q, want same-binary convoy list --all --json", call.executable, call.args)
	}
	if call.dir != townRoot {
		t.Fatalf("native convoy cwd = %q, want %q", call.dir, townRoot)
	}
	if call.deadline.IsZero() || time.Until(call.deadline) > nativeConvoyCommandTimeout+time.Second {
		t.Fatalf("native convoy runner did not receive bounded deadline: %v", call.deadline)
	}
}

func TestConvoyZeroZeroRequiresExplicitEmptyTracked(t *testing.T) {
	townRoot := t.TempDir()
	valid := &nativeConvoyFakeRunner{stdout: nativeConvoyJSON("hq-cv-empty", "Actually empty")}
	state, err := newNativeConvoySource(townRoot, valid).Fetch(context.Background())
	if err != nil {
		t.Fatalf("explicit empty tracked list rejected: %v", err)
	}
	if got := state.InProgress[0].Completed; got != 0 {
		t.Fatalf("completed = %d, want 0", got)
	}
	if got := state.InProgress[0].Total; got != 0 {
		t.Fatalf("total = %d, want 0", got)
	}

	invalids := [][]byte{
		[]byte(`[{"id":"hq-cv-missing","title":"missing tracked","status":"open","completed":0,"total":0}]`),
		[]byte(`[{"id":"hq-cv-mismatch","title":"mismatch","status":"open","tracked":[],"completed":1,"total":0}]`),
		[]byte(`[{"id":"hq-cv-bad","title":"bad","status":"open","tracked":"not-a-list","completed":0,"total":0}]`),
		[]byte(`{"not":"an array"}`),
		[]byte(`not-json`),
	}
	for i, raw := range invalids {
		t.Run(fmt.Sprintf("invalid-%d", i), func(t *testing.T) {
			runner := &nativeConvoyFakeRunner{stdout: raw}
			if _, err := newNativeConvoySource(townRoot, runner).Fetch(context.Background()); err == nil {
				t.Fatalf("invalid convoy payload was accepted: %s", raw)
			}
		})
	}

	for name, runner := range map[string]*nativeConvoyFakeRunner{
		"command failure": {err: errors.New("command failed")},
		"stderr failure":  {stderr: []byte("database unavailable"), err: errors.New("exit status 1")},
		"output limit":    {stdout: []byte(strings.Repeat("x", maxConvoyCommandOutputBytes+1))},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newNativeConvoySource(townRoot, runner).Fetch(context.Background()); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

func TestConvoyRefreshFailureRetainsLastGoodAndReportsError(t *testing.T) {
	good := nativeConvoyJSON("hq-cv-good", "last good", "open", "closed")
	runner := &nativeConvoyFakeRunner{stdout: good}
	source := newNativeConvoySource(t.TempDir(), runner)
	lastGood, err := source.Fetch(context.Background())
	if err != nil {
		t.Fatalf("initial Fetch: %v", err)
	}

	runner.stdout = []byte(`not-json`)
	if _, err := source.Fetch(context.Background()); err == nil {
		t.Fatal("invalid refresh did not fail visibly")
	}
	if len(lastGood.InProgress) != 1 || lastGood.InProgress[0].Total != 2 {
		t.Fatalf("last good state was cleared or changed: %#v", lastGood)
	}
}

func TestRenderCollapsesOnlyVerifiedSystemSingleTaskWrappers(t *testing.T) {
	m := NewModel(nil)
	m.mu.Lock()
	m.focusedPanel = PanelConvoy

	rows := make([]Convoy, 0, 15)
	for i := 0; i < 10; i++ {
		rows = append(rows, Convoy{
			ID:         fmt.Sprintf("sys-wrapper-%02d", i),
			Title:      fmt.Sprintf("Campaign wrapper %02d", i),
			Status:     "open",
			Completed:  0,
			Total:      1,
			Tracked:    []ConvoyTrackedIssue{{ID: fmt.Sprintf("sys-child-%02d", i), Status: "open"}},
			Owned:      false,
			OwnedKnown: true,
			Lifecycle:  "system-managed",
			MetadataOK: true,
		})
	}
	rows = append(rows,
		Convoy{
			ID:         "caller-owned-single",
			Title:      "Caller-owned single",
			Status:     "open",
			Total:      1,
			Tracked:    []ConvoyTrackedIssue{{ID: "caller-child", Status: "open"}},
			Owned:      true,
			OwnedKnown: true,
			Lifecycle:  "caller-managed",
			MetadataOK: true,
		},
		Convoy{
			ID:         "metadata-unknown-single",
			Title:      "Work: unknown metadata",
			Status:     "open",
			Total:      1,
			Tracked:    []ConvoyTrackedIssue{{ID: "unknown-child", Status: "open"}},
			Owned:      false,
			OwnedKnown: false,
			Lifecycle:  "",
			MetadataOK: false,
		},
		Convoy{
			ID:         "system-managed-multi",
			Title:      "Campaign multi-ticket",
			Status:     "open",
			Total:      2,
			Tracked:    []ConvoyTrackedIssue{{ID: "multi-child-1", Status: "open"}, {ID: "multi-child-2", Status: "open"}},
			Owned:      false,
			OwnedKnown: true,
			Lifecycle:  "system-managed",
			MetadataOK: true,
		},
		Convoy{
			ID:         "campaign-like-wrapper",
			Title:      "Campaign: wrapper title",
			Status:     "open",
			Total:      1,
			Tracked:    []ConvoyTrackedIssue{{ID: "campaign-child", Status: "open"}},
			Owned:      false,
			OwnedKnown: true,
			Lifecycle:  "system-managed",
			MetadataOK: true,
		},
	)
	m.convoyState = &ConvoyState{InProgress: rows, All: rows, LastUpdate: time.Now()}
	defaultView := m.renderConvoys()
	if !strings.Contains(defaultView, "SYSTEM TASKS (11 hidden) 0/11 complete [open:11]") {
		t.Fatalf("default wrapper summary missing counts/status: %s", defaultView)
	}
	for _, want := range []string{"caller-owned-single", "metadata-unknown-single", "system-managed-multi", "[metadata unknown]"} {
		if !strings.Contains(defaultView, want) {
			t.Fatalf("default view omitted visible row %q: %s", want, defaultView)
		}
	}
	for _, hidden := range []string{"sys-wrapper-00", "sys-wrapper-09", "sys-child-00", "sys-child-09", "campaign-like-wrapper"} {
		if strings.Contains(defaultView, hidden) {
			t.Fatalf("default view exposed verified wrapper %q: %s", hidden, defaultView)
		}
	}
	if help := m.renderShortHelp(); !strings.Contains(help, "o:show wrappers") {
		t.Fatalf("default convoy help omitted wrapper toggle: %s", help)
	}
	m.mu.Unlock()

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	m.mu.Lock()
	detailView := m.renderConvoys()
	if !strings.Contains(detailView, "sys-wrapper-00") || !strings.Contains(detailView, "sys-child-00") || !strings.Contains(detailView, "caller-owned-single") {
		t.Fatalf("detail view omitted wrapper/child or visible row: %s", detailView)
	}
	if strings.Contains(detailView, "SYSTEM TASKS (") {
		t.Fatalf("detail view retained collapsed summary: %s", detailView)
	}
	if help := m.renderShortHelp(); !strings.Contains(help, "o:hide wrappers") {
		t.Fatalf("detail convoy help omitted hide toggle: %s", help)
	}
	m.mu.Unlock()

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	m.mu.Lock()
	collapsedAgain := m.renderConvoys()
	m.mu.Unlock()
	if !strings.Contains(collapsedAgain, "SYSTEM TASKS (11 hidden) 0/11 complete [open:11]") {
		t.Fatalf("toggle back changed wrapper counts: %s", collapsedAgain)
	}
}

func nativeClosedConvoyRow(id string, closedAt *time.Time) map[string]any {
	row := map[string]any{
		"id":         id,
		"title":      id,
		"status":     "closed",
		"created_at": "2026-09-11T00:00:00Z",
		"tracked": []map[string]string{{
			"id":     id + "-child",
			"title":  "closed child",
			"status": "closed",
		}},
		"completed": 1,
		"total":     1,
		"owned":     false,
		"lifecycle": "system-managed",
	}
	if closedAt != nil {
		row["closed_at"] = closedAt.Format(time.RFC3339Nano)
	}
	return row
}

func TestConvoyLandedUsesOneCapturedClockAndRecentValidClosedAt(t *testing.T) {
	captured := time.Now().UTC()
	recent := captured.Add(-23 * time.Hour)
	boundary := captured.Add(-24 * time.Hour)
	old := captured.Add(-25 * time.Hour)
	rows := []map[string]any{
		nativeClosedConvoyRow("recent", &recent),
		nativeClosedConvoyRow("boundary", &boundary),
		nativeClosedConvoyRow("old", &old),
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	runner := &nativeConvoyFakeRunner{stdout: raw}
	source := newNativeConvoySource(t.TempDir(), runner)
	state, err := source.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(state.All) != 3 {
		t.Fatalf("All = %d rows, want 3", len(state.All))
	}
	if len(state.Landed) != 1 || state.Landed[0].ID != "recent" {
		t.Fatalf("Landed = %#v, want only recent row", state.Landed)
	}
	if len(state.InProgress) != 0 {
		t.Fatalf("closed rows leaked into InProgress: %#v", state.InProgress)
	}

	for name, closedAt := range map[string]any{
		"missing": nil,
		"invalid": "not-a-timestamp",
	} {
		t.Run(name, func(t *testing.T) {
			row := nativeClosedConvoyRow("bad-"+name, nil)
			if closedAt != nil {
				row["closed_at"] = closedAt
			}
			bad, err := json.Marshal([]map[string]any{row})
			if err != nil {
				t.Fatalf("marshal bad row: %v", err)
			}
			_, err = newNativeConvoySource(t.TempDir(), &nativeConvoyFakeRunner{stdout: bad}).Fetch(context.Background())
			if err == nil {
				t.Fatalf("closed row with %s closed_at was accepted", name)
			}
		})
	}
}

func TestNativeConvoyFetchUsesOneInjectedClock(t *testing.T) {
	captured := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	recent := captured.Add(-23 * time.Hour)
	boundary := captured.Add(-24 * time.Hour)
	rows := []map[string]any{
		nativeClosedConvoyRow("clock-recent", &recent),
		nativeClosedConvoyRow("clock-boundary", &boundary),
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}

	source := newNativeConvoySource(t.TempDir(), &nativeConvoyFakeRunner{stdout: raw})
	clockCalls := 0
	source.now = func() time.Time {
		clockCalls++
		return captured
	}
	state, err := source.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if clockCalls != 1 {
		t.Fatalf("source.now calls = %d, want exactly one", clockCalls)
	}
	if !state.LastUpdate.Equal(captured) {
		t.Fatalf("LastUpdate = %s, want injected clock %s", state.LastUpdate, captured)
	}
	if len(state.Landed) != 1 || state.Landed[0].ID != "clock-recent" {
		t.Fatalf("Landed = %#v, want only recent row at deterministic boundary", state.Landed)
	}
}

func TestNativeConvoyFetchUsesOneClockSource(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(sourceFile), "convoy_issues.go"))
	if err != nil {
		t.Fatalf("read convoy source: %v", err)
	}
	if got := bytes.Count(source, []byte("time.Now()")); got != 1 {
		t.Fatalf("convoy source captures wall clock %d times, want exactly one provider capture", got)
	}
}

func TestConvoyWrapperSummariesUseDisplayedSlices(t *testing.T) {
	open := Convoy{
		ID: "open-wrapper", Title: "Open wrapper", Status: "open", Total: 1,
		Tracked:    []ConvoyTrackedIssue{{ID: "open-child", Status: "open"}},
		OwnedKnown: true, Lifecycle: "system-managed", MetadataOK: true,
	}
	recent := Convoy{
		ID: "recent-wrapper", Title: "Recent wrapper", Status: "closed", Completed: 1, Total: 1,
		ClosedAt: time.Now(), Tracked: []ConvoyTrackedIssue{{ID: "recent-child", Status: "closed"}},
		OwnedKnown: true, Lifecycle: "system-managed", MetadataOK: true,
	}
	historical := Convoy{
		ID: "historical-wrapper", Title: "Historical wrapper", Status: "closed", Completed: 1, Total: 1,
		ClosedAt: time.Now().Add(-48 * time.Hour), Tracked: []ConvoyTrackedIssue{{ID: "historical-child", Status: "closed"}},
		OwnedKnown: true, Lifecycle: "system-managed", MetadataOK: true,
	}
	m := NewModel(nil)
	m.mu.Lock()
	m.focusedPanel = PanelConvoy
	m.convoyState = &ConvoyState{
		InProgress: []Convoy{open},
		Landed:     []Convoy{recent},
		All:        []Convoy{open, recent, historical},
	}
	view := m.renderConvoys()
	m.mu.Unlock()

	landedAt := strings.Index(view, "RECENTLY LANDED")
	if landedAt < 0 {
		t.Fatalf("recently landed section missing: %s", view)
	}
	inProgress := view[:landedAt]
	landed := view[landedAt:]
	if !strings.Contains(inProgress, "SYSTEM TASKS (1 hidden) 0/1 complete [open:1]") {
		t.Fatalf("in-progress summary is not scoped to open displayed slice: %s", inProgress)
	}
	if strings.Contains(inProgress, "closed:1") || strings.Contains(inProgress, "2 hidden") {
		t.Fatalf("in-progress summary included landed/all rows: %s", inProgress)
	}
	if !strings.Contains(landed, "SYSTEM TASKS (1 hidden) 1/1 complete [closed:1]") {
		t.Fatalf("recent-landed summary is not scoped to landed displayed slice: %s", landed)
	}
	if strings.Contains(landed, "historical-wrapper") || strings.Contains(landed, "2 hidden") {
		t.Fatalf("recent-landed summary included excluded historical/all rows: %s", landed)
	}
}
