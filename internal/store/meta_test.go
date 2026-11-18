package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestReingestKeepsSourceDates checks the re-ingest rule: metadata may
// refresh, but the created and updated dates come from the source and
// move only when the stored message snapshot changes. A project rename
// is duck-mem's own change and must not move either date.
func TestReingestKeepsSourceDates(t *testing.T) {
	db := openTemp(t)
	created := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	updated := created.Add(2 * time.Hour)
	sess := Session{ID: "s1", Source: "claude", Project: "p", Path: "f",
		StartedAt: created, UpdatedAt: updated, Model: "claude-opus-5-5", Effort: "low"}
	msgs := []Message{{SessionID: "s1", Seq: 0, Source: "claude", Project: "p", Role: "user", Text: "alpha"}}
	if err := db.SyncConversation(sess, msgs, false, nil); err != nil {
		t.Fatal(err)
	}

	// Re-ingest of identical content: metadata refreshes, dates stay put.
	re := sess
	re.Model, re.UpdatedAt = "claude-haiku-4", updated.Add(time.Hour)
	if err := db.SyncConversation(re, msgs, false, nil); err != nil {
		t.Fatal(err)
	}
	var got struct {
		model, effort       string
		started, lastUpdate sql.NullTime
	}
	if err := db.sql.QueryRow(`SELECT model, effort, started_at, updated_at FROM sessions WHERE session_id='s1'`).
		Scan(&got.model, &got.effort, &got.started, &got.lastUpdate); err != nil {
		t.Fatal(err)
	}
	if got.model != "claude-haiku-4" || got.effort != "low" {
		t.Fatalf("model/effort not refreshed: %q/%q", got.model, got.effort)
	}
	if !got.started.Time.Equal(created) || !got.lastUpdate.Time.Equal(updated) {
		t.Fatalf("dates moved on identical re-ingest: %v → %v", got.started.Time, got.lastUpdate.Time)
	}

	// New content advances the updated date to the source value.
	later := updated.Add(3 * time.Hour)
	re.UpdatedAt = later
	grown := append(msgs, Message{SessionID: "s1", Seq: 1, Source: "claude", Project: "p", Role: "assistant", Text: "beta"})
	if err := db.SyncConversation(re, grown, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`SELECT updated_at FROM sessions WHERE session_id='s1'`).Scan(&got.lastUpdate); err != nil {
		t.Fatal(err)
	}
	if !got.lastUpdate.Time.Equal(later) {
		t.Fatalf("updated_at %v want %v after new content", got.lastUpdate.Time, later)
	}

	// A rename is not new data.
	if _, err := db.RenameProject("p", "q"); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`SELECT started_at, updated_at FROM sessions WHERE session_id='s1'`).
		Scan(&got.started, &got.lastUpdate); err != nil {
		t.Fatal(err)
	}
	if !got.started.Time.Equal(created) || !got.lastUpdate.Time.Equal(later) {
		t.Fatalf("rename moved dates: %v → %v", got.started.Time, got.lastUpdate.Time)
	}
}

// TestLegacyRowsBackfillAtOpen checks that a database written before the
// model, effort and date columns existed is filled at open: rows with
// message timestamps take the date from them, and rows without any take
// the OS file times during the one-time pass.
func TestLegacyRowsBackfillAtOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.duckdb")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-existing schema shape: no model or effort values,
	// no updated date, and one message timestamp to source the date from.
	for _, q := range []string{
		`INSERT INTO sessions(session_id, source, project, started_at, path) VALUES('old', 'claude', 'p', NULL, 'c.jsonl')`,
		`INSERT INTO sessions(session_id, source, project, started_at, path) VALUES('old2', 'cursor', 'p', NULL, 't.jsonl')`,
		`INSERT INTO messages(session_id, seq, source, project, role, text, created_at)
			VALUES('old', 0, 'claude', 'p', 'user', 'hello', '2026-01-02 03:04:05+00')`,
		`UPDATE sessions SET model='', effort=''`,
		`UPDATE sessions SET updated_at=NULL`,
	} {
		if _, err := db.sql.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var model, effort string
	var started, updated sql.NullTime
	if err := db.sql.QueryRow(`SELECT model, effort, started_at, updated_at FROM sessions WHERE session_id='old'`).
		Scan(&model, &effort, &started, &updated); err != nil {
		t.Fatal(err)
	}
	if model != UnknownModel || effort != UnknownEffort {
		t.Fatalf("legacy row model/effort: %q/%q", model, effort)
	}
	if want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC); !updated.Time.Equal(want) {
		t.Fatalf("legacy row updated_at %v want %v from its message", updated.Time, want)
	}

	// The one-time pass fills the rows the message timestamps could not,
	// including the created date of a row that never had one.
	stub := func(p string) (time.Time, time.Time) {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	}
	if err := db.BackfillSessionMeta(stub); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`SELECT started_at, updated_at FROM sessions WHERE session_id='old2'`).
		Scan(&started, &updated); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC); !started.Time.Equal(want) {
		t.Fatalf("file-backed row started_at %v want %v", started.Time, want)
	}
	if want := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC); !updated.Time.Equal(want) {
		t.Fatalf("file-backed row updated_at %v want %v", updated.Time, want)
	}
}
