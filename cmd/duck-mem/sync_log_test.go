package main

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRotateSyncLogsKeepsCurrentMonthAndPrunesOldArchives(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(logDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	month := func(offset int) string { return now.AddDate(0, offset, 0).Format("2006-01") }
	write := func(name, body string) string {
		p := filepath.Join(logDir(), name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	current := write(month(0)+".jsonl", "current\n")
	previous := write(month(-1)+".jsonl", "previous\n")
	write(month(-2)+".jsonl.gz", "recent archive\n")
	stale := write(month(-logRetentionMonths-1)+".jsonl.gz", "stale archive\n")
	if err := rotateSyncLogs(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(current); err != nil {
		t.Fatalf("current log removed: %v", err)
	}
	if _, err := os.Stat(previous); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("previous month left uncompressed")
	}
	f, err := os.Open(filepath.Join(logDir(), month(-1)+".jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "previous\n" {
		t.Fatalf("rotated content: %q", body)
	}
	if _, err := os.Stat(filepath.Join(logDir(), month(-2)+".jsonl.gz")); err != nil {
		t.Fatal("archive inside retention removed")
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("archive older than retention kept")
	}
}

func TestCleanCycleClearsFailureOnlyWhenNothingWasSkipped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := appendSyncLog(syncLogEntry{Harness: "all", Result: "failure", Reason: "Database error", Detail: "lock"}); err != nil {
		t.Fatal(err)
	}
	if err := acknowledgeCleanCycle(1); err != nil {
		t.Fatal(err)
	}
	if !unreadSyncFailure() {
		t.Fatal("a cycle that skipped a file cleared the failure")
	}
	if err := acknowledgeCleanCycle(0); err != nil {
		t.Fatal(err)
	}
	if unreadSyncFailure() {
		t.Fatal("a complete cycle left the failure set")
	}
}

func TestSyncHistoryAndFailureAcknowledgement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	failure := syncLogEntry{Harness: "cursor", Result: "failure", Reason: "Unexpected format", Detail: "unrecognized record envelope", Path: "/cursor/thread.jsonl"}
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
	if err := appendSyncLog(syncLogEntry{Harness: "cursor", Sessions: 1, Lines: 3, Result: "success"}); err != nil {
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
	if len(lines) != 4 {
		t.Fatalf("history has %d entries", len(lines))
	}
	var entry syncLogEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Path != failure.Path {
		t.Fatalf("failure file path lost: %+v", entry)
	}
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Harness != "claude" || entry.Sessions != 2 || entry.Lines != 7 || entry.Time.IsZero() {
		t.Fatalf("success entry: %+v", entry)
	}
}
