package feed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gtFeedTestEvent(kind string) []byte {
	payload := map[string]any{
		"ts":         "2026-09-12T00:00:00Z",
		"source":     "test",
		"type":       kind,
		"actor":      "gastown/crew/test",
		"payload":    map[string]any{"marker": kind},
		"visibility": "feed",
	}
	raw, _ := json.Marshal(payload)
	return raw
}

func waitGtFeedEvent(t *testing.T, events <-chan Event, wantType string) Event {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("event source closed while waiting for %q", wantType)
			}
			if event.Type == "diagnostic" {
				continue
			}
			if event.Type != wantType {
				t.Fatalf("event type = %q, want %q (raw=%q)", event.Type, wantType, event.Raw)
			}
			return event
		case <-deadline.C:
			t.Fatalf("timed out waiting for event %q", wantType)
			return Event{}
		}
	}
}

func waitGtFeedDiagnostic(t *testing.T, source *GtEventsSource) error {
	t.Helper()
	select {
	case err, ok := <-source.Diagnostics():
		if !ok {
			t.Fatal("event diagnostics channel closed before diagnostic")
		}
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event diagnostic")
		return nil
	}
}

func TestGtEventsSourceAppendAfterInitialEOFWithoutRestart(t *testing.T) {
	townRoot := t.TempDir()
	path := filepath.Join(townRoot, ".events.jsonl")
	if err := os.WriteFile(path, append(gtFeedTestEvent("initial"), '\n'), 0644); err != nil {
		t.Fatalf("write initial events: %v", err)
	}
	source, err := NewGtEventsSource(townRoot)
	if err != nil {
		t.Fatalf("NewGtEventsSource: %v", err)
	}
	defer source.Close()
	waitGtFeedEvent(t, source.Events(), "initial")

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := file.Write(append(gtFeedTestEvent("appended"), '\n')); err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close append: %v", err)
	}
	waitGtFeedEvent(t, source.Events(), "appended")
}

func TestGtEventsSourcePartialLineThenNewlinePreservesOrder(t *testing.T) {
	townRoot := t.TempDir()
	path := filepath.Join(townRoot, ".events.jsonl")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatalf("write empty events: %v", err)
	}
	source, err := NewGtEventsSource(townRoot)
	if err != nil {
		t.Fatalf("NewGtEventsSource: %v", err)
	}
	defer source.Close()

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	partial := gtFeedTestEvent("partial")
	if _, err := file.Write(partial); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close partial: %v", err)
	}
	select {
	case event := <-source.Events():
		t.Fatalf("partial record emitted before newline: %#v", event)
	case <-time.After(250 * time.Millisecond):
	}

	file, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("reopen append: %v", err)
	}
	if _, err := file.Write(append([]byte{'\n'}, append(gtFeedTestEvent("delayed"), '\n')...)); err != nil {
		t.Fatalf("complete records: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close delayed: %v", err)
	}
	waitGtFeedEvent(t, source.Events(), "partial")
	waitGtFeedEvent(t, source.Events(), "delayed")
}

func TestGtEventsSourceRenameCreateRotationEmitsUnseenRowsOnce(t *testing.T) {
	townRoot := t.TempDir()
	path := filepath.Join(townRoot, ".events.jsonl")
	if err := os.WriteFile(path, append(gtFeedTestEvent("before"), '\n'), 0644); err != nil {
		t.Fatalf("write initial events: %v", err)
	}
	source, err := NewGtEventsSource(townRoot)
	if err != nil {
		t.Fatalf("NewGtEventsSource: %v", err)
	}
	defer source.Close()
	waitGtFeedEvent(t, source.Events(), "before")

	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rotate old file: %v", err)
	}
	if err := os.WriteFile(path, append(append(gtFeedTestEvent("before"), '\n'), append(gtFeedTestEvent("rotated"), '\n')...), 0644); err != nil {
		t.Fatalf("create rotated file: %v", err)
	}
	waitGtFeedEvent(t, source.Events(), "rotated")
	select {
	case event := <-source.Events():
		if event.Type == "before" {
			t.Fatalf("rotation replayed already-emitted event: %#v", event)
		}
	case <-time.After(400 * time.Millisecond):
	}
}

// TestGtEventsSourceRotationDrainsRenamedOldInode proves that a complete row
// appended to the descriptor being tailed is delivered even when operators
// rotate that inode before the next poll. The old descriptor must be drained
// before recovery closes it.
func TestGtEventsSourceRotationDrainsRenamedOldInode(t *testing.T) {
	townRoot := t.TempDir()
	path := filepath.Join(townRoot, ".events.jsonl")
	if err := os.WriteFile(path, append(gtFeedTestEvent("before-old-drain"), '\n'), 0644); err != nil {
		t.Fatalf("write initial events: %v", err)
	}
	source, err := NewGtEventsSource(townRoot)
	if err != nil {
		t.Fatalf("NewGtEventsSource: %v", err)
	}
	defer source.Close()
	waitGtFeedEvent(t, source.Events(), "before-old-drain")

	oldFile, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open old inode: %v", err)
	}
	if _, err := oldFile.Write(append(gtFeedTestEvent("unseen-old-inode"), '\n')); err != nil {
		_ = oldFile.Close()
		t.Fatalf("append old inode: %v", err)
	}
	if err := oldFile.Close(); err != nil {
		t.Fatalf("close old inode: %v", err)
	}
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rotate old inode: %v", err)
	}
	if err := os.WriteFile(path, append(gtFeedTestEvent("replacement-after-old"), '\n'), 0644); err != nil {
		t.Fatalf("create replacement: %v", err)
	}

	waitGtFeedEvent(t, source.Events(), "unseen-old-inode")
	waitGtFeedEvent(t, source.Events(), "replacement-after-old")
}

// TestGtEventsSourceLargeReplacementDoesNotSkipCompleteRows proves recovery
// starts early enough to see complete records in a replacement larger than
// the one-megabyte preload window, rather than silently jumping over them.
func TestGtEventsSourceLargeReplacementDoesNotSkipCompleteRows(t *testing.T) {
	townRoot := t.TempDir()
	path := filepath.Join(townRoot, ".events.jsonl")
	if err := os.WriteFile(path, append(gtFeedTestEvent("before-large-replacement"), '\n'), 0644); err != nil {
		t.Fatalf("write initial events: %v", err)
	}
	source, err := NewGtEventsSource(townRoot)
	if err != nil {
		t.Fatalf("NewGtEventsSource: %v", err)
	}
	defer source.Close()
	waitGtFeedEvent(t, source.Events(), "before-large-replacement")

	contents := append([]byte{}, append(gtFeedTestEvent("unseen-before-preload-window"), '\n')...)
	// Blank complete records make the replacement exceed the preload window
	// without filling the bounded pending-event queue and obscuring the row
	// under test.
	contents = append(contents, bytes.Repeat([]byte{'\n'}, gtEventPreloadBytes+gtEventPollBytes)...)
	contents = append(contents, append(gtFeedTestEvent("replacement-tail"), '\n')...)
	if len(contents) <= gtEventPreloadBytes {
		t.Fatalf("replacement fixture size = %d, want > %d", len(contents), gtEventPreloadBytes)
	}
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rotate old file: %v", err)
	}
	if err := os.WriteFile(path, contents, 0644); err != nil {
		t.Fatalf("create large replacement: %v", err)
	}

	seenUnseen := false
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !seenUnseen {
		select {
		case event, ok := <-source.Events():
			if !ok {
				t.Fatal("event source closed before large replacement rows")
			}
			if event.Type == "diagnostic" {
				continue
			}
			switch event.Type {
			case "unseen-before-preload-window":
				seenUnseen = true
			case "replacement-tail":
				t.Fatal("replacement tail arrived after unseen prefix was skipped")
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for complete row before preload window")
		}
	}
}

func TestGtEventsSourceTruncateAndCopyTruncateRegrowRecover(t *testing.T) {
	townRoot := t.TempDir()
	path := filepath.Join(townRoot, ".events.jsonl")
	if err := os.WriteFile(path, append(gtFeedTestEvent("before"), '\n'), 0644); err != nil {
		t.Fatalf("write initial events: %v", err)
	}
	source, err := NewGtEventsSource(townRoot)
	if err != nil {
		t.Fatalf("NewGtEventsSource: %v", err)
	}
	defer source.Close()
	waitGtFeedEvent(t, source.Events(), "before")

	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644); err != nil {
		t.Fatalf("open after truncate: %v", err)
	} else {
		if _, err := file.Write(append(gtFeedTestEvent("after-truncate"), '\n')); err != nil {
			t.Fatalf("write after truncate: %v", err)
		}
		_ = file.Close()
	}
	waitGtFeedEvent(t, source.Events(), "after-truncate")

	// Copy-truncate on the same inode can regrow beyond the previous offset;
	// the trailing boundary digest must still force recovery.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("copy-truncate: %v", err)
	}
	if file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644); err != nil {
		t.Fatalf("reopen regrow: %v", err)
	} else {
		if _, err := file.Write(append(gtFeedTestEvent("after-regrow"), '\n')); err != nil {
			t.Fatalf("write after regrow: %v", err)
		}
		_ = file.Close()
	}
	waitGtFeedEvent(t, source.Events(), "after-regrow")
}

func TestGtEventsSourceMalformedAndOversizeRecordsDiagnoseAndContinue(t *testing.T) {
	townRoot := t.TempDir()
	path := filepath.Join(townRoot, ".events.jsonl")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatalf("write empty events: %v", err)
	}
	source, err := NewGtEventsSource(townRoot)
	if err != nil {
		t.Fatalf("NewGtEventsSource: %v", err)
	}
	defer source.Close()

	badOversize := []byte(strings.Repeat("x", 128*1024+1))
	badOversize = append(badOversize, '\n')
	contents := append([]byte("{malformed}\n"), badOversize...)
	contents = append(contents, append(gtFeedTestEvent("valid-successor"), '\n')...)
	if file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644); err != nil {
		t.Fatalf("open diagnostics append: %v", err)
	} else {
		if _, err := file.Write(contents); err != nil {
			t.Fatalf("write diagnostics: %v", err)
		}
		_ = file.Close()
	}
	waitGtFeedDiagnostic(t, source)
	waitGtFeedDiagnostic(t, source)
	waitGtFeedEvent(t, source.Events(), "valid-successor")
}

func TestGtEventsSourceRefusesSymlinkAndCloseWaitsForTail(t *testing.T) {
	townRoot := t.TempDir()
	target := filepath.Join(townRoot, "real-events.jsonl")
	if err := os.WriteFile(target, nil, 0644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	path := filepath.Join(townRoot, ".events.jsonl")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink events: %v", err)
	}
	if _, err := NewGtEventsSource(townRoot); err == nil {
		t.Fatal("symlinked event path was followed")
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatalf("write regular events: %v", err)
	}
	source, err := NewGtEventsSource(townRoot)
	if err != nil {
		t.Fatalf("NewGtEventsSource regular: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case _, ok := <-source.Events():
		if ok {
			t.Fatal("event channel remained open after Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close returned before tail goroutine stopped")
	}
}

func TestGtEventsSourceInitialReadAndRecoveryBounds(t *testing.T) {
	townRoot := t.TempDir()
	path := filepath.Join(townRoot, ".events.jsonl")
	var contents []byte
	for i := 0; i < 300; i++ {
		contents = append(contents, append(gtFeedTestEvent(fmt.Sprintf("bulk-%03d-%s", i, strings.Repeat("x", 3500))), '\n')...)
	}
	if err := os.WriteFile(path, contents, 0644); err != nil {
		t.Fatalf("write bulk events: %v", err)
	}
	source, err := NewGtEventsSource(townRoot)
	if err != nil {
		t.Fatalf("NewGtEventsSource: %v", err)
	}
	defer source.Close()
	if source.initialReadBytes > gtEventPreloadBytes {
		t.Fatalf("initial preload read %d bytes, bound is %d", source.initialReadBytes, gtEventPreloadBytes)
	}
	if len(source.recentDigests) > gtEventDigestCap {
		t.Fatalf("recovery digest set grew to %d, bound is %d", len(source.recentDigests), gtEventDigestCap)
	}
	if len(source.partial) > gtEventRecordBytes {
		t.Fatalf("partial record buffer grew to %d, bound is %d", len(source.partial), gtEventRecordBytes)
	}
}

func TestGtEventsSourceDiagnosticBurstWithoutReaderDoesNotStall(t *testing.T) {
	townRoot := t.TempDir()
	path := filepath.Join(townRoot, ".events.jsonl")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatalf("write empty events: %v", err)
	}
	source, err := NewGtEventsSource(townRoot)
	if err != nil {
		t.Fatalf("NewGtEventsSource: %v", err)
	}
	defer source.Close()

	var contents []byte
	for i := 0; i < gtEventDiagnosticCap+1; i++ {
		contents = append(contents, []byte(fmt.Sprintf("{malformed-%03d}\n", i))...)
	}
	contents = append(contents, append(gtFeedTestEvent("valid-after-diagnostics"), '\n')...)
	if err := os.WriteFile(path, contents, 0644); err != nil {
		t.Fatalf("write hostile diagnostics: %v", err)
	}

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	sawDiagnostic := false
	for {
		select {
		case event, ok := <-source.Events():
			if !ok {
				t.Fatal("event source closed before valid successor")
			}
			if event.Type == "diagnostic" {
				sawDiagnostic = true
				if !strings.Contains(event.Message, "diagnostic") || !strings.Contains(event.Message, "dropped") {
					t.Fatalf("production diagnostic event lacks bounded drop accounting: %#v", event)
				}
				continue
			}
			if event.Type != "valid-after-diagnostics" {
				t.Fatalf("unexpected event before valid successor: %#v", event)
			}
			break
		case <-deadline.C:
			t.Fatal("valid successor was blocked by an undrained diagnostics channel")
		}
		break
	}
	if !sawDiagnostic {
		t.Fatal("production event stream did not expose a bounded diagnostic summary")
	}
	closed := make(chan error, 1)
	go func() { closed <- source.Close() }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked after hostile diagnostics burst")
	}
}

func TestGtEventsSourceAuditVisibilityIsFilteredWithoutDiagnostics(t *testing.T) {
	townRoot := t.TempDir()
	path := filepath.Join(townRoot, ".events.jsonl")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatalf("write empty events: %v", err)
	}
	source, err := NewGtEventsSource(townRoot)
	if err != nil {
		t.Fatalf("NewGtEventsSource: %v", err)
	}
	defer source.Close()

	var contents []byte
	for i := 0; i < gtEventDiagnosticCap+1; i++ {
		payload := map[string]any{
			"ts":         "2026-09-12T00:00:00Z",
			"source":     "test",
			"type":       fmt.Sprintf("audit-%03d", i),
			"actor":      "gastown/crew/test",
			"payload":    map[string]any{"marker": i},
			"visibility": "audit",
		}
		raw, _ := json.Marshal(payload)
		contents = append(contents, append(raw, '\n')...)
	}
	contents = append(contents, append(gtFeedTestEvent("after-audit"), '\n')...)
	if err := os.WriteFile(path, contents, 0644); err != nil {
		t.Fatalf("write audit fixture: %v", err)
	}

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event, ok := <-source.Events():
			if !ok {
				t.Fatal("event source closed before valid successor")
			}
			if event.Type == "diagnostic" {
				t.Fatalf("valid audit records produced a production diagnostic: %#v", event)
			}
			if event.Type == "after-audit" {
				select {
				case diagnostic := <-source.Diagnostics():
					t.Fatalf("valid audit records consumed diagnostic capacity: %v", diagnostic)
				default:
				}
				return
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for valid successor after audit records")
		}
	}
}
