package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMalformedAndIncompleteSessionRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "sessions", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		data    string
		failure bool
	}{
		{"broken\n", true},
		{"broken", true},
		{"{\"new_envelope\":true}\n", true},
		{"{\"type\":", false},
		{"{\"type\":\"event_msg\",\"payload\":{\"type\":\"tool_output\"}}\n", false},
	} {
		if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := IngestFile(path)
		if errors.Is(err, ErrUnexpectedFormat) != tc.failure {
			t.Errorf("input %q: error=%v", tc.data, err)
		}
	}
}
