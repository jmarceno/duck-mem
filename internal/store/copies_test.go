package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestCopyIdentityConservativeMatching(t *testing.T) {
	original := []Message{
		{Role: "user", Text: "Investigate the redship queue before changing the worker."},
		{Role: "assistant", Text: strings.Repeat("The redship queue retains the running worker and cancels only its active operation. ", 7)},
		{Role: "user", Text: "Implement the cancellation fix and preserve the queued requests."},
	}
	wrapped := slices.Clone(original)
	wrapped[0].Text = "<timestamp>Monday</timestamp>\n<user_query>\n" + wrapped[0].Text + "\n</user_query>"
	wrapped = append(wrapped, Message{Role: "assistant", Text: "<EXTERNAL SESSION IMPORTED>"},
		Message{Role: "user", Text: "# AGENTS.md instructions for /project\ncontext"},
		Message{Role: "user", Text: `<external_codex_apps_open_page>{"page_id":null}</external_codex_apps_open_page>`})
	a := copySession{Session: Session{ID: "a", Source: "cursor", Project: "p"}, copyIdentity: identityOf(original)}
	b := copySession{Session: Session{ID: "b", Source: "codex", Project: "p"}, copyIdentity: identityOf(wrapped)}
	if !coveredBy(b, a) || !b.Imported {
		t.Fatal("known transport wrappers should preserve the identity")
	}
	for _, tc := range []struct {
		name   string
		change func(*copySession)
	}{
		{"different project", func(s *copySession) { s.Project = "other" }},
		{"same harness", func(s *copySession) { s.Source = "cursor" }},
		{"short conversation", func(s *copySession) { s.copyIdentity = identityOf(original[:2]) }},
		{"internal whitespace", func(s *copySession) {
			m := slices.Clone(original)
			m[1].Text = strings.Replace(m[1].Text, "redship queue", "redship  queue", 1)
			s.copyIdentity = identityOf(m)
		}},
		{"new failure", func(s *copySession) {
			s.copyIdentity = identityOf(append(slices.Clone(original), Message{Role: "note", Text: "turn failed: usage limit"}))
		}},
		{"real tagged user text", func(s *copySession) {
			s.copyIdentity = identityOf(append(slices.Clone(original), Message{Role: "user", Text: "<request>hello world</request>"}))
		}},
		{"reordered", func(s *copySession) {
			s.Hashes = slices.Clone(s.Hashes)
			s.Hashes[0], s.Hashes[1] = s.Hashes[1], s.Hashes[0]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := b
			tc.change(&c)
			if coveredBy(c, a) {
				t.Fatal("uncertain or distinct content was suppressed")
			}
		})
	}
	extended := a
	extended.Hashes = append(slices.Clone(a.Hashes), "new-message")
	if !coveredBy(b, extended) {
		t.Fatal("an exact shorter snapshot is covered")
	}
}

func TestCopySuppressionPromotionAndOwnerRewrite(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	owner := Session{ID: "cursor-original", Source: "cursor", Project: "p", Path: "original.jsonl"}
	copySess := Session{ID: "codex-copy", Source: "codex", Project: "p", Path: "copy.jsonl"}
	makeMessages := func(s Session) []Message {
		return []Message{
			{SessionID: s.ID, Seq: 0, Source: s.Source, Project: s.Project, Role: "user", Text: "Fix redship queue cancellation."},
			{SessionID: s.ID, Seq: 1, Source: s.Source, Project: s.Project, Role: "assistant", Text: strings.Repeat("The redship queue serializes worker requests and preserves cancellation boundaries. ", 7)},
			{SessionID: s.ID, Seq: 2, Source: s.Source, Project: s.Project, Role: "user", Text: "Implement redship cancellation without losing queued requests."},
		}
	}
	original, copied := makeMessages(owner), makeMessages(copySess)
	copied = append(copied, Message{SessionID: copySess.ID, Seq: 3, Source: copySess.Source, Project: "p", Role: "assistant", Text: "<EXTERNAL SESSION IMPORTED>"})
	// Backfill already-indexed sessions, not just newly imported files.
	for _, s := range []Session{owner, copySess} {
		if err := db.UpsertSession(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SyncMessages(owner.ID, original); err != nil {
		t.Fatal(err)
	}
	if err := db.SyncMessages(copySess.ID, copied); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.IndexNew(1); err != nil {
		t.Fatal(err)
	}
	if err := db.PrepareSessionCopies(); err != nil {
		t.Fatal(err)
	}
	if err := db.PrepareSessionCopies(); err != nil {
		t.Fatal(err)
	}
	suppress := func(want int) {
		t.Helper()
		n, err := db.SuppressSessionCopies()
		if err != nil || n != want {
			t.Fatalf("suppressed=%d want=%d error=%v", n, want, err)
		}
	}
	suppress(1)
	suppress(0)
	if err := db.SaveCheckpoint(FileCheckpoint{Path: copySess.Path, SessionID: copySess.ID}); err != nil {
		t.Fatal(err)
	}
	if err := db.SyncConversation(owner, []Message{{SessionID: owner.ID, Seq: 3, Source: owner.Source, Project: owner.Project, Role: "user", Text: "The original conversation continued here."}}, true, nil); err != nil {
		t.Fatal(err)
	}
	if copy, err := db.IsSessionCopy(copySess.ID); err != nil || !copy {
		t.Fatalf("owner growth should still cover its older snapshot: %v %v", copy, err)
	}
	result, err := db.ResolveSession(ctx, copySess.ID)
	if err != nil || result.Session.ID != owner.ID {
		t.Fatalf("alias resolution: %+v %v", result, err)
	}
	hits, err := db.Search(ctx, "redship cancellation", "p", "", 20)
	if err != nil || len(hits) == 0 {
		t.Fatalf("search: %+v %v", hits, err)
	}
	for _, h := range hits {
		if h.SessionID != owner.ID {
			t.Fatal("suppressed copy still appears in search")
		}
	}
	for _, table := range []string{"messages", "msg_terms", "msg_edges"} {
		var n int
		if err := db.sql.QueryRow(`SELECT count(*) FROM `+table+` WHERE session_id=?`, copySess.ID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("copy contributed to %s: %d %v", table, n, err)
		}
	}
	var df int
	if err := db.sql.QueryRow(`SELECT msgs FROM term_df WHERE project='p' AND term='redship'`).Scan(&df); err != nil || df != 3 {
		t.Fatalf("duplicate document frequency: %d %v", df, err)
	}
	// An unchanged copy must be restored when its owner is rewritten.
	rewritten := slices.Clone(original)
	rewritten[1].Text = "A completely different worker design."
	failed := func(s Session) (Session, []Message, error) { return s, nil, errors.New("log unavailable") }
	if err := db.SyncConversation(owner, rewritten, false, failed); err == nil {
		t.Fatal("owner rewrite lost an unavailable copy")
	}
	current, err := db.Window(ctx, owner.ID, 0, 9, -1, true)
	if err != nil || current[1].Text != original[1].Text {
		t.Fatal("failed restoration modified the owner")
	}
	load := func(s Session) (Session, []Message, error) { return copySess, copied, nil }
	if err := db.SyncConversation(owner, rewritten, false, load); err != nil {
		t.Fatal(err)
	}
	isCopy, err := db.IsSessionCopy(copySess.ID)
	if err != nil || isCopy {
		t.Fatalf("rewritten owner's copy was not restored: %v %v", isCopy, err)
	}
	checkpoint, err := db.GetCheckpoint(copySess.Path)
	if err != nil || checkpoint != nil {
		t.Fatalf("restored snapshot would reuse a stale append checkpoint: %+v %v", checkpoint, err)
	}
	suppress(0)
	// Re-establish the copy, then promote it for the observed native Codex turn.
	if err := db.SyncConversation(owner, original, false, load); err != nil {
		t.Fatal(err)
	}
	suppress(1)
	continued := append(slices.Clone(copied), Message{SessionID: copySess.ID, Seq: 4, Source: "codex", Project: "p", Role: "user", Text: "hello world"}, Message{SessionID: copySess.ID, Seq: 5, Source: "codex", Project: "p", Role: "assistant", Text: "Hello!"})
	if err := db.SyncConversation(copySess, continued, false, load); err != nil {
		t.Fatal(err)
	}
	suppress(0)
	if _, _, err := db.IndexNew(1); err != nil {
		t.Fatal(err)
	}
	result, err = db.ResolveSession(ctx, copySess.ID)
	if err != nil || result.Session.ID != copySess.ID || result.Messages != len(continued) {
		t.Fatalf("continuation was not promoted: %+v %v", result, err)
	}
	hits, err = db.Search(ctx, "hello world", "p", "codex", 10)
	if err != nil || !slices.ContainsFunc(hits, func(h Hit) bool { return h.SessionID == copySess.ID && h.Seq == 4 }) {
		t.Fatalf("new Codex turn missing: %+v %v", hits, err)
	}
	// A later append uses the original raw sequence, including the import marker.
	next := Message{SessionID: copySess.ID, Seq: 6, Source: "codex", Project: "p", Role: "user", Text: "another distinct request"}
	if err := db.SyncConversation(copySess, []Message{next}, true, load); err != nil {
		t.Fatal(err)
	}
	last, err := db.LastMessage(copySess.ID)
	if err != nil || last.Seq != 6 || last.Text != next.Text {
		t.Fatalf("append sequence: %+v %v", last, err)
	}
	// Current Codex import markers can be metadata rather than messages.
	// Keep the previously retained session even without that optional hint.
	previous := Session{ID: "z-previous", Source: "cursor", Project: "stable"}
	newCopy := Session{ID: "a-new", Source: "codex", Project: "stable"}
	for _, s := range []Session{previous, newCopy} {
		if err := db.SyncConversation(s, makeMessages(s), false, nil); err != nil {
			t.Fatal(err)
		}
	}
	suppress(1)
	result, err = db.ResolveSession(ctx, newCopy.ID)
	if err != nil || result.Session.ID != previous.ID {
		t.Fatalf("new ID displaced the retained session: %+v %v", result, err)
	}
}
