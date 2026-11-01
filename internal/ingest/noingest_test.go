package ingest

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmarceno/duck-mem/internal/store"
)

func TestExclusionTagFirstWordOnly(t *testing.T) {
	user := func(texts ...string) []store.Message {
		var msgs []store.Message
		for i, tx := range texts {
			msgs = append(msgs, store.Message{Seq: i, Role: "user", Text: tx})
		}
		return msgs
	}
	cases := []struct {
		msgs []store.Message
		want string
	}{
		{user("private the launch plans"), "private"},
		{user("Private: keep this out"), "private"},
		{user("#noduck, payroll"), "noduck"},
		{user("noingest"), "noingest"},
		{user("privateer the ship"), ""},
		{user("privacy of the endpoint"), ""},
		{append([]store.Message{{Seq: 0, Role: "assistant", Text: "yes"}}, user("private x")...), "private"},
		{[]store.Message{{Seq: 0, Role: "assistant", Text: "private x"}}, ""},
	}
	for i, c := range cases {
		if got := ExclusionTag(c.msgs); got != c.want {
			t.Errorf("case %d (%q): got %q want %q", i, c.msgs[0].Text, got, c.want)
		}
	}
}

func TestExclusionTagOnParsedCodexSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "secret.jsonl")
	writeTemp(t, path, strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"secret","cwd":"/project"}}`,
		fmt.Sprintf(`{"timestamp":"2026-11-01T00:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`, "private the payroll merge"),
		`{"timestamp":"2026-11-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"keeping it out"}]}}`,
	}, "\n"))
	_, msgs, err := IngestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := ExclusionTag(msgs); got != "private" {
		t.Fatalf("parsed session opening with private not detected: %q", got)
	}
}
