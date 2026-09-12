package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// The feed invokes the native convoy command at most once per refresh. Keep
// the subprocess and all data it returns bounded so a damaged or wedged town
// cannot stall the TUI or grow its memory without limit.
const (
	nativeConvoyCommandTimeout  = 8 * time.Second
	maxConvoyCommandOutputBytes = 256 * 1024
)

var errNativeConvoyOutputLimit = errors.New("native convoy output exceeded bound")

// nativeConvoyRunner is deliberately small so tests can exercise the source
// without a process-wide command hook. The production runner is the current
// executable and always receives the fixed convoy-list argv below.
type nativeConvoyRunner interface {
	Run(context.Context, string, []string, string) ([]byte, []byte, error)
}

type execNativeConvoyRunner struct{}

type boundedConvoyOutput struct {
	data    bytes.Buffer
	limited bool
	cancel  context.CancelFunc
}

func (w *boundedConvoyOutput) Write(p []byte) (int, error) {
	remaining := maxConvoyCommandOutputBytes - w.data.Len()
	if remaining <= 0 {
		w.limited = true
		if w.cancel != nil {
			w.cancel()
		}
		return 0, errNativeConvoyOutputLimit
	}
	if len(p) > remaining {
		_, _ = w.data.Write(p[:remaining])
		w.limited = true
		if w.cancel != nil {
			w.cancel()
		}
		return remaining, errNativeConvoyOutputLimit
	}
	return w.data.Write(p)
}

func (execNativeConvoyRunner) Run(parent context.Context, executable string, args []string, dir string) ([]byte, []byte, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	cmd := exec.CommandContext(ctx, executable, args...) //nolint:gosec // executable and argv are fixed by native source
	util.SetProcessGroup(cmd)
	cmd.Dir = dir
	var stdout, stderr boundedConvoyOutput
	stdout.cancel = cancel
	stderr.cancel = cancel
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if stdout.limited || stderr.limited {
		return stdout.data.Bytes(), stderr.data.Bytes(), errNativeConvoyOutputLimit
	}
	return stdout.data.Bytes(), stderr.data.Bytes(), err
}

type nativeConvoySource struct {
	townRoot string
	runner   nativeConvoyRunner
	now      func() time.Time
}

func nativeConvoyNow() time.Time {
	return time.Now().UTC()
}

func newNativeConvoySource(townRoot string, runner nativeConvoyRunner) *nativeConvoySource {
	if runner == nil {
		runner = execNativeConvoyRunner{}
	}
	return &nativeConvoySource{
		townRoot: townRoot,
		runner:   runner,
		now:      nativeConvoyNow,
	}
}

// Fetch obtains one authoritative `gt convoy list --all --json` snapshot.
// Errors are returned to the model so it can retain and label the last good
// snapshot; they are never converted to an empty 0/0 state.
func (s *nativeConvoySource) Fetch(parent context.Context) (*ConvoyState, error) {
	if parent == nil {
		parent = context.Background()
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locating native gt executable: %w", err)
	}

	ctx, cancel := context.WithTimeout(parent, nativeConvoyCommandTimeout)
	defer cancel()
	stdout, stderr, runErr := s.runner.Run(ctx, executable,
		[]string{"convoy", "list", "--all", "--json"}, s.townRoot)
	if len(stdout) > maxConvoyCommandOutputBytes || len(stderr) > maxConvoyCommandOutputBytes || errors.Is(runErr, errNativeConvoyOutputLimit) {
		return nil, fmt.Errorf("native convoy list output exceeded %d-byte bound", maxConvoyCommandOutputBytes)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("native convoy list timed out after %s", nativeConvoyCommandTimeout)
	}
	if runErr != nil {
		detail := strings.TrimSpace(string(stderr))
		if detail != "" {
			return nil, fmt.Errorf("native convoy list failed: %s", detail)
		}
		return nil, fmt.Errorf("native convoy list failed: %w", runErr)
	}

	records, err := decodeNativeConvoys(stdout)
	if err != nil {
		return nil, err
	}
	clock := s.now
	if clock == nil {
		clock = nativeConvoyNow
	}
	now := clock().UTC()
	cutoff := now.Add(-24 * time.Hour)
	state := &ConvoyState{
		InProgress: make([]Convoy, 0, len(records)),
		Landed:     make([]Convoy, 0),
		All:        make([]Convoy, 0, len(records)),
		LastUpdate: now,
	}
	for _, record := range records {
		convoy := record.convoy()
		state.All = append(state.All, convoy)
		if convoy.Status == "closed" {
			if convoy.ClosedAt.IsZero() {
				return nil, fmt.Errorf("native convoy list row %q has closed status without a valid closed_at", convoy.ID)
			}
			if convoy.ClosedAt.After(cutoff) {
				state.Landed = append(state.Landed, convoy)
			}
		} else {
			state.InProgress = append(state.InProgress, convoy)
		}
	}
	sortConvoyState(state)
	// Merge queue data remains independent of the convoy authority. A failed
	// queue probe must not erase a valid native convoy snapshot.
	state.MQEntries = fetchMQEntries(s.townRoot)
	return state, nil
}

type nativeTrackedConvoy struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status"`
}

type nativeConvoyRecord struct {
	ID            string
	Title         string
	Status        string
	CreatedAt     time.Time
	ClosedAt      time.Time
	Tracked       []nativeTrackedConvoy
	Completed     int
	Total         int
	Owned         bool
	OwnedPresent  bool
	Lifecycle     string
	LifecycleSeen bool
}

func decodeNativeConvoys(raw []byte) ([]nativeConvoyRecord, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errors.New("native convoy list returned invalid JSON array")
	}
	var values []json.RawMessage
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return nil, fmt.Errorf("native convoy list returned invalid JSON: %w", err)
	}

	records := make([]nativeConvoyRecord, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		record, err := decodeNativeConvoy(index, value)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[record.ID]; ok {
			return nil, fmt.Errorf("native convoy list row %d repeats convoy id %q", index, record.ID)
		}
		seen[record.ID] = struct{}{}
		records = append(records, record)
	}
	return records, nil
}

func decodeNativeConvoy(index int, value json.RawMessage) (nativeConvoyRecord, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil || object == nil {
		return nativeConvoyRecord{}, fmt.Errorf("native convoy list row %d is not an object", index)
	}
	getString := func(name string, required bool) (string, error) {
		raw, ok := object[name]
		if !ok {
			if required {
				return "", fmt.Errorf("native convoy list row %d is missing %s", index, name)
			}
			return "", nil
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("native convoy list row %d has invalid %s", index, name)
		}
		return value, nil
	}
	getInt := func(name string) (int, error) {
		raw, ok := object[name]
		if !ok {
			return 0, fmt.Errorf("native convoy list row %d is missing %s", index, name)
		}
		var value int
		if err := json.Unmarshal(raw, &value); err != nil {
			return 0, fmt.Errorf("native convoy list row %d has invalid %s", index, name)
		}
		return value, nil
	}

	id, err := getString("id", true)
	if err != nil {
		return nativeConvoyRecord{}, err
	}
	title, err := getString("title", true)
	if err != nil {
		return nativeConvoyRecord{}, err
	}
	status, err := getString("status", true)
	if err != nil {
		return nativeConvoyRecord{}, err
	}
	if !knownConvoyStatus(status) {
		return nativeConvoyRecord{}, fmt.Errorf("native convoy list row %d has unsupported status %q", index, status)
	}
	createdRaw, err := getString("created_at", true)
	if err != nil {
		return nativeConvoyRecord{}, err
	}
	createdAt, err := parseNativeConvoyTime(createdRaw)
	if err != nil {
		return nativeConvoyRecord{}, fmt.Errorf("native convoy list row %d has invalid created_at: %w", index, err)
	}
	closedAt := time.Time{}
	if closedRaw, ok := object["closed_at"]; ok {
		var closedValue string
		if err := json.Unmarshal(closedRaw, &closedValue); err != nil {
			return nativeConvoyRecord{}, fmt.Errorf("native convoy list row %d has invalid closed_at", index)
		}
		if strings.TrimSpace(closedValue) != "" {
			closedAt, err = parseNativeConvoyTime(closedValue)
			if err != nil {
				return nativeConvoyRecord{}, fmt.Errorf("native convoy list row %d has invalid closed_at: %w", index, err)
			}
		}
	}

	completed, err := getInt("completed")
	if err != nil {
		return nativeConvoyRecord{}, err
	}
	total, err := getInt("total")
	if err != nil {
		return nativeConvoyRecord{}, err
	}
	if completed < 0 || total < 0 || completed > total {
		return nativeConvoyRecord{}, fmt.Errorf("native convoy list row %d has impossible counts %d/%d", index, completed, total)
	}

	trackedRaw, ok := object["tracked"]
	if !ok {
		return nativeConvoyRecord{}, fmt.Errorf("native convoy list row %d is missing tracked", index)
	}
	var tracked []nativeTrackedConvoy
	if err := json.Unmarshal(trackedRaw, &tracked); err != nil || tracked == nil {
		return nativeConvoyRecord{}, fmt.Errorf("native convoy list row %d has invalid tracked", index)
	}
	seenTracked := make(map[string]struct{}, len(tracked))
	closedCount := 0
	for childIndex, child := range tracked {
		if strings.TrimSpace(child.ID) == "" || strings.TrimSpace(child.Status) == "" {
			return nativeConvoyRecord{}, fmt.Errorf("native convoy list row %d tracked item %d has invalid id/status", index, childIndex)
		}
		if !knownTrackedStatus(child.Status) {
			return nativeConvoyRecord{}, fmt.Errorf("native convoy list row %d tracked item %d has unsupported status %q", index, childIndex, child.Status)
		}
		if _, exists := seenTracked[child.ID]; exists {
			return nativeConvoyRecord{}, fmt.Errorf("native convoy list row %d repeats tracked id %q", index, child.ID)
		}
		seenTracked[child.ID] = struct{}{}
		if child.Status == "closed" {
			closedCount++
		}
	}
	if len(tracked) != total || closedCount != completed {
		return nativeConvoyRecord{}, fmt.Errorf("native convoy list row %d count mismatch: native %d/%d, tracked %d/%d", index, completed, total, closedCount, len(tracked))
	}

	record := nativeConvoyRecord{
		ID:        id,
		Title:     title,
		Status:    status,
		CreatedAt: createdAt,
		ClosedAt:  closedAt,
		Tracked:   tracked,
		Completed: completed,
		Total:     total,
	}
	if rawOwned, exists := object["owned"]; exists {
		var owned bool
		if err := json.Unmarshal(rawOwned, &owned); err == nil && string(bytes.TrimSpace(rawOwned)) != "null" {
			record.Owned = owned
			record.OwnedPresent = true
		}
	}
	if rawLifecycle, exists := object["lifecycle"]; exists {
		var lifecycle string
		if err := json.Unmarshal(rawLifecycle, &lifecycle); err == nil && strings.TrimSpace(lifecycle) != "" {
			record.Lifecycle = lifecycle
			record.LifecycleSeen = true
		}
	}
	if record.LifecycleSeen && record.Lifecycle != "system-managed" && record.Lifecycle != "caller-managed" {
		// Preserve this row but make it explicitly unverified at render time.
		record.LifecycleSeen = false
		record.Lifecycle = ""
	}
	if record.OwnedPresent && record.LifecycleSeen {
		consistent := (record.Owned && record.Lifecycle == "caller-managed") || (!record.Owned && record.Lifecycle == "system-managed")
		if !consistent {
			record.LifecycleSeen = false
		}
	}
	return record, nil
}

func parseNativeConvoyTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp %q", value)
}

func knownConvoyStatus(status string) bool {
	switch status {
	case "open", "closed", "in_progress", "tombstone", "hooked", "pinned", "deferred", "blocked", "staged_ready", "staged_warnings":
		return true
	default:
		return false
	}
}

func knownTrackedStatus(status string) bool {
	switch status {
	case "open", "closed", "in_progress", "tombstone", "hooked", "pinned", "deferred", "blocked", "unknown", "staged_ready", "staged_warnings":
		return true
	default:
		return false
	}
}

func (r nativeConvoyRecord) convoy() Convoy {
	tracked := make([]ConvoyTrackedIssue, 0, len(r.Tracked))
	for _, issue := range r.Tracked {
		tracked = append(tracked, ConvoyTrackedIssue{ID: issue.ID, Title: issue.Title, Status: issue.Status})
	}
	return Convoy{
		ID:         r.ID,
		Title:      r.Title,
		Status:     r.Status,
		Completed:  r.Completed,
		Total:      r.Total,
		CreatedAt:  r.CreatedAt,
		ClosedAt:   r.ClosedAt,
		Tracked:    tracked,
		Owned:      r.Owned,
		OwnedKnown: r.OwnedPresent,
		Lifecycle:  r.Lifecycle,
		MetadataOK: r.OwnedPresent && r.LifecycleSeen,
	}
}
