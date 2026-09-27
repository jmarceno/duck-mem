package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSearchFindsByKeywordFiltersAndDedupes(t *testing.T) {
	db := openTemp(t)
	sess := Session{ID: "s1", Source: "codex", Project: "/home/u/organizer", Path: "f", StartedAt: time.Now()}
	if err := db.UpsertSession(sess); err != nil {
		t.Fatal(err)
	}
	msgs := []Message{
		{SessionID: "s1", Seq: 0, Source: "codex", Project: "/home/u/organizer", Role: "user", Text: "fix the redship bug"},
		{SessionID: "s1", Seq: 1, Source: "codex", Project: "/home/u/organizer", Role: "assistant", Text: "redship fixed and deployed"},
	}
	if err := db.InsertMessages(msgs); err != nil {
		t.Fatal(err)
	}

	hits, err := db.Search("redship", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits want 2", len(hits))
	}

	hits, err = db.Search("redship fixed", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Seq != 1 {
		t.Fatalf("AND-match failed: %+v", hits)
	}

	hits, err = db.Search("redship", "omen", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("project filter failed: %+v", hits)
	}

	hits, err = db.Search("redship", "ORGANIZER", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("project substring match failed: %+v", hits)
	}

	// Re-ingest must not duplicate.
	if err := db.InsertMessages(msgs); err != nil {
		t.Fatal(err)
	}
	_, n, err := db.Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("duplicate messages stored: n=%d", n)
	}
}
