package ingest

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jmarceno/duck-mem/internal/store"
)

func atTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestSessionModelEffortAndDates checks that each harness's model and
// effort reach the session, that formats carrying neither store the
// Unknown placeholder, and that the session dates come from the source
// records rather than from ingest time.
func TestSessionModelEffortAndDates(t *testing.T) {
	dir := t.TempDir()

	claude := dir + "/.claude/projects/slug/c.jsonl"
	writeTemp(t, claude, strings.Join([]string{
		`{"type":"user","sessionId":"c1","cwd":"/home/u/omen","timestamp":"2026-09-27T00:59:28Z","message":{"role":"user","content":"deploy the redship"}}`,
		`{"type":"assistant","sessionId":"c1","cwd":"/home/u/omen","timestamp":"2026-09-27T00:59:29Z","message":{"role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"deploying now"}]},"effort":"low"}`,
	}, "\n"))

	dshLines := []string{
		`{"type":"session","version":4,"id":"` + dshTestID + `","createdAt":1789000000000,"cwd":"/home/u/Projects/demo"}`,
		`{"type":"model/selection","seq":1,"time":1789000000100,"data":{"provider":"command-code","model":"deepseek/deepseek-v4.1-flash","reasoningEffort":"max"}}`,
		`{"type":"user/message","seq":2,"time":1789000000200,"data":{"content":[{"type":"text","text":"how do I add a source?"}],"source":{"kind":"user"},"role":"user"}}`,
		`{"type":"assistant/message","seq":3,"time":1789000000400,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[{"type":"text","text":"Add a kind and a parser."}]}}}`,
	}
	dsh := writeDSHSession(t, encodeDSHLines(t, dshLines))

	muse := dir + "/muse/sessions/session.jsonl"
	writeTemp(t, muse, strings.Join([]string{
		`{"payload_type":"runtime.session.metadata","stream":{"id":"m1"},"recorded_at":1789984409011408,"payload":{"record":{"model_id":"muse-spark-1.3-contributor","provider_id":"meta","workspace_root":"/home/u/portal"}}}`,
		`{"payload_type":"runtime.session","stream":{"id":"m1"},"recorded_at":1789984409398898,"payload":{"event":{"kind":"started","prompt":"migrate the redship db"}}}`,
	}, "\n"))

	for path, want := range map[string]struct {
		model, effort    string
		started, updated time.Time
	}{
		claude: {"claude-opus-5-5", "low", atTime(t, "2026-09-27T00:59:28Z"), atTime(t, "2026-09-27T00:59:29Z")},
		dsh:    {"deepseek/deepseek-v4.1-flash", "max", time.UnixMilli(1789000000000).UTC(), time.UnixMilli(1789000000400).UTC()},
		muse:   {"muse-spark-1.3-contributor", store.UnknownEffort, time.UnixMicro(1789984409011408).UTC(), time.UnixMicro(1789984409398898).UTC()},
	} {
		sess, _, err := IngestFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if sess.Model != want.model || sess.Effort != want.effort {
			t.Errorf("%s: model/effort %q/%q want %q/%q", path, sess.Model, sess.Effort, want.model, want.effort)
		}
		if !sess.StartedAt.Equal(want.started) || !sess.UpdatedAt.Equal(want.updated) {
			t.Errorf("%s: dates %v → %v want %v → %v", path, sess.StartedAt, sess.UpdatedAt, want.started, want.updated)
		}
	}

	// Codex carries no effort in its rollout log: model from the system
	// prompt's provenance, effort as the Unknown placeholder.
	codex := dir + "/.codex/s.jsonl"
	writeTemp(t, codex, strings.Join([]string{
		`{"timestamp":"2026-07-18T21:33:28Z","type":"session_meta","payload":{"session_id":"s1","cwd":"/home/u/Projects/organizer","base_instructions":{"provenance":{"model":"gpt-6.1-sol"}}}}`,
		`{"timestamp":"2026-07-18T21:33:29Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the redship bug"}]}}`,
	}, "\n"))
	sess, _, err := IngestFile(codex)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Model != "gpt-6.1-sol" || sess.Effort != store.UnknownEffort {
		t.Errorf("codex model/effort: %q/%q want gpt-6.1-sol/%s", sess.Model, sess.Effort, store.UnknownEffort)
	}
}

// TestCursorSessionUsesOSFileTimes checks that a Cursor session, whose
// transcript carries no dates or model at all, takes the OS file times
// and the Unknown placeholder.
func TestCursorSessionUsesOSFileTimes(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/.cursor/projects/home-u-Proj/agent-transcripts/t1/t1.jsonl"
	writeTemp(t, path, strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"paint the redship hull"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"painting"}]}}`,
	}, "\n"))
	mod := time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, mod.Add(-2*time.Hour), mod); err != nil {
		t.Fatal(err)
	}
	sess, _, err := IngestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Model != store.UnknownModel || sess.Effort != store.UnknownEffort {
		t.Errorf("model/effort %q/%q want Unknown", sess.Model, sess.Effort)
	}
	// Birth time depends on filesystem support; the mtime always applies.
	if sess.StartedAt.IsZero() {
		t.Errorf("started_at not filled from the OS")
	}
	if !sess.UpdatedAt.Equal(mod) {
		t.Errorf("updated_at %v want %v", sess.UpdatedAt, mod)
	}
}
