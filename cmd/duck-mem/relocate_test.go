package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmarceno/duck-mem/internal/owner"
	"github.com/jmarceno/duck-mem/internal/store"
)

func relocateTestEnv(t *testing.T) (source, target string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	return filepath.Join(home, "old", "memory.duckdb"), filepath.Join(home, "new", "memory.duckdb")
}

// holdOwnerLock keeps the database locked the way the running daemon does,
// so the move has to stop the indexer to get in. The returned function
// releases it.
func holdOwnerLock(t *testing.T, path string) func() {
	t.Helper()
	unlock, err := owner.TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	return unlock
}

// fakeIndexer never touches systemd; stop releases the lock the way a
// stopped daemon would, and started records the restart the move owes.
func fakeIndexer(release func()) (indexerControl, *bool) {
	started := new(bool)
	return indexerControl{
		stop:  func() (bool, error) { release(); return true, nil },
		start: func() error { *started = true; return nil },
	}, started
}

func seedRelocateDatabase(t *testing.T, path string) {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := store.Session{ID: "s1", Source: "codex", Project: "proj", Path: "/logs/s1.jsonl"}
	if err := db.UpsertSession(s); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertMessages([]store.Message{{
		SessionID: s.ID, Seq: 0, Source: s.Source, Project: s.Project,
		Role: "user", Text: "remember the relocate smoke run", CreatedAt: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.IndexNew(2); err != nil {
		t.Fatal(err)
	}
	if err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}
}

func TestRelocateMovesVerifiedCopyAndKeepsOldWhenRefused(t *testing.T) {
	source, target := relocateTestEnv(t)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	seedRelocateDatabase(t, source)
	release := holdOwnerLock(t, source)
	control, started := fakeIndexer(release)
	var out bytes.Buffer
	if err := runRelocate(source, target, control, strings.NewReader("n\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !*started {
		t.Fatal("the indexer was not restarted after the move")
	}
	db, err := store.OpenReadOnly(target)
	if err != nil {
		t.Fatal(err)
	}
	sessions, messages, err := db.Count()
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || messages != 1 {
		t.Fatalf("copy holds %d sessions/%d messages; want 1/1", sessions, messages)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("answering no must keep the old database: %v", err)
	}
	if got := defaultDB(); got != target {
		t.Fatalf("commands would use %s; want the new location %s", got, target)
	}
	files, _ := trashDirs()
	if entries, _ := os.ReadDir(files); len(entries) != 0 {
		t.Fatalf("answering no must not trash anything; trash holds %d entries", len(entries))
	}
}

func TestRelocateTrashesOldDatabaseWhenConfirmed(t *testing.T) {
	source, target := relocateTestEnv(t)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	seedRelocateDatabase(t, source)
	release := holdOwnerLock(t, source)
	control, _ := fakeIndexer(release)
	var out bytes.Buffer
	if err := runRelocate(source, target, control, strings.NewReader("yes\n"), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("old database still at %s: %v", source, err)
	}
	if leftovers, _ := filepath.Glob(source + ".*"); len(leftovers) != 0 {
		t.Fatalf("runtime files left beside the moved database: %v", leftovers)
	}
	files, info := trashDirs()
	entries, err := os.ReadDir(files)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash files: %d entries, err=%v; want the old database", len(entries), err)
	}
	infos, err := os.ReadDir(info)
	if err != nil || len(infos) != 1 {
		t.Fatalf("trash info: %d entries, err=%v; want one entry", len(infos), err)
	}
	data, err := os.ReadFile(filepath.Join(info, infos[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Path="+trashInfoPath(source)) {
		t.Fatalf("trashinfo does not restore to %s: %s", source, data)
	}
	restored := filepath.Join(t.TempDir(), "restored.duckdb")
	if err := os.Rename(filepath.Join(files, entries[0].Name()), restored); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenReadOnly(restored)
	if err != nil {
		t.Fatal(err)
	}
	sessions, _, err := db.Count()
	db.Close()
	if err != nil || sessions != 1 {
		t.Fatalf("trashed database holds %d sessions, err=%v; want a restorable 1", sessions, err)
	}
}

func TestRelocateRecordsFreshLocationWithoutCopy(t *testing.T) {
	source, target := relocateTestEnv(t)
	control, started := fakeIndexer(nil)
	var out bytes.Buffer
	if err := runRelocate(source, target, control, strings.NewReader("\n"), &out); err != nil {
		t.Fatal(err)
	}
	if *started {
		t.Fatal("a fresh location must not touch the indexer")
	}
	if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a fresh location must not create the file yet: %v", err)
	}
	if got := defaultDB(); got != target {
		t.Fatalf("commands would use %s; want %s", got, target)
	}
}

func TestRelocateRefusesAnExistingDestination(t *testing.T) {
	source, target := relocateTestEnv(t)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	seedRelocateDatabase(t, source)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	control, _ := fakeIndexer(nil)
	var out bytes.Buffer
	if err := runRelocate(source, target, control, strings.NewReader("yes\n"), &out); err == nil {
		t.Fatal("relocating onto an existing file must fail")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("the old database was disturbed: %v", err)
	}
	if configuredDB() != "" {
		t.Fatalf("a failed relocation recorded %s as the location", configuredDB())
	}
}
