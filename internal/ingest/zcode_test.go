package ingest

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestZCodeKeepsVisibleTextOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zcode", "db.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT, time_created INTEGER)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, data TEXT)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, time_created INTEGER, data TEXT)`,
		`INSERT INTO session VALUES ('sess_386f266d-4c09', NULL, '/project/', 1789980111419)`,
		// Subagent sessions are kept: their reports only reach the main
		// conversation as tool output, which is dropped.
		`INSERT INTO session VALUES ('sess_subagent_agent_5f0c', 'sess_386f266d-4c09', '/project/', 1789980111442)`,
		`INSERT INTO message VALUES ('msg_u1', 'sess_386f266d-4c09', 1789980111525, '{"id":"msg_u1","role":"user","time":{"created":1789980111525}}')`,
		`INSERT INTO message VALUES ('msg_a1', 'sess_386f266d-4c09', 1789980111526, '{"id":"msg_a1","role":"assistant","modelID":"GLM-5.3-Flash","time":{"created":1789980111526}}')`,
		`INSERT INTO message VALUES ('msg_a2', 'sess_386f266d-4c09', 1789980111527, '{"id":"msg_a2","role":"assistant","time":{"created":1789980111527}}')`,
		`INSERT INTO message VALUES ('msg_k1', 'sess_subagent_agent_5f0c', 1789980111550, '{"id":"msg_k1","role":"user","time":{"created":1789980111550}}')`,
		`INSERT INTO part VALUES ('p1', 'msg_u1', 'sess_386f266d-4c09', 1789980111525, '{"type":"text","text":"hello"}')`,
		`INSERT INTO part VALUES ('p2', 'msg_a1', 'sess_386f266d-4c09', 1789980111526, '{"type":"reasoning","text":"private thought"}')`,
		`INSERT INTO part VALUES ('p3', 'msg_a1', 'sess_386f266d-4c09', 1789980111526, '{"type":"tool","callID":"c1","tool":"Read","state":{"status":"completed","output":"secret output"}}')`,
		`INSERT INTO part VALUES ('p4', 'msg_a1', 'sess_386f266d-4c09', 1789980111526, '{"type":"text","text":"visible answer"}')`,
		`INSERT INTO part VALUES ('p5', 'msg_a1', 'sess_386f266d-4c09', 1789980111526, '{"type":"step-finish","reason":"stop"}')`,
		`INSERT INTO part VALUES ('p6', 'msg_a1', 'sess_386f266d-4c09', 1789980111527, '{"type":"text","text":"second paragraph"}')`,
		// An assistant message with no visible text parts yields no message.
		`INSERT INTO part VALUES ('p7', 'msg_a2', 'sess_386f266d-4c09', 1789980111527, '{"type":"tool","callID":"c2","tool":"Bash","state":{"status":"running"}}')`,
		`INSERT INTO part VALUES ('p8', 'msg_k1', 'sess_subagent_agent_5f0c', 1789980111550, '{"type":"text","text":"child prompt"}')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	sessions, err := IngestZCode(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions", len(sessions))
	}
	s, child := sessions[0], sessions[1]
	if s.Session.ID != "zcode:386f266d-4c09" || s.Session.Project != "/project" || s.Session.Source != "zcode" ||
		!s.Session.StartedAt.Equal(time.UnixMilli(1789980111419).UTC()) || len(s.Messages) != 2 {
		t.Fatalf("wrong parent session metadata or message count: %+v", s.Session)
	}
	if s.Messages[0].Role != "user" || s.Messages[0].Text != "hello" ||
		s.Messages[1].Role != "assistant" || s.Messages[1].Text != "visible answer\nsecond paragraph" ||
		s.Messages[1].Seq != 1 ||
		!s.Messages[1].CreatedAt.Equal(time.UnixMilli(1789980111526).UTC()) {
		t.Fatalf("wrong visible conversation: %+v", s.Messages)
	}
	for _, m := range s.Messages {
		if strings.Contains(m.Text, "private thought") || strings.Contains(m.Text, "secret output") {
			t.Fatalf("non-visible content leaked into %q", m.Text)
		}
	}
	if child.Session.ID != "zcode:subagent_agent_5f0c" || child.Session.Project != "/project" ||
		len(child.Messages) != 1 || child.Messages[0].Text != "child prompt" {
		t.Fatalf("subagent session missing or wrong: %+v", child)
	}
}
