package ingest

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// antigravityTranscriptPath builds the only path Antigravity classifies as a
// transcript: the untruncated log under a conversation's brain directory.
func antigravityTranscriptPath(home, id string) string {
	return filepath.Join(home, ".gemini", "antigravity", "brain", id,
		".system_generated", "logs", "transcript_full.jsonl")
}

// TestAntigravityKeepsPromptsAndReplies checks the record mapping: the prompt
// wrapper and the harness metadata blocks are stripped, tool output and tool
// calls are dropped, and the model and dates come from the records themselves.
func TestAntigravityKeepsPromptsAndReplies(t *testing.T) {
	home := t.TempDir()
	id := "a7cacf96-0f66-4723-bd39-1d0d61a33cd8"
	path := antigravityTranscriptPath(home, id)
	initial := strings.Join([]string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-19T10:42:38Z","content":"<USER_REQUEST>\ndo not add anything just give me some data\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-19T07:42:38-03:00.\n</ADDITIONAL_METADATA>\n<USER_SETTINGS_CHANGE>\nThe user changed setting 'Model Selection'.\n</USER_SETTINGS_CHANGE>"}`,
		// A step that only called tools carries tool_calls and no content.
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-19T10:42:38Z","model":"gemini-3.8-flash-high","tool_calls":[{"name":"run_command","args":{"CommandLine":"git status"}}]}`,
		`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-19T10:42:43Z","content":"Command exited with code 0.\n\nCommand output:\nsecret tool output"}`,
		`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-19T10:42:44Z","model":"gemini-3.8-flash-high","thinking":"private thought","content":"Here is some data."}`,
	}, "\n") + "\n"
	writeTemp(t, path, initial)

	sess, msgs, err := IngestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "antigravity:"+id || sess.Source != "antigravity" || sess.Project != "" {
		t.Fatalf("wrong session metadata: %+v", sess)
	}
	if sess.Model != "gemini-3.8-flash" || sess.Effort != "high" {
		t.Fatalf("model/effort not split: %q/%q", sess.Model, sess.Effort)
	}
	if got := sess.StartedAt.UTC().Format("2006-01-02T15:04:05Z"); got != "2026-11-19T10:42:38Z" {
		t.Fatalf("started at %s", got)
	}
	if got := sess.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"); got != "2026-11-19T10:42:44Z" {
		t.Fatalf("updated at %s", got)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[0].Text != "do not add anything just give me some data" ||
		msgs[1].Role != "assistant" || msgs[1].Text != "Here is some data." || msgs[1].Seq != 1 {
		t.Fatalf("wrong conversation: %+v", msgs)
	}
	for _, m := range msgs {
		for _, leak := range []string{"USER_REQUEST", "ADDITIONAL_METADATA", "USER_SETTINGS_CHANGE", "Command output", "secret tool output", "private thought", "git status"} {
			if strings.Contains(m.Text, leak) {
				t.Errorf("message %d kept %q: %q", m.Seq, leak, m.Text)
			}
		}
		if m.Project != "" || m.Source != "antigravity" {
			t.Errorf("message %d carries %q/%q", m.Seq, m.Source, m.Project)
		}
	}

	// One appended step adds one message and keeps the sequence.
	appended := initial + `{"step_index":4,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-19T10:43:46Z","content":"<USER_REQUEST>\nnext\n</USER_REQUEST>"}` + "\n"
	writeTemp(t, path, appended)
	updated, added, err := IngestAppended(path, int64(len(initial)), sess, &msgs[len(msgs)-1], Projects{})
	if err != nil || updated.ID != sess.ID || updated.Project != sess.Project ||
		len(added) != 1 || added[0].Seq != 2 || added[0].Text != "next" {
		t.Fatalf("append parse: session=%+v messages=%+v err=%v", updated, added, err)
	}
}

// TestAntigravityOnlyFullTranscriptIsIngestible guards the duplicate rule: the
// truncated twin and the chunk partitions hold the same conversation.
func TestAntigravityOnlyFullTranscriptIsIngestible(t *testing.T) {
	home := t.TempDir()
	logs := filepath.Join(home, ".gemini", "antigravity", "brain", "c1", ".system_generated", "logs")
	for _, path := range []string{
		filepath.Join(logs, "transcript.jsonl"),
		filepath.Join(logs, "chunks", "transcript_full", "00000000.jsonl"),
		filepath.Join(logs, "chunks", "transcript", "00000000.jsonl"),
	} {
		if got := Classify(path); got != KindUnknown {
			t.Errorf("%s classified as %v", path, got)
		}
	}
	if got := Classify(filepath.Join(logs, "transcript_full.jsonl")); got != KindAntigravity {
		t.Errorf("full transcript classified as %v", got)
	}
}

// TestAntigravityModelEffort covers the model-id suffixes the app writes.
func TestAntigravityModelEffort(t *testing.T) {
	for id, want := range map[string][2]string{
		"gemini-3.8-flash-high": {"gemini-3.8-flash", "high"},
		"gemini-3.8-flash-low":  {"gemini-3.8-flash", "low"},
		// Not reasoning levels: part of the model name.
		"gemini-3.8-flash-tiered": {"gemini-3.8-flash-tiered", ""},
		"gemini-3.5-flash-lite":   {"gemini-3.5-flash-lite", ""},
		"gemini-3.8-flash-n":      {"gemini-3.8-flash-n", ""},
	} {
		model, effort := antigravityModelEffort(id)
		if model != want[0] || effort != want[1] {
			t.Errorf("%s: got %q/%q want %q/%q", id, model, effort, want[0], want[1])
		}
	}
}

// TestAntigravityProjectsFromSummaries reads the workspace mapping from the
// index, and treats a missing or unusable one as unresolved rather than
// inventing a path or failing the cycle.
func TestAntigravityProjectsFromSummaries(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".gemini", "antigravity")
	path := filepath.Join(dir, "conversation_summaries.db")
	writeTemp(t, path, "")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`CREATE TABLE conversation_summaries (conversation_id TEXT, workspace_uris TEXT)`,
		`INSERT INTO conversation_summaries VALUES ('c1', '["file:///home/u/Projects/duck-mem"]')`,
		`INSERT INTO conversation_summaries VALUES ('c2', '')`,
		`INSERT INTO conversation_summaries VALUES ('c3', 'not json')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	projects := LoadAntigravityProjects(dir)
	if projects["c1"] != "/home/u/Projects/duck-mem" || projects["c2"] != "" || projects["c3"] != "" {
		t.Fatalf("wrong workspace mapping: %+v", projects)
	}

	transcript := antigravityTranscriptPath(home, "c1")
	writeTemp(t, transcript, `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-19T10:42:38Z","content":"hello"}`+"\n")
	sess, msgs, err := IngestFile(transcript)
	if err != nil || sess.Project != "/home/u/Projects/duck-mem" || len(msgs) != 1 || msgs[0].Project != sess.Project {
		t.Fatalf("workspace not applied: session=%+v messages=%+v err=%v", sess, msgs, err)
	}

	// An unresolved conversation keeps an empty project, never a synthetic one.
	unresolved := antigravityTranscriptPath(home, "c2")
	writeTemp(t, unresolved, `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-19T10:42:38Z","content":"hello"}`+"\n")
	if sess, _, err := IngestFile(unresolved); err != nil || sess.Project != "" {
		t.Fatalf("unresolved workspace became a path: %+v err=%v", sess, err)
	}

	// A missing index is not an error.
	if got := LoadAntigravityProjects(filepath.Join(t.TempDir(), "missing")); len(got) != 0 {
		t.Fatalf("missing index produced %+v", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
