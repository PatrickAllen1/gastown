package feed

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/util"
)

// EventSource represents a source of events
type EventSource interface {
	Events() <-chan Event
	Close() error
}

// BdActivitySource reads events from bd activity --follow
type BdActivitySource struct {
	cmd     *exec.Cmd
	events  chan Event
	cancel  context.CancelFunc
	workDir string
}

// NewBdActivitySource creates a new source that tails bd activity
func NewBdActivitySource(workDir string) (*BdActivitySource, error) {
	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, "bd", "activity", "--follow")
	util.SetDetachedProcessGroup(cmd)
	cmd.Dir = workDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}

	source := &BdActivitySource{
		cmd:     cmd,
		events:  make(chan Event, 100),
		cancel:  cancel,
		workDir: workDir,
	}

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if event := parseBdActivityLine(line); event != nil {
				select {
				case source.events <- *event:
				default:
					// Drop event if channel full
				}
			}
		}
		close(source.events)
	}()

	return source, nil
}

// Events returns the event channel
func (s *BdActivitySource) Events() <-chan Event {
	return s.events
}

// Close stops the source
func (s *BdActivitySource) Close() error {
	s.cancel()
	return s.cmd.Wait()
}

// bd activity line pattern: [HH:MM:SS] SYMBOL BEAD_ID action · description
var bdActivityPattern = regexp.MustCompile(`^\[(\d{2}:\d{2}:\d{2})\]\s+([+→✓✗⊘📌])\s+(\S+)?\s*(\w+)?\s*·?\s*(.*)$`)

// parseBdActivityLine parses a line from bd activity output
func parseBdActivityLine(line string) *Event {
	matches := bdActivityPattern.FindStringSubmatch(line)
	if matches == nil {
		// Try simpler pattern
		return parseSimpleLine(line)
	}

	timeStr := matches[1]
	symbol := matches[2]
	beadID := matches[3]
	action := matches[4]
	message := matches[5]

	// Parse time (assume today)
	now := time.Now()
	t, err := time.Parse("15:04:05", timeStr)
	if err != nil {
		t = now
	} else {
		t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, now.Location())
	}

	// Map symbol to event type
	eventType := "update"
	switch symbol {
	case "+":
		eventType = "create"
	case "→":
		eventType = "update"
	case "✓":
		eventType = "complete"
	case "✗":
		eventType = "fail"
	case "⊘":
		eventType = "delete"
	case "📌":
		eventType = "pin"
	}

	// Try to extract actor and rig from bead ID
	actor, rig, role := parseBeadContext(beadID)

	return &Event{
		Time:    t,
		Type:    eventType,
		Actor:   actor,
		Target:  beadID,
		Message: strings.TrimSpace(action + " " + message),
		Rig:     rig,
		Role:    role,
		Raw:     line,
	}
}

// parseSimpleLine handles lines that don't match the full pattern
func parseSimpleLine(line string) *Event {
	if strings.TrimSpace(line) == "" {
		return nil
	}

	// Try to extract timestamp
	var t time.Time
	if len(line) > 10 && line[0] == '[' {
		if idx := strings.Index(line, "]"); idx > 0 {
			timeStr := line[1:idx]
			now := time.Now()
			if parsed, err := time.Parse("15:04:05", timeStr); err == nil {
				t = time.Date(now.Year(), now.Month(), now.Day(),
					parsed.Hour(), parsed.Minute(), parsed.Second(), 0, now.Location())
			}
		}
	}

	if t.IsZero() {
		t = time.Now()
	}

	return &Event{
		Time:    t,
		Type:    "update",
		Message: line,
		Raw:     line,
	}
}

// parseBeadContext extracts actor/rig/role from a bead ID
// Uses canonical naming: prefix-rig-role-name
// Examples: gt-gastown-crew-joe, gt-gastown-witness, gt-mayor
func parseBeadContext(beadID string) (actor, rig, role string) {
	if beadID == "" {
		return
	}

	// Use the canonical parser
	parsedRig, parsedRole, name, ok := beads.ParseAgentBeadID(beadID)
	if !ok {
		return
	}

	rig = parsedRig
	role = parsedRole

	// Build actor identifier
	switch parsedRole {
	case constants.RoleMayor, constants.RoleDeacon:
		actor = parsedRole
	case constants.RoleWitness, constants.RoleRefinery:
		actor = parsedRole
	case constants.RoleCrew:
		if name != "" {
			actor = parsedRig + "/crew/" + name
		} else {
			actor = parsedRole
		}
	case constants.RolePolecat:
		if name != "" {
			actor = parsedRig + "/" + name
		} else {
			actor = parsedRole
		}
	}

	return
}

// The raw events file is append-only in the normal case, but it is also
// rotated and copy-truncated by operators. Keep every read bounded and use
// the file identity plus a small trailing boundary to distinguish append from
// replacement.
const (
	gtEventPreloadBytes  = 1024 * 1024
	gtEventPollBytes     = 64 * 1024
	gtEventRecordBytes   = 128 * 1024
	gtEventDigestCap     = 200
	gtEventDiagnosticCap = 64
	gtEventPollInterval  = 200 * time.Millisecond
	gtEventBoundaryBytes = 64
)

// eventCursor owns all mutable file-reading state. It is only touched by the
// GtEventsSource tail goroutine after construction, which lets Close wait for
// the reader before closing its descriptor.
type eventCursor struct {
	path string
	file *os.File

	offset          int64
	partial         []byte
	discardOversize bool
	skipLeading     bool
	boundary        []byte

	recentDigests map[string]struct{}
	digestOrder   []string

	initialReadBytes int
	recovery         bool
	recoveryEnd      int64

	pendingEvents      []Event
	pendingDiagnostics []error
	diagnosticCount    int
	diagnosticDropped  int
	diagnosticSample   string
	lastPathDiagnostic string
}

// GtEventsSource reads events from ~/gt/.events.jsonl (gt activity log).
type GtEventsSource struct {
	*eventCursor
	events      chan Event
	diagnostics chan error
	cancel      context.CancelFunc
	done        chan struct{}
	closeOnce   sync.Once
	closeErr    error
}

// GtEvent is the structure of events in .events.jsonl
type GtEvent struct {
	Timestamp  string                 `json:"ts"`
	Source     string                 `json:"source"`
	Type       string                 `json:"type"`
	Actor      string                 `json:"actor"`
	Payload    map[string]interface{} `json:"payload"`
	Visibility string                 `json:"visibility"`
}

// NewGtEventsSource creates a source that tails ~/gt/.events.jsonl
func NewGtEventsSource(townRoot string) (*GtEventsSource, error) {
	eventsPath := filepath.Join(townRoot, ".events.jsonl")
	pathInfo, err := regularEventFileInfo(eventsPath)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(eventsPath)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !os.SameFile(pathInfo, openedInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("events file changed while opening %s", eventsPath)
	}
	cursor, err := newEventCursor(eventsPath, file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	source := &GtEventsSource{
		eventCursor: cursor,
		events:      make(chan Event, gtEventDigestCap),
		diagnostics: make(chan error, gtEventDiagnosticCap),
		cancel:      cancel,
		done:        make(chan struct{}),
	}

	go source.tail(ctx)

	return source, nil
}

// regularEventFileInfo rejects symlinks and special files. Refusing a
// symlink is intentional: following one could make an operator's feed read
// an unexpected file after rotation or replacement.
func regularEventFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("events path %s is a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("events path %s is not a regular file", path)
	}
	return info, nil
}

func newEventCursor(path string, file *os.File) (*eventCursor, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	cursor := &eventCursor{
		path:          path,
		file:          file,
		recentDigests: make(map[string]struct{}, gtEventDigestCap),
	}
	start := info.Size() - gtEventPreloadBytes
	if start < 0 {
		start = 0
	}
	cursor.offset = start
	if start > 0 {
		var marker [1]byte
		if n, readErr := file.ReadAt(marker[:], start-1); n == 1 && readErr == nil {
			cursor.skipLeading = marker[0] != '\n'
		} else {
			// A failed boundary read is safer as a discarded fragment than as a
			// possibly duplicated partial record.
			cursor.skipLeading = true
		}
	}
	readBytes := info.Size() - start
	if readBytes > 0 {
		data := make([]byte, readBytes)
		n, readErr := file.ReadAt(data, start)
		if n > 0 {
			cursor.initialReadBytes = n
			cursor.offset = start + int64(n)
			cursor.consume(data[:n])
		}
		if readErr != nil && readErr != io.EOF {
			return nil, fmt.Errorf("reading initial events window: %w", readErr)
		}
	}
	cursor.updateBoundary()
	return cursor, nil
}

func (c *eventCursor) addDiagnostic(err error) {
	if err == nil {
		return
	}
	c.diagnosticCount++
	if c.diagnosticSample == "" {
		c.diagnosticSample = err.Error()
	}
	if len(c.pendingDiagnostics) < gtEventDiagnosticCap {
		c.pendingDiagnostics = append(c.pendingDiagnostics, err)
	} else {
		c.diagnosticDropped++
	}
}

func (c *eventCursor) takeDiagnosticEvent() *Event {
	if c.diagnosticCount == 0 {
		return nil
	}
	message := fmt.Sprintf("diagnostic: %d issue(s)", c.diagnosticCount)
	if c.diagnosticCount > 1 {
		message += " (coalesced)"
	}
	message += fmt.Sprintf("; dropped %d", c.diagnosticDropped)
	if c.diagnosticSample != "" {
		message += ": " + c.diagnosticSample
	}
	c.diagnosticCount = 0
	c.diagnosticDropped = 0
	c.diagnosticSample = ""
	return &Event{
		Time:    time.Now(),
		Type:    "diagnostic",
		Message: message,
		Raw:     message,
	}
}

func (c *eventCursor) rememberDigest(raw []byte) bool {
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	if _, exists := c.recentDigests[digest]; exists {
		return false
	}
	c.recentDigests[digest] = struct{}{}
	c.digestOrder = append(c.digestOrder, digest)
	if len(c.digestOrder) > gtEventDigestCap {
		oldest := c.digestOrder[0]
		c.digestOrder = c.digestOrder[1:]
		delete(c.recentDigests, oldest)
	}
	return true
}

func (c *eventCursor) queueEvent(event Event, raw []byte) {
	if c.recovery {
		if !c.rememberDigest(raw) {
			return
		}
	} else {
		_ = c.rememberDigest(raw)
	}
	if len(c.pendingEvents) == gtEventDigestCap {
		copy(c.pendingEvents, c.pendingEvents[1:])
		c.pendingEvents = c.pendingEvents[:gtEventDigestCap-1]
	}
	c.pendingEvents = append(c.pendingEvents, event)
}

func (c *eventCursor) processLine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	if !utf8.Valid(trimmed) {
		c.addDiagnostic(fmt.Errorf("malformed events record: invalid UTF-8"))
		return
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
		c.addDiagnostic(fmt.Errorf("malformed events record: %v", err))
		return
	}
	var visibility string
	if raw, ok := object["visibility"]; !ok || json.Unmarshal(raw, &visibility) != nil {
		c.addDiagnostic(fmt.Errorf("events record has invalid visibility"))
		return
	}
	if visibility == "audit" {
		// Audit-only rows are valid in the raw log but intentionally filtered
		// from the feed. They are not malformed and must not consume diagnostic
		// capacity.
		return
	}
	if visibility != "feed" && visibility != "both" {
		c.addDiagnostic(fmt.Errorf("events record has invalid visibility"))
		return
	}
	event := parseGtEventLine(string(line))
	if event == nil {
		c.addDiagnostic(fmt.Errorf("malformed events record"))
		return
	}
	c.queueEvent(*event, line)
}

// consume accepts arbitrary chunks and emits only complete newline-delimited
// records. The partial and oversize buffers never grow beyond the record
// limit; an oversize record is discarded through its next newline.
func (c *eventCursor) consume(data []byte) {
	if len(data) == 0 {
		return
	}
	if len(c.partial) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			if len(c.partial)+len(data) > gtEventRecordBytes {
				c.addDiagnostic(fmt.Errorf("events record exceeds %d-byte bound", gtEventRecordBytes))
				c.partial = nil
				c.discardOversize = true
				return
			}
			c.partial = append(c.partial, data...)
			return
		}
		if len(c.partial)+newline > gtEventRecordBytes {
			c.addDiagnostic(fmt.Errorf("events record exceeds %d-byte bound", gtEventRecordBytes))
			c.partial = nil
			data = data[newline+1:]
		} else {
			line := make([]byte, 0, len(c.partial)+newline)
			line = append(line, c.partial...)
			line = append(line, data[:newline]...)
			c.partial = nil
			if !c.skipLeading && !c.discardOversize {
				c.processLine(line)
			}
			c.skipLeading = false
			data = data[newline+1:]
		}
	}
	for len(data) > 0 {
		if c.skipLeading {
			newline := bytes.IndexByte(data, '\n')
			if newline < 0 {
				// This is the fragment before the bounded recovery window. Do
				// not retain it or accidentally join it to a new record.
				return
			}
			data = data[newline+1:]
			c.skipLeading = false
			continue
		}
		if c.discardOversize {
			newline := bytes.IndexByte(data, '\n')
			if newline < 0 {
				return
			}
			data = data[newline+1:]
			c.discardOversize = false
			continue
		}
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			if len(data) > gtEventRecordBytes {
				c.addDiagnostic(fmt.Errorf("events record exceeds %d-byte bound", gtEventRecordBytes))
				c.discardOversize = true
				return
			}
			c.partial = append([]byte(nil), data...)
			return
		}
		line := data[:newline]
		data = data[newline+1:]
		if len(line) > gtEventRecordBytes {
			c.addDiagnostic(fmt.Errorf("events record exceeds %d-byte bound", gtEventRecordBytes))
			continue
		}
		c.processLine(line)
	}
}

func (c *eventCursor) updateBoundary() {
	if c.offset <= 0 {
		c.boundary = nil
		return
	}
	n := int64(gtEventBoundaryBytes)
	if c.offset < n {
		n = c.offset
	}
	boundary := make([]byte, n)
	read, err := c.file.ReadAt(boundary, c.offset-n)
	if read > 0 && (err == nil || err == io.EOF) {
		c.boundary = append(c.boundary[:0], boundary[:read]...)
	}
}

func (c *eventCursor) boundaryChanged(size int64) bool {
	if c.offset <= 0 || len(c.boundary) == 0 {
		return false
	}
	if size < c.offset {
		return true
	}
	start := c.offset - int64(len(c.boundary))
	got := make([]byte, len(c.boundary))
	read, err := c.file.ReadAt(got, start)
	if read != len(got) || (err != nil && err != io.EOF) {
		return true
	}
	return !bytes.Equal(got, c.boundary)
}

// drainCurrentFile finishes the complete records that were appended to the
// descriptor we have been following. Rotation can rename that inode before a
// poll observes it; those rows are still authoritative and must be consumed
// before the descriptor is closed. The read remains bounded per iteration and
// the existing pending-event cap preserves the source's memory bound.
func (c *eventCursor) drainCurrentFile() (int64, []byte, error) {
	info, err := c.file.Stat()
	if err != nil {
		return 0, nil, err
	}
	target := info.Size()
	if target < c.offset {
		// The old inode was truncated before it was renamed. There is no
		// complete row left to recover from its prior offset.
		c.offset = target
		c.partial = nil
		c.discardOversize = false
		c.skipLeading = false
		c.updateBoundary()
		return c.offset, append([]byte(nil), c.boundary...), nil
	}
	for c.offset < target {
		amount := target - c.offset
		if amount > gtEventPollBytes {
			amount = gtEventPollBytes
		}
		data := make([]byte, amount)
		read, readErr := c.file.ReadAt(data, c.offset)
		if read > 0 {
			c.offset += int64(read)
			c.consume(data[:read])
		}
		if readErr != nil && readErr != io.EOF {
			return 0, nil, fmt.Errorf("reading renamed events file: %w", readErr)
		}
		if read == 0 {
			break
		}
	}
	c.updateBoundary()
	boundary := append([]byte(nil), c.boundary...)
	// An incomplete final row cannot be joined to a different inode. Keep
	// complete rows already queued, but discard only this old fragment.
	c.partial = nil
	c.discardOversize = false
	c.skipLeading = false
	return c.offset, boundary, nil
}

// recoveryStartOffset skips a replacement's copied prefix only when its
// bytes prove that the old inode's complete prefix is still present. If the
// replacement is unrelated (including a large file with new rows at offset
// zero), recovery starts at zero so no unseen complete row is skipped.
func recoveryStartOffset(file *os.File, replacementSize, oldSize int64, oldBoundary []byte) int64 {
	if oldSize <= 0 || len(oldBoundary) == 0 || oldSize > replacementSize {
		return 0
	}
	start := oldSize - int64(len(oldBoundary))
	got := make([]byte, len(oldBoundary))
	read, err := file.ReadAt(got, start)
	if read == len(got) && (err == nil || err == io.EOF) && bytes.Equal(got, oldBoundary) {
		return oldSize
	}
	return 0
}

func (c *eventCursor) beginRecovery(info os.FileInfo) error {
	file, err := os.Open(c.path)
	if err != nil {
		return err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return fmt.Errorf("events path changed while recovering")
	}
	oldFile := c.file
	oldInfo, oldStatErr := oldFile.Stat()
	sameFile := oldStatErr == nil && os.SameFile(info, oldInfo)
	oldSize := int64(0)
	var oldBoundary []byte
	if !sameFile {
		oldSize, oldBoundary, err = c.drainCurrentFile()
		if err != nil {
			_ = file.Close()
			return err
		}
	}
	c.file = file
	_ = oldFile.Close()
	start := int64(0)
	if !sameFile {
		start = recoveryStartOffset(file, openedInfo.Size(), oldSize, oldBoundary)
	}
	c.offset = start
	c.partial = nil
	c.discardOversize = false
	c.skipLeading = false
	c.boundary = nil
	c.recovery = true
	c.recoveryEnd = openedInfo.Size()
	if start > 0 {
		var marker [1]byte
		if n, readErr := file.ReadAt(marker[:], start-1); n == 1 && readErr == nil {
			c.skipLeading = marker[0] != '\n'
		} else {
			c.skipLeading = true
		}
	}
	return nil
}

func (c *eventCursor) ensureCurrentFile() error {
	info, err := regularEventFileInfo(c.path)
	if err != nil {
		return err
	}
	current, err := c.file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) || info.Size() < c.offset || c.boundaryChanged(info.Size()) {
		return c.beginRecovery(info)
	}
	return nil
}

func (c *eventCursor) readAvailable() {
	if err := c.ensureCurrentFile(); err != nil {
		message := err.Error()
		if message != c.lastPathDiagnostic {
			c.addDiagnostic(fmt.Errorf("events tail unavailable: %w", err))
			c.lastPathDiagnostic = message
		}
		return
	}
	c.lastPathDiagnostic = ""
	info, err := c.file.Stat()
	if err != nil {
		c.addDiagnostic(fmt.Errorf("stat events file: %w", err))
		return
	}
	targetSize := info.Size()
	if c.recovery && c.recoveryEnd < targetSize {
		targetSize = c.recoveryEnd
	}
	if targetSize > c.offset {
		amount := targetSize - c.offset
		if amount > gtEventPollBytes {
			amount = gtEventPollBytes
		}
		data := make([]byte, amount)
		read, readErr := c.file.ReadAt(data, c.offset)
		if read > 0 {
			c.offset += int64(read)
			c.consume(data[:read])
		}
		if readErr != nil && readErr != io.EOF {
			c.addDiagnostic(fmt.Errorf("reading events file: %w", readErr))
		}
	}
	c.updateBoundary()
	if c.recovery && c.offset >= c.recoveryEnd {
		c.recovery = false
	}
}

// tail loads the bounded history and then follows the file for new events.
func (s *GtEventsSource) tail(ctx context.Context) {
	defer close(s.events)
	defer close(s.diagnostics)
	defer close(s.done)

	flush := func() bool {
		for len(s.pendingDiagnostics) > 0 {
			diagnostic := s.pendingDiagnostics[0]
			select {
			case s.diagnostics <- diagnostic:
				s.pendingDiagnostics = s.pendingDiagnostics[1:]
			default:
				// Diagnostics are advisory. Never let an undrained side channel
				// stop the event stream; account for everything that could not
				// fit in its bounded channel in the production summary below.
				s.diagnosticDropped += len(s.pendingDiagnostics)
				s.pendingDiagnostics = nil
			case <-ctx.Done():
				return false
			}
		}
		if diagnostic := s.takeDiagnosticEvent(); diagnostic != nil {
			select {
			case s.events <- *diagnostic:
			case <-ctx.Done():
				return false
			}
		}
		for len(s.pendingEvents) > 0 {
			event := s.pendingEvents[0]
			select {
			case s.events <- event:
				s.pendingEvents = s.pendingEvents[1:]
			case <-ctx.Done():
				return false
			}
		}
		return true
	}
	if !flush() {
		return
	}
	ticker := time.NewTicker(gtEventPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.readAvailable()
			if !flush() {
				return
			}
		}
	}
}

// Events returns the event channel
func (s *GtEventsSource) Events() <-chan Event {
	return s.events
}

// Diagnostics reports malformed records, unsafe path changes, and bounded
// read errors without stopping the event stream.
func (s *GtEventsSource) Diagnostics() <-chan error {
	return s.diagnostics
}

// Close stops the source
func (s *GtEventsSource) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		<-s.done
		s.closeErr = s.file.Close()
	})
	return s.closeErr
}

// parseGtEventLine parses a line from .events.jsonl
func parseGtEventLine(line string) *Event {
	if strings.TrimSpace(line) == "" {
		return nil
	}

	var ge GtEvent
	if err := json.Unmarshal([]byte(line), &ge); err != nil {
		return nil
	}

	// Only show feed-visible events
	if ge.Visibility != "feed" && ge.Visibility != "both" {
		return nil
	}

	t, err := time.Parse(time.RFC3339, ge.Timestamp)
	if err != nil {
		t = time.Now()
	}

	// Extract rig from payload or actor
	rig := ""
	if ge.Payload != nil {
		if r, ok := ge.Payload["rig"].(string); ok {
			rig = r
		}
	}
	if rig == "" && ge.Actor != "" {
		// Extract rig from actor like "gastown/witness"
		parts := strings.Split(ge.Actor, "/")
		if len(parts) > 0 && parts[0] != constants.RoleMayor && parts[0] != constants.RoleDeacon {
			rig = parts[0]
		}
	}

	// Extract role from actor
	role := ""
	if ge.Actor != "" {
		parts := strings.Split(ge.Actor, "/")
		if len(parts) >= 2 {
			role = parts[len(parts)-1]
			// Check for known roles
			switch parts[len(parts)-1] {
			case constants.RoleWitness, constants.RoleRefinery:
				role = parts[len(parts)-1]
			default:
				// Could be polecat name - check second-to-last part
				if len(parts) >= 2 {
					switch parts[len(parts)-2] {
					case "polecats":
						role = constants.RolePolecat
					case constants.RoleCrew:
						role = constants.RoleCrew
					}
				}
			}
		} else if len(parts) == 1 {
			role = parts[0]
		}
	}

	// Build message from event type and payload
	message := buildEventMessage(ge.Type, ge.Payload)

	return &Event{
		Time:    t,
		Type:    ge.Type,
		Actor:   ge.Actor,
		Target:  getPayloadString(ge.Payload, "bead"),
		Message: message,
		Rig:     rig,
		Role:    role,
		Raw:     line,
	}
}

// buildEventMessage creates a human-readable message from event type and payload
func buildEventMessage(eventType string, payload map[string]interface{}) string {
	switch eventType {
	case "patrol_started":
		count := getPayloadInt(payload, "polecat_count")
		if msg := getPayloadString(payload, "message"); msg != "" {
			return msg
		}
		if count > 0 {
			return fmt.Sprintf("patrol started (%d polecats)", count)
		}
		return "patrol started"

	case "patrol_complete":
		count := getPayloadInt(payload, "polecat_count")
		if msg := getPayloadString(payload, "message"); msg != "" {
			return msg
		}
		if count > 0 {
			return fmt.Sprintf("patrol complete (%d polecats)", count)
		}
		return "patrol complete"

	case "polecat_checked":
		polecat := getPayloadString(payload, "polecat")
		status := getPayloadString(payload, "status")
		if polecat != "" {
			if status != "" {
				return fmt.Sprintf("checked %s (%s)", polecat, status)
			}
			return fmt.Sprintf("checked %s", polecat)
		}
		return "polecat checked"

	case "polecat_nudged":
		polecat := getPayloadString(payload, "polecat")
		reason := getPayloadString(payload, "reason")
		if polecat != "" {
			if reason != "" {
				return fmt.Sprintf("nudged %s: %s", polecat, reason)
			}
			return fmt.Sprintf("nudged %s", polecat)
		}
		return "polecat nudged"

	case "escalation_sent":
		target := getPayloadString(payload, "target")
		to := getPayloadString(payload, "to")
		reason := getPayloadString(payload, "reason")
		if target != "" && to != "" {
			if reason != "" {
				return fmt.Sprintf("escalated %s to %s: %s", target, to, reason)
			}
			return fmt.Sprintf("escalated %s to %s", target, to)
		}
		return "escalation sent"

	case "sling":
		bead := getPayloadString(payload, "bead")
		target := getPayloadString(payload, "target")
		if bead != "" && target != "" {
			return fmt.Sprintf("slung %s to %s", bead, target)
		}
		return "work slung"

	case "hook":
		bead := getPayloadString(payload, "bead")
		if bead != "" {
			return fmt.Sprintf("hooked %s", bead)
		}
		return "bead hooked"

	case "handoff":
		subject := getPayloadString(payload, "subject")
		if subject != "" {
			return fmt.Sprintf("handoff: %s", subject)
		}
		return "session handoff"

	case "done":
		bead := getPayloadString(payload, "bead")
		if bead != "" {
			return fmt.Sprintf("done: %s", bead)
		}
		return "work done"

	case "mail":
		subject := getPayloadString(payload, "subject")
		to := getPayloadString(payload, "to")
		if subject != "" {
			if to != "" {
				return fmt.Sprintf("→ %s: %s", to, subject)
			}
			return subject
		}
		return "mail sent"

	case "merged":
		worker := getPayloadString(payload, "worker")
		if worker != "" {
			return fmt.Sprintf("merged work from %s", worker)
		}
		return "merged"

	case "merge_failed":
		reason := getPayloadString(payload, "reason")
		if reason != "" {
			return fmt.Sprintf("merge failed: %s", reason)
		}
		return "merge failed"

	default:
		if msg := getPayloadString(payload, "message"); msg != "" {
			return msg
		}
		return eventType
	}
}

// getPayloadString extracts a string from payload
func getPayloadString(payload map[string]interface{}, key string) string {
	if payload == nil {
		return ""
	}
	if v, ok := payload[key].(string); ok {
		return v
	}
	return ""
}

// getPayloadInt extracts an int from payload
func getPayloadInt(payload map[string]interface{}, key string) int {
	if payload == nil {
		return 0
	}
	if v, ok := payload[key].(float64); ok {
		return int(v)
	}
	return 0
}

// CombinedSource merges events from multiple sources
type CombinedSource struct {
	sources []EventSource
	events  chan Event
	cancel  context.CancelFunc
}

// fanInTimeout is the maximum time a fan-in goroutine will wait for an event
// before checking if it should exit. This prevents goroutine leaks when a source
// channel blocks forever and the context is never canceled.
const fanInTimeout = 30 * time.Second

// NewCombinedSource creates a source that merges multiple event sources
func NewCombinedSource(sources ...EventSource) *CombinedSource {
	ctx, cancel := context.WithCancel(context.Background())

	combined := &CombinedSource{
		sources: sources,
		events:  make(chan Event, 100),
		cancel:  cancel,
	}

	// Fan-in from all sources with timeout to prevent goroutine leaks.
	// Each goroutine will exit if:
	// 1. Context is canceled (Close() called)
	// 2. Source channel is closed
	// 3. No event received for fanInTimeout (prevents indefinite blocking)
	for _, src := range sources {
		go func(s EventSource) {
			for {
				select {
				case <-ctx.Done():
					return
				case event, ok := <-s.Events():
					if !ok {
						return
					}
					select {
					case combined.events <- event:
					case <-ctx.Done():
						return
					default:
						// Drop if full
					}
				case <-time.After(fanInTimeout):
					// Timeout - check if we should exit
					select {
					case <-ctx.Done():
						return
					default:
						// Context still active, continue waiting
					}
				}
			}
		}(src)
	}

	return combined
}

// Events returns the combined event channel
func (c *CombinedSource) Events() <-chan Event {
	return c.events
}

// Close stops all sources
func (c *CombinedSource) Close() error {
	c.cancel()
	var lastErr error
	for _, src := range c.sources {
		if err := src.Close(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// FindBeadsDir finds the beads directory for the given working directory
func FindBeadsDir(workDir string) (string, error) {
	// Walk up looking for .beads
	dir := workDir
	for {
		beadsPath := filepath.Join(dir, ".beads")
		if info, err := os.Stat(beadsPath); err == nil && info.IsDir() {
			return beadsPath, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", os.ErrNotExist
}
