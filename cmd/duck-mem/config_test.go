package main

import (
	"path/filepath"
	"testing"
)

func TestDefaultDBFollowsRecordedLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	want := filepath.Join(home, ".local", "share", "duck-mem", "memory.duckdb")
	if got := defaultDB(); got != want {
		t.Fatalf("defaultDB() = %s; want %s", got, want)
	}
	recorded := filepath.Join(home, "elsewhere", "memory.duckdb")
	if err := writeConfiguredDB(recorded); err != nil {
		t.Fatal(err)
	}
	if got := defaultDB(); got != recorded {
		t.Fatalf("defaultDB() = %s; want the recorded %s", got, recorded)
	}
}
