package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestRepackPreservesValuesSequencesAndSource(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.duckdb")
	out := filepath.Join(dir, "repacked.duckdb")
	db, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	insertMsg(t, db, "s", 0, "zephyrturbine storage")
	if _, err = db.sql.Exec(`CREATE SEQUENCE repack_seq; SELECT nextval('repack_seq'); SELECT nextval('repack_seq'); INSERT INTO file_checkpoints VALUES ('f',10,20,'hash',true,'s','p',1)`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = Repack(source, out); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(original) != sha256.Sum256(after) {
		t.Fatal("repack changed source")
	}
	if err = Repack(source, out); err == nil {
		t.Fatal("overwrote existing destination")
	}
	old, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	copy, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	var oldNext, copyNext int64
	if err = old.sql.QueryRow(`SELECT nextval('repack_seq')`).Scan(&oldNext); err != nil {
		t.Fatal(err)
	}
	if err = copy.sql.QueryRow(`SELECT nextval('repack_seq')`).Scan(&copyNext); err != nil {
		t.Fatal(err)
	}
	if oldNext != copyNext {
		t.Fatalf("sequence position: source=%d copy=%d", oldNext, copyNext)
	}
	if cp, err := copy.GetCheckpoint("f"); err != nil || cp == nil || cp.TailHash != "hash" {
		t.Fatalf("checkpoint: %v %v", cp, err)
	}
	if hits, err := copy.Search(context.Background(), "zephyrturbine storage", "", "", 5); err != nil || len(hits) == 0 {
		t.Fatalf("repacked search: %v %v", hits, err)
	}
}

func TestRepackSwapRebuildsInPlace(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "memory.duckdb")
	db, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	insertMsg(t, db, "s", 0, "zephyrturbine storage")
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	// Leftovers from an interrupted repack are replaced, not kept.
	for _, stale := range []string{source + ".repack-tmp", source + ".repack-tmp.wal"} {
		if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	res, err := RepackSwap(source)
	if err != nil {
		t.Fatal(err)
	}
	if res.Before != before.Size() || res.Out != "" {
		t.Fatalf("result = %+v; source was %d bytes", res, before.Size())
	}
	after, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if res.After == 0 || res.After != after.Size() {
		t.Fatalf("result = %+v; file is %d bytes", res, after.Size())
	}
	for _, leftover := range []string{source + ".repack-tmp", source + ".repack-tmp.wal", source + ".wal"} {
		if _, err := os.Stat(leftover); err == nil {
			t.Fatalf("%s was left behind", leftover)
		}
	}
	rebuilt, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	if hits, err := rebuilt.Search(context.Background(), "zephyrturbine storage", "", "", 5); err != nil || len(hits) == 0 {
		t.Fatalf("rebuilt search: %v %v", hits, err)
	}
}

func TestRepackSwapLeavesSourceOnFailure(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "memory.duckdb")
	garbage := []byte("not a duckdb file")
	if err := os.WriteFile(source, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RepackSwap(source); err == nil {
		t.Fatal("repacked a corrupt source")
	}
	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, garbage) {
		t.Fatal("the source changed after a failed repack")
	}
	for _, leftover := range []string{source + ".repack-tmp", source + ".repack-tmp.wal"} {
		if _, err := os.Stat(leftover); err == nil {
			t.Fatalf("%s was left behind", leftover)
		}
	}
}
