package ingest

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeKeepsVisibleTextOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode", "opencode.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`CREATE TABLE project (id TEXT, worktree TEXT)`,
		`CREATE TABLE session_v2 (id TEXT, project_id TEXT, directory TEXT, time_created INTEGER)`,
		`CREATE TABLE session_message (id TEXT, session_id TEXT, type TEXT, seq INTEGER, time_created INTEGER, data TEXT)`,
		`INSERT INTO project VALUES ('p', '/fallback')`,
		`INSERT INTO session_v2 VALUES ('s', 'p', '/project', 1789980111419)`,
		`INSERT INTO session_v2 VALUES ('child', 'p', '', 1789980111420)`,
		`INSERT INTO session_message VALUES ('a', 's', 'user', 1, 1789980111525, '{"text":"hello"}')`,
		`INSERT INTO session_message VALUES ('b', 's', 'assistant', 2, 1789980111526, '{"content":[{"type":"reasoning","text":"private thought"},{"type":"tool","name":"shell","state":{"output":"secret output"}},{"type":"text","text":"visible answer"},{"type":"text","text":"second paragraph"}]}')`,
		`INSERT INTO session_message VALUES ('c', 's', 'system', 3, 1789980111527, '{"text":"system note"}')`,
		`INSERT INTO session_message VALUES ('d', 's', 'compaction', 4, 1789980111528, '{"text":"summary to drop"}')`,
		`INSERT INTO session_message VALUES ('e', 'child', 'user', 1, 1789980111529, '{"text":"child prompt"}')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	sessions, err := IngestOpenCode(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions", len(sessions))
	}
	s, child := sessions[0], sessions[1]
	if s.Session.ID != "opencode:s" || s.Session.Project != "/project" || s.Session.Source != "opencode" ||
		!s.Session.StartedAt.Equal(time.UnixMilli(1789980111419).UTC()) || len(s.Messages) != 3 {
		t.Fatalf("wrong parent session metadata or message count: %+v", s.Session)
	}
	if s.Messages[0].Role != "user" || s.Messages[0].Text != "hello" ||
		s.Messages[1].Role != "assistant" || s.Messages[1].Text != "visible answer\nsecond paragraph" ||
		s.Messages[1].Seq != 1 || s.Messages[2].Role != "system" || s.Messages[2].Text != "system note" ||
		!s.Messages[1].CreatedAt.Equal(time.UnixMilli(1789980111526).UTC()) {
		t.Fatalf("wrong visible conversation: %+v", s.Messages)
	}
	for _, m := range s.Messages {
		if strings.Contains(m.Text, "private thought") || strings.Contains(m.Text, "secret output") || strings.Contains(m.Text, "summary to drop") {
			t.Fatalf("non-visible content leaked into %q", m.Text)
		}
	}
	if child.Session.ID != "opencode:child" || child.Session.Project != "/fallback" || len(child.Messages) != 1 || child.Messages[0].Project != "/fallback" {
		t.Fatalf("child or fallback project missing: %+v", child)
	}
}
