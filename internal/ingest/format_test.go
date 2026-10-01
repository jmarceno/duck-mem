package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestCursorTurnEndedRecordsPreserveFullAndAppendedConversation(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".cursor", "projects", "p", "agent-transcripts", "thread.jsonl")
	initial := strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"start"}]}}`,
		`{"type":"turn_ended","status":"completed"}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"answer"},{"type":"tool_use","name":"Read"}]}}`,
		`{"type":"turn_ended","status":"error","error":"interrupted"}`,
	}, "\n") + "\n"
	writeTemp(t, path, initial)
	sess, msgs, err := IngestFile(path)
	if err != nil || len(msgs) != 3 || msgs[0].Text != "start" || msgs[1].Text != "answer" ||
		msgs[2].Role != "note" || msgs[2].Text != "turn error: interrupted" {
		t.Fatalf("full parse: messages=%+v error=%v", msgs, err)
	}
	writeTemp(t, path, initial+`{"type":"turn_ended","status":"completed"}`+"\n"+
		`{"role":"user","message":{"content":[{"type":"text","text":"next"}]}}`+"\n")
	_, added, err := IngestAppended(path, int64(len(initial)), sess, &msgs[2], nil)
	if err != nil || len(added) != 1 || added[0].Text != "next" || added[0].Seq != 3 {
		t.Fatalf("append parse: messages=%+v error=%v", added, err)
	}
	writeTemp(t, path, initial+`{"type":"unknown_envelope"}`+"\n")
	if _, _, err := IngestFile(path); !errors.Is(err, ErrUnexpectedFormat) {
		t.Fatalf("unknown envelope should still fail: %v", err)
	}
}
