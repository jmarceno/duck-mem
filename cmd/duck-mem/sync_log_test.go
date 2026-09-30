package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncHistoryAndFailureAcknowledgement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	failure := syncLogEntry{Harness: "codex", Result: "failure", Reason: "Unexpected format", Detail: "unrecognized record envelope"}
	if err := appendSyncLog(failure); err != nil {
		t.Fatal(err)
	}
	if !unreadSyncFailure() {
		t.Fatal("failure did not set unread state")
	}
	marker, err := os.ReadFile(failurePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := appendSyncLog(syncLogEntry{Harness: "claude", Sessions: 2, Lines: 7, Result: "success"}); err != nil {
		t.Fatal(err)
	}
	if !unreadSyncFailure() {
		t.Fatal("success cleared failure")
	}
	if err := acknowledgeSyncFailure(marker); err != nil {
		t.Fatal(err)
	}
	if unreadSyncFailure() {
		t.Fatal("acknowledgement did not clear failure")
	}
	if err := appendSyncLog(failure); err != nil {
		t.Fatal(err)
	}
	// Acknowledging an older snapshot must not hide a newer failure.
	if err := acknowledgeSyncFailure(marker); err != nil {
		t.Fatal(err)
	}
	if !unreadSyncFailure() {
		t.Fatal("new failure was hidden")
	}
	files, _ := filepath.Glob(filepath.Join(logDir(), "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("log files = %v", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("history has %d entries", len(lines))
	}
	var entry syncLogEntry
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Harness != "claude" || entry.Sessions != 2 || entry.Lines != 7 || entry.Time.IsZero() {
		t.Fatalf("success entry: %+v", entry)
	}
}
