package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCodexKeepsLargeAndRepeatedMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "large.jsonl")
	large := strings.Repeat("x", 1024*1024+1) + " needle"
	writeTemp(t, path, strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"large","cwd":"/project"}}`,
		fmt.Sprintf(`{"timestamp":"2026-09-27T00:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`, large),
		`{"timestamp":"2026-09-27T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"retry"}]}}`,
		`{"timestamp":"2026-09-27T00:00:03Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"retry"}]}}`,
		`{"timestamp":"2026-09-27T00:00:04Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
	}, "\n"))
	_, msgs, err := IngestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 || msgs[0].Text != large || msgs[1].Text != "retry" || msgs[2].Text != "retry" || msgs[3].Text != "done" {
		t.Fatalf("large or repeated utterance lost: count=%d", len(msgs))
	}
}

func TestIngestKeepsConversationDropsToolCalls(t *testing.T) {
	dir := t.TempDir()

	codex := dir + "/.codex/s.jsonl"
	writeTemp(t, codex, strings.Join([]string{
		`{"timestamp":"2026-07-18T21:33:28Z","type":"session_meta","payload":{"session_id":"s1","cwd":"/home/u/Projects/organizer"}}`,
		`{"timestamp":"2026-07-18T21:33:29Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the redship bug"}]}}`,
		`{"timestamp":"2026-07-18T21:33:30Z","type":"response_item","payload":{"type":"reasoning","text":"let me think"}}`,
		`{"timestamp":"2026-07-18T21:33:31Z","type":"response_item","payload":{"type":"custom_tool_call","name":"bash","input":"rm -rf /"}}`,
		`{"timestamp":"2026-07-18T21:33:32Z","type":"response_item","payload":{"type":"custom_tool_call_output","output":"done"}}`,
		`{"timestamp":"2026-07-18T21:33:33Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fixed redship"}]}}`,
		`{"timestamp":"2026-07-18T21:33:34Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"ship the redship"}]}}}`,
		`{"timestamp":"2026-07-18T21:33:35Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"Reasoning","content":[{"type":"Text","text":"hmm"}]}}}`,
		`{"timestamp":"2026-07-18T21:33:36Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":"rm -rf /"}}}`,
		`{"timestamp":"2026-07-18T21:33:37Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","content":[{"type":"Text","text":"shipped redship\n[external_agent_tool_call: Shell]\ncommand: rm -rf /\n[/external_agent_tool_call]"}]}}}`,
	}, "\n"))

	claude := dir + "/.claude/projects/slug/c.jsonl"
	writeTemp(t, claude, strings.Join([]string{
		`{"type":"user","sessionId":"c1","cwd":"/home/u/omen","timestamp":"2026-09-27T00:59:28Z","message":{"role":"user","content":"deploy the redship"}}`,
		`{"type":"assistant","sessionId":"c1","cwd":"/home/u/omen","timestamp":"2026-09-27T00:59:29Z","message":{"role":"assistant","content":[{"type":"text","text":"deploying now"},{"type":"tool_use","name":"Bash","input":{"cmd":"rm -rf /"}}]}}`,
		`{"type":"attachment","sessionId":"c1","cwd":"/home/u/omen"}`,
	}, "\n"))

	muse := dir + "/muse/sessions/session.jsonl"
	writeTemp(t, muse, strings.Join([]string{
		`{"payload_type":"runtime.session.route_facts","stream":{"id":"m1"},"recorded_at":1789984409011408,"payload":{"record":{"cwd":"/home/u/portal"}}}`,
		`{"payload_type":"runtime.user_intent.accepted","stream":{"id":"m1"},"recorded_at":1789984409395508,"payload":{"refill_blocks":[{"kind":"text","text":"migrate the redship db"}]}}`,
		`{"payload_type":"runtime.session","stream":{"id":"m1"},"recorded_at":1789984409398898,"payload":{"event":{"kind":"started","prompt":"migrate the redship db"}}}`,
		`{"payload_type":"runtime.session","stream":{"id":"m1"},"recorded_at":1789984409400000,"payload":{"event":{"kind":"assistant_tool_calls_committed"}}}`,
		`{"payload_type":"runtime.session","stream":{"id":"m1"},"recorded_at":1789984409400001,"payload":{"event":{"kind":"output","chunk":"some tool output"}}}`,
	}, "\n"))

	cursor := dir + "/x/projects/home-u-Proj/agent-transcripts/t1/t1.jsonl"
	writeTemp(t, cursor, strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"paint the redship hull"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"painting"},{"type":"tool_use","name":"Read","input":{"path":"/etc/passwd"}}]}}`,
	}, "\n"))

	for path, want := range map[string]struct {
		source, project string
		roles           []string
		mustContain     []string
		mustNotContain  []string
	}{
		codex: {"codex", "/home/u/Projects/organizer",
			[]string{"user", "assistant", "user", "assistant"},
			[]string{"redship bug", "fixed redship", "ship the redship", "shipped redship"},
			[]string{"rm -rf", "let me think", "done", "external_agent_tool_call", "hmm"}},
		claude: {"claude", "/home/u/omen",
			[]string{"user", "assistant"},
			[]string{"deploy the redship", "deploying now"},
			[]string{"rm -rf", "Bash"}},
		muse: {"muse", "/home/u/portal",
			[]string{"user"}, // started.prompt duplicates the intent text
			[]string{"migrate the redship db"},
			[]string{"tool output"}},
		cursor: {"cursor", "/home/u/Proj",
			[]string{"user", "assistant"},
			[]string{"paint the redship hull", "painting"},
			[]string{"/etc/passwd", "Read"}},
	} {
		sess, msgs, err := IngestFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if sess.Source != want.source || sess.Project != want.project {
			t.Errorf("%s: got %s/%s want %s/%s", path, sess.Source, sess.Project, want.source, want.project)
		}
		if len(msgs) != len(want.roles) {
			t.Fatalf("%s: got %d messages want %d", path, len(msgs), len(want.roles))
		}
		for i, role := range want.roles {
			if msgs[i].Role != role {
				t.Errorf("%s msg %d: role %q want %q", path, i, msgs[i].Role, role)
			}
		}
		var all strings.Builder
		for _, m := range msgs {
			all.WriteString(m.Text)
			all.WriteString("\n")
		}
		for _, s := range want.mustContain {
			if !strings.Contains(all.String(), s) {
				t.Errorf("%s: texts missing %q", path, s)
			}
		}
		for _, s := range want.mustNotContain {
			if strings.Contains(all.String(), s) {
				t.Errorf("%s: tool/reasoning text leaked: %q", path, s)
			}
		}
	}
}
