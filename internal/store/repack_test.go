package store

import (
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
	if hits, err := copy.Search("zephyrturbine storage", "", "", 5); err != nil || len(hits) == 0 {
		t.Fatalf("repacked search: %v %v", hits, err)
	}
}
