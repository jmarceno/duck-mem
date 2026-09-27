package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestUninstallPromptKeepsDataUnlessExplicitlyConfirmed(t *testing.T) {
	for _, tc := range []struct {
		answer string
		purge  bool
		fails  bool
	}{
		{"\n", false, false},
		{"no\n", false, false},
		{"yes\n", true, false},
		{"maybe\n", false, true},
	} {
		var prompt bytes.Buffer
		purge, err := askPurge(strings.NewReader(tc.answer), &prompt)
		if purge != tc.purge || (err != nil) != tc.fails || !strings.Contains(prompt.String(), "database") {
			t.Fatalf("answer %q: purge=%v err=%v prompt=%q", tc.answer, purge, err, prompt.String())
		}
	}
}

func TestCompletedSyncIsReadableByTray(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want := syncStatus{CompletedAt: time.Date(2026, 9, 27, 17, 45, 0, 0, time.UTC), Files: 12, Skipped: 1}
	if err := writeSyncStatus(defaultDB(), want); err != nil {
		t.Fatal(err)
	}
	got, err := readSyncStatus()
	if err != nil || got != want {
		t.Fatalf("last sync = %+v, err=%v; want %+v", got, err, want)
	}
}
