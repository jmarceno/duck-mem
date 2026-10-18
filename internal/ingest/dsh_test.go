package ingest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

const dshTestID = "session-11111111-2222-3333-4444-555555555555"

// dshTestLines is one harness session holding a real prompt, the context the
// harness injects as user-role records, a system prompt, an assistant answer
// with reasoning and a tool call, a tool round trip, and a second answer.
var dshTestLines = []string{
	`{"type":"session","version":4,"id":"` + dshTestID + `","createdAt":1789000000000,"cwd":"/home/u/Projects/demo","isSeeded":false,"delegationDepth":0,"agentPreset":"standard"}`,
	`{"type":"permission/preset","seq":0,"time":1789000000050,"data":{"preset":"workspace-write"}}`,
	`{"type":"agent/inbox/spliced","seq":1,"time":1789000000100,"data":{"target":"next-turn","start":0,"inserted":[{"content":[{"type":"text","text":"how do I add a source?"}],"source":{"kind":"user","rpcId":"r1"},"role":"user","id":"u1"}]}}`,
	`{"type":"user/message","seq":2,"time":1789000000200,"data":{"content":[{"type":"text","text":"how do I add a source?"}],"source":{"kind":"user","rpcId":"r1","clientTimeZone":"UTC"},"role":"user","id":"u1"}}`,
	`{"type":"user/message","seq":3,"time":1789000000201,"data":{"content":[{"type":"text","text":"<system-reminder>workspace instructions</system-reminder>"}],"source":{"kind":"agent-instructions","form":"markdown"},"role":"user"}}`,
	`{"type":"user/message","seq":4,"time":1789000000202,"data":{"content":[{"type":"text","text":"Current runtime context."}],"source":{"kind":"runtime-context","sections":[]},"role":"user"}}`,
	`{"type":"user/message","seq":5,"time":1789000000203,"data":{"content":[{"type":"text","text":"<system-reminder>skill catalog</system-reminder>"}],"source":{"kind":"skill-catalog","entries":[]},"role":"user"}}`,
	`{"type":"system/message","seq":6,"time":1789000000204,"data":{"turn":1,"step":1,"message":{"role":"system","content":[{"type":"text","text":"You are an AI agent powered by DeepSeek Harness."}]}}}`,
	`{"type":"assistant/message","seq":7,"time":1789000000300,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[{"type":"reasoning","text":"private thought"},{"type":"text","text":"Add a kind and a parser."},{"type":"tool-call","id":"c1","name":"bash","arguments":"{}"}]}}}`,
	`{"type":"tool/call","seq":8,"time":1789000000310,"data":{"turn":1,"step":1,"callId":"c1","name":"bash","arguments":"{\"command\":\"ls\"}"}}`,
	`{"type":"tool/result","seq":9,"time":1789000000320,"data":{"turn":1,"step":1,"message":{"role":"tool","content":[{"type":"tool-result","text":"secret tool output"}]}}}`,
	`{"type":"assistant/message","seq":10,"time":1789000000400,"data":{"turn":1,"step":2,"message":{"role":"assistant","content":[{"type":"text","text":"Then update the docs."},{"type":"text","text":"Second paragraph."}]}}}`,
}

// encodeDSHLines writes each record as its own zstd frame, the way the
// harness appends events to a session file.
func encodeDSHLines(t *testing.T, lines []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, line := range lines {
		w, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func writeDSHSession(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".dsh", "sessions", "--home-u-Projects-demo--", dshTestID, "session.v4.jsonl.zstd")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDSHKeepsUserPromptsAndAssistantText(t *testing.T) {
	path := writeDSHSession(t, encodeDSHLines(t, dshTestLines))
	if Classify(path) != KindDSH {
		t.Fatalf("classified as %v", Classify(path))
	}
	sess, msgs, err := IngestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "dsh:"+dshTestID || sess.Source != "dsh" || sess.Project != "/home/u/Projects/demo" ||
		!sess.StartedAt.Equal(time.UnixMilli(1789000000000).UTC()) {
		t.Fatalf("wrong session metadata: %+v", sess)
	}
	want := []struct {
		role string
		text string
		at   int64
	}{
		{"user", "how do I add a source?", 1789000000200},
		{"assistant", "Add a kind and a parser.", 1789000000300},
		{"assistant", "Then update the docs.\nSecond paragraph.", 1789000000400},
	}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		m := msgs[i]
		if m.Role != w.role || m.Text != w.text || m.Seq != i || m.Source != "dsh" || m.Project != sess.Project ||
			!m.CreatedAt.Equal(time.UnixMilli(w.at).UTC()) || m.SessionID != sess.ID {
			t.Errorf("message %d = %+v, want %s %q", i, m, w.role, w.text)
		}
	}
	for _, m := range msgs {
		for _, dropped := range []string{"private thought", "secret tool output", "system-reminder", "runtime context", "DeepSeek Harness", "skill catalog"} {
			if strings.Contains(m.Text, dropped) {
				t.Errorf("%q leaked into %q", dropped, m.Text)
			}
		}
	}
}

func TestDSHTornTailKeepsDecodedRecords(t *testing.T) {
	// A torn final frame stands in for a session the harness was still
	// appending to: the records already written must survive.
	last := encodeDSHLines(t, dshTestLines[len(dshTestLines)-1:])
	path := writeDSHSession(t, append(encodeDSHLines(t, dshTestLines[:len(dshTestLines)-1]), last[:6]...))
	sess, msgs, err := IngestFile(path)
	if err != nil || sess.ID != "dsh:"+dshTestID {
		t.Fatalf("torn tail: session=%+v error=%v", sess, err)
	}
	if len(msgs) != 2 || msgs[1].Text != "Add a kind and a parser." {
		t.Fatalf("torn tail lost decoded records: %+v", msgs)
	}
}

func TestDSHUnknownVersionFailsLoudly(t *testing.T) {
	lines := append([]string{`{"type":"session","version":5,"id":"` + dshTestID + `","createdAt":1789000000000,"cwd":"/home/u/demo"}`}, dshTestLines[1:]...)
	path := writeDSHSession(t, encodeDSHLines(t, lines))
	if _, _, err := IngestFile(path); !errors.Is(err, ErrUnexpectedFormat) {
		t.Fatalf("version 5 should be reported as unexpected format, got %v", err)
	}
}
