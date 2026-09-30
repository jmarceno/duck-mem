package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jmarceno/duck-mem/internal/ingest"
)

type syncLogEntry struct {
	Time     time.Time `json:"time"`
	Harness  string    `json:"harness"`
	Sessions int       `json:"sessions"`
	Lines    int       `json:"lines"`
	Result   string    `json:"result"`
	Reason   string    `json:"reason,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

func logDir() string           { return filepath.Join(stateDir(), "logs") }
func failurePath() string      { return filepath.Join(stateDir(), "sync-failure") }
func acknowledgedPath() string { return filepath.Join(stateDir(), "sync-failure-seen") }

func appendSyncLog(entry syncLogEntry) error {
	entry.Time = time.Now().UTC()
	if err := os.MkdirAll(logDir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(logDir(), entry.Time.Format("2006-01")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(entry)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if entry.Result == "failure" {
		markerErr := os.WriteFile(failurePath(), []byte(entry.Time.Format(time.RFC3339Nano)), 0o600)
		err = errors.Join(err, markerErr)
	}
	return errors.Join(err, closeErr)
}

func logSyncFailure(harness, reason string, err error) {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	if logErr := appendSyncLog(syncLogEntry{Harness: harness, Result: "failure", Reason: reason, Detail: detail}); logErr != nil {
		fmt.Fprintln(os.Stderr, "sync log:", logErr)
	}
}

func unreadSyncFailure() bool {
	failed, err := os.ReadFile(failurePath())
	if err != nil {
		return false
	}
	seen, _ := os.ReadFile(acknowledgedPath())
	return string(failed) != string(seen)
}

func acknowledgeSyncFailure(marker []byte) error {
	if len(marker) == 0 {
		return nil
	}
	return os.WriteFile(acknowledgedPath(), marker, 0o600)
}

func harnessName(kind ingest.Kind) string {
	switch kind {
	case ingest.KindCodex:
		return "codex"
	case ingest.KindClaude:
		return "claude"
	case ingest.KindMuse:
		return "muse"
	case ingest.KindCursorTranscript, ingest.KindCursorPlan:
		return "cursor"
	case ingest.KindOpenCode:
		return "opencode"
	default:
		return "unknown"
	}
}

func extractionReason(err error) string {
	if errors.Is(err, ingest.ErrUnexpectedFormat) {
		return "Unexpected format"
	}
	return "Other"
}

type syncDatabaseError struct{ error }

func (e *syncDatabaseError) Unwrap() error { return e.error }
