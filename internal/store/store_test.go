package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmarceno/duck-mem/internal/embed"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSearchFindsBySimilarityFiltersAndDedupes(t *testing.T) {
	db := openTemp(t)
	sess := Session{ID: "s1", Source: "codex", Project: "/home/u/organizer", Path: "f", StartedAt: time.Now()}
	if err := db.UpsertSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSession(Session{ID: "s2", Source: "codex", Project: "/home/u/organizer", Path: "g"}); err != nil {
		t.Fatal(err)
	}
	msgs := []Message{
		{SessionID: "s1", Seq: 0, Source: "codex", Project: "/home/u/organizer", Role: "user", Text: "fix the redship bug"},
		{SessionID: "s1", Seq: 1, Source: "codex", Project: "/home/u/organizer", Role: "assistant", Text: "redship deployed"},
		{SessionID: "s2", Seq: 0, Source: "codex", Project: "/home/u/organizer", Role: "user", Text: "unrelated quartermaster ledger"},
	}
	if err := db.InsertMessages(msgs); err != nil {
		t.Fatal(err)
	}

	hits, err := db.Search("redship", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits want 2: %+v", len(hits), hits)
	}

	hits, err = db.Search("redship deployed", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Seq != 1 || hits[0].SessionID != "s1" {
		t.Fatalf("closest message should be the deployed one: %+v", hits)
	}
	for _, h := range hits {
		if h.SessionID == "s2" {
			t.Fatalf("unrelated message ranked as similar: %+v", hits)
		}
	}

	hits, err = db.Search("redship", "omen", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("project filter failed: %+v", hits)
	}

	hits, err = db.Search("redship", "ORGANIZER", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("project substring match failed: %+v", hits)
	}

	// Re-ingest must not duplicate.
	if err := db.InsertMessages(msgs); err != nil {
		t.Fatal(err)
	}
	_, n, err := db.Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("duplicate messages stored: n=%d", n)
	}
}

func TestSyncMessagesWhenEveryRowHasAnEmbedding(t *testing.T) {
	db := openTemp(t)
	if err := db.UpsertSession(Session{ID: "s", Source: "codex", Project: "p", Path: "f"}); err != nil {
		t.Fatal(err)
	}
	msgs := []Message{
		{SessionID: "s", Seq: 0, Source: "codex", Project: "p", Role: "user", Text: "alpha bravo"},
		{SessionID: "s", Seq: 1, Source: "codex", Project: "p", Role: "assistant", Text: "charlie delta"},
	}
	if err := db.SyncMessages("s", msgs); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSession(Session{ID: "empty", Source: "codex", Project: "p", Path: "g"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SyncMessages("empty", []Message{
		{SessionID: "empty", Seq: 0, Source: "codex", Project: "p", Role: "user", Text: "..."},
		{SessionID: "empty", Seq: 1, Source: "codex", Project: "p", Role: "assistant", Text: "echo foxtrot"},
	}); err != nil {
		t.Fatal(err)
	}
}

// Databases written by the vss version carry a persisted HNSW index whose
// checkpoints leaked blocks. Open must drop it without vss loaded, since a
// table with an unknown index type refuses writes.
func TestOpenDropsLegacyHNSWIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.duckdb")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	insertMsg(t, db, "s", 0, "zephyrturbine spins")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`LOAD vss`); err != nil {
		legacy.Close()
		t.Skipf("vss extension unavailable: %v", err)
	}
	if _, err := legacy.Exec(`SET hnsw_enable_experimental_persistence = true;
		CREATE INDEX idx_messages_hnsw ON messages USING HNSW (embedding) WITH (metric = 'cosine')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.sql.QueryRow(`SELECT count(*) FROM duckdb_indexes() WHERE index_name = 'idx_messages_hnsw'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("legacy index still present: %d %v", n, err)
	}
	insertMsg(t, db, "s", 1, "zephyrturbine stalls")
	hits, err := db.Search("zephyrturbine", "", "", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("search after migration: %v %v", hits, err)
	}
}

func TestSearchAndRelatedRejectInvalidLimits(t *testing.T) {
	db := openTemp(t)
	if _, err := db.Search("anything", "", "", 0); err == nil {
		t.Fatal("search accepted zero limit")
	}
	if _, err := db.Related("anything", "", 2, -1); err == nil {
		t.Fatal("related accepted negative limit")
	}
	if _, err := db.Related("anything", "", 3, 10); err == nil {
		t.Fatal("related accepted unsupported depth")
	}
}

func TestSearchSnippetShowsMatchInLongMessage(t *testing.T) {
	db := openTemp(t)
	insertMsg(t, db, "long", 0, strings.Repeat("padding ", 100)+"zephyrturbine conclusion")
	hits, err := db.Search("zephyrturbine", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Snippet, "zephyrturbine") {
		t.Fatalf("matching text missing from snippet: %+v", hits)
	}
}

func TestSearchRanksPhraseAndFocusedMessageFirst(t *testing.T) {
	db := openTemp(t)
	insertMsg(t, db, "a", 0, "bastion "+strings.Repeat("padding ", 80)+"sentry")
	insertMsg(t, db, "b", 0, "bastion sentry")
	insertMsg(t, db, "c", 0, "bastion sentry "+strings.Repeat("padding ", 80))
	hits, err := db.Search("bastion sentry", "", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 || hits[0].SessionID != "b" || hits[1].SessionID != "c" || hits[2].SessionID != "a" {
		t.Fatalf("relevance ranking wrong: %+v", hits)
	}
}

// A long message that uses other word forms than the query shares too
// few hashed features for the vector ranking; BM25 over stems finds it.
func TestSearchFindsWordFormsInLongMessage(t *testing.T) {
	db := openTemp(t)
	insertMsg(t, db, "report", 0, strings.Repeat("padding words here ", 60)+
		"then the enemies simply disengaged as if the player vanished "+strings.Repeat("more filler text ", 60))
	insertMsg(t, db, "other", 0, "quartermaster ledger totals")
	hits, err := db.Search("enemy disengage", "", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].SessionID != "report" || !strings.Contains(hits[0].Snippet, "enemies simply disengaged") {
		t.Fatalf("word forms in long message not found: %+v", hits)
	}
}

func TestRecallGroupsBySessionWithTitle(t *testing.T) {
	db := openTemp(t)
	add := func(sid string, texts ...string) {
		if err := db.UpsertSession(Session{ID: sid, Source: "claude", Project: "p", Path: sid + ".jsonl"}); err != nil {
			t.Fatal(err)
		}
		var msgs []Message
		for i, text := range texts {
			msgs = append(msgs, Message{SessionID: sid, Seq: i, Source: "claude", Project: "p", Role: "user", Text: text})
		}
		if err := db.InsertMessages(msgs); err != nil {
			t.Fatal(err)
		}
	}
	add("aaa-1", "<environment_context>cwd</environment_context>", "the barricade blocks enemy sight",
		"barricade sight plan", "barricade sight plan", "unrelated")
	add("aaa-2", "barricade")
	results, _, err := db.Recall("barricade sight", "", "", 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Session.ID != "aaa-1" {
		t.Fatalf("sessions not grouped best first: %+v", results)
	}
	first := results[0]
	if first.Title != "the barricade blocks enemy sight" || first.Messages != 5 || first.Session.Path != "aaa-1.jsonl" {
		t.Fatalf("session description wrong: %+v", first)
	}
	if len(first.Hits) != 2 || first.Hits[0].Snippet == first.Hits[1].Snippet {
		t.Fatalf("hits not capped and deduplicated: %+v", first.Hits)
	}

	if _, err := db.ResolveSession("aaa"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous prefix resolved: %v", err)
	}
	s, err := db.ResolveSession("aaa-1")
	if err != nil || s.Messages != 5 {
		t.Fatalf("resolve: %+v err=%v", s, err)
	}
	window, err := db.Window(s.Session.ID, 1, 2)
	if err != nil || len(window) != 2 || window[0].Seq != 1 || window[1].Seq != 2 {
		t.Fatalf("window: %+v err=%v", window, err)
	}
}

// Databases from before keyword search get their posting list on open.
func TestOpenBackfillsKeywordIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.duckdb")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	insertMsg(t, db, "old", 0, "zephyrturbine spins")
	for _, q := range []string{`DELETE FROM msg_terms`, `DELETE FROM meta WHERE key='terms_schema'`} {
		if _, err := db.sql.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.sql.QueryRow(`SELECT count(*) FROM msg_terms WHERE term = 'zephyrturbin'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("posting list not backfilled: n=%d err=%v", n, err)
	}
}

func TestReadOnlyOpenReadsAndRefusesMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.duckdb")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.UpsertSession(Session{ID: "s1", Source: "codex", Project: "p", Path: "f"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.InsertMessages([]Message{
		{SessionID: "s1", Seq: 0, Source: "codex", Project: "p", Role: "user", Text: "readonly zephyrturbine read"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.ReplaceEdges(map[ProjectEdge]int{
		{Project: "p", A: "zephyr", B: "read"}: 2,
	}); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	// Cross-process reads-while-daemon-writes are covered by live E2E
	// (same-process second opens share one DuckDB instance); here the
	// read-only DSN must connect and see committed rows.
	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	hits, err := reader.Search("zephyrturbine", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits want 1", len(hits))
	}
	related, err := reader.Related("zephyr", "p", 1, 5)
	if err != nil || len(related) != 1 || related[0].Term != "read" {
		t.Fatalf("read-only graph traversal: %+v err=%v", related, err)
	}
	if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "nope.duckdb")); err == nil {
		t.Fatal("expected error for missing database")
	}
}

func TestRelatedRanksDirectAndTwoHop(t *testing.T) {
	db := openTemp(t)
	if err := db.ReplaceEdges(map[ProjectEdge]int{
		{Project: "/home/u/omen", A: "bastion", B: "sentry"}:   5,
		{Project: "/home/u/omen", A: "bastion", B: "aegis"}:    2,
		{Project: "/home/u/omen", A: "sanctuary", B: "sentry"}: 3,
		{Project: "/home/u/other", A: "bastion", B: "hubspot"}: 9,
	}); err != nil {
		t.Fatal(err)
	}

	hits, err := db.Related("Bastion", "omen", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Term != "sentry" || hits[0].Weight != 5 || hits[0].Via != "" {
		t.Fatalf("depth1 wrong: %+v", hits)
	}

	hits, err = db.Related("bastion", "omen", 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.Term == "hubspot" {
			t.Fatalf("other-project edge leaked: %+v", hits)
		}
		if h.Term == "sanctuary" && h.Via == "sentry" && h.Weight == 8 {
			found = true
		}
	}
	if !found {
		t.Fatalf("two-hop sanctuary via sentry missing: %+v", hits)
	}
}

func TestRelatedAggregatesAcrossProjects(t *testing.T) {
	db := openTemp(t)
	if err := db.ReplaceEdges(map[ProjectEdge]int{
		{Project: "one", A: "bastion", B: "sentry"}: 2,
		{Project: "two", A: "bastion", B: "sentry"}: 3,
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := db.Related("bastion", "", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Term != "sentry" || hits[0].Weight != 5 {
		t.Fatalf("expected aggregated weight 5, got %+v", hits)
	}
}

func embedDim() int { return embed.Dim }

func mustEmbed(t *testing.T, text string) []float32 {
	t.Helper()
	v := embed.Embed(text)
	if v == nil {
		t.Fatalf("no embedding for %q", text)
	}
	return v
}

func insertMsg(t *testing.T, db *DB, sid string, seq int, text string) {
	t.Helper()
	if err := db.UpsertSession(Session{ID: sid, Source: "codex", Project: "p", Path: "f"}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertMessages([]Message{{
		SessionID: sid, Seq: seq, Source: "codex", Project: "p", Role: "user", Text: text,
	}}); err != nil {
		t.Fatal(err)
	}
}

// edgeWeight reads the co-mention weight between two words' graph nodes.
func edgeWeight(t *testing.T, db *DB, a, b string) int {
	t.Helper()
	stems, err := db.stemWords([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	a, b = stems[a], stems[b]
	if a > b {
		a, b = b, a
	}
	var w int
	err = db.sql.QueryRow(`SELECT weight FROM topic_edges WHERE term_a=? AND term_b=? AND kind='co-mention' LIMIT 1`, a, b).Scan(&w)
	if err != nil {
		return -1
	}
	return w
}

func TestIndexNewIsIncrementalAndIdempotent(t *testing.T) {
	db := openTemp(t)
	insertMsg(t, db, "s1", 0, "bastion deploys sentry turret")
	insertMsg(t, db, "s1", 1, "bastion sentry fires")

	n, pairs, err := db.IndexNew(1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || pairs == 0 {
		t.Fatalf("first index: msgs=%d pairs=%d", n, pairs)
	}
	if w := edgeWeight(t, db, "bastion", "sentry"); w != 2 {
		t.Fatalf("bastion-sentry weight = %d, want 2", w)
	}

	n, _, err = db.IndexNew(1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("re-index should find nothing new, got %d", n)
	}
	if w := edgeWeight(t, db, "bastion", "sentry"); w != 2 {
		t.Fatalf("re-index changed weight to %d", w)
	}

	insertMsg(t, db, "s2", 0, "bastion sentry holds")
	n, _, err = db.IndexNew(1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 new message, got %d", n)
	}
	if w := edgeWeight(t, db, "bastion", "sentry"); w != 3 {
		t.Fatalf("bastion-sentry weight = %d, want 3", w)
	}
}

func TestSyncMessagesRefreshesEditedSessionAndGraph(t *testing.T) {
	db := openTemp(t)
	s := Session{ID: "edited", Source: "codex", Project: "old", Path: "f"}
	if err := db.UpsertSession(s); err != nil {
		t.Fatal(err)
	}
	message := func(seq int, text, project string) Message {
		return Message{SessionID: s.ID, Seq: seq, Source: s.Source, Project: project, Role: "user", Text: text}
	}
	if err := db.UpsertSession(Session{ID: "other", Source: "codex", Project: "keep", Path: "g"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SyncMessages("other", []Message{{SessionID: "other", Source: "codex", Project: "keep", Role: "user", Text: "bastion aegis"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SyncMessages(s.ID, []Message{message(0, "bastion sentry", "old")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.IndexNew(1); err != nil {
		t.Fatal(err)
	}
	if err := db.SyncMessages(s.ID, []Message{message(0, "bastion sentry", "old"), message(1, "bastion sentry", "old")}); err != nil {
		t.Fatal(err)
	}
	if n, _, err := db.IndexNew(1); err != nil || n != 1 || edgeWeight(t, db, "bastion", "sentry") != 2 {
		t.Fatalf("append was not indexed incrementally: n=%d err=%v", n, err)
	}
	s.Project = "new"
	if err := db.UpsertSession(s); err != nil {
		t.Fatal(err)
	}
	if err := db.SyncMessages(s.ID, []Message{message(0, "bastion turret", "new"), message(1, "bastion turret", "new")}); err != nil {
		t.Fatal(err)
	}
	// The rewrite is subtracted in place: no full rebuild is queued.
	var dirty int
	if err := db.sql.QueryRow(`SELECT count(*) FROM meta WHERE key='graph_dirty'`).Scan(&dirty); err != nil || dirty != 0 {
		t.Fatalf("rewrite queued a full rebuild: dirty=%d err=%v", dirty, err)
	}
	if n, _, err := db.IndexNew(1); err != nil || n != 2 {
		t.Fatalf("rewritten rows not indexed incrementally: n=%d err=%v", n, err)
	}
	old, err := db.Search("sentry", "", "", 10)
	if err != nil || len(old) != 0 {
		t.Fatalf("stale message remained: hits=%+v err=%v", old, err)
	}
	hits, err := db.Related("bastion", "new", 1, 10)
	if err != nil || len(hits) != 1 || hits[0].Term != "turret" || hits[0].Weight != 2 {
		t.Fatalf("updated graph wrong: hits=%+v err=%v", hits, err)
	}
	hits, err = db.Related("bastion", "old", 1, 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("stale graph remained: hits=%+v err=%v", hits, err)
	}
	hits, err = db.Related("bastion", "keep", 1, 10)
	if err != nil || len(hits) != 1 || hits[0].Term != "aegis" || hits[0].Weight != 1 {
		t.Fatalf("other session's graph changed: hits=%+v err=%v", hits, err)
	}
	if err := db.SyncMessages(s.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.IndexNew(1); err != nil {
		t.Fatal(err)
	}
	hits, err = db.Related("bastion", "", 1, 10)
	if err != nil || len(hits) != 1 || hits[0].Term != "aegis" {
		t.Fatalf("deleted session still indexed: hits=%+v err=%v", hits, err)
	}
}

func TestIndexFullHealsThresholdDrift(t *testing.T) {
	db := openTemp(t)
	insertMsg(t, db, "s1", 0, "bastion sentry turret")
	if _, _, err := db.IndexNew(5); err != nil {
		t.Fatal(err)
	}
	if w := edgeWeight(t, db, "bastion", "sentry"); w != -1 {
		t.Fatalf("rare pair should be absent, weight = %d", w)
	}
	if err := db.IndexFull(1); err != nil {
		t.Fatal(err)
	}
	if w := edgeWeight(t, db, "bastion", "sentry"); w != 1 {
		t.Fatalf("full rebuild should materialize pair, weight = %d", w)
	}
}

// A typed relation seen twice outranks co-mentions; a one-off, often a
// misparse like "barricade's actual shape", ranks by weight like them.
// Word forms share one graph node, shown by its most common surface word.
func TestGraphMergesWordFormsUnderOneLabel(t *testing.T) {
	db := openTemp(t)
	insertMsg(t, db, "a", 0, "enemies roam")
	insertMsg(t, db, "b", 0, "the enemy roams")
	insertMsg(t, db, "c", 0, "enemies roam")
	if _, _, err := db.IndexNew(1); err != nil {
		t.Fatal(err)
	}
	hits, err := db.Related("enemy", "p", 1, 10)
	if err != nil || len(hits) != 1 || hits[0].Term != "roam" || hits[0].Weight != 3 {
		t.Fatalf("word forms not merged: %+v err=%v", hits, err)
	}
}

// Graph neighbours only fill slots the query's own matches leave open,
// after them, and marked.
func TestRecallFillsFromGraphNeighboursAfterDirectMatches(t *testing.T) {
	db := openTemp(t)
	insertMsg(t, db, "direct", 0, "enemies disengage and roam")
	insertMsg(t, db, "neighbour", 0, "patrols roam the campaign")
	insertMsg(t, db, "unrelated", 0, "quartermaster ledger totals")
	if _, _, err := db.IndexNew(1); err != nil {
		t.Fatal(err)
	}
	results, related, err := db.Recall("disengage", "", "", 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Session.ID != "direct" || results[0].ViaGraph ||
		results[1].Session.ID != "neighbour" || !results[1].ViaGraph {
		t.Fatalf("graph fill wrong: %+v", results)
	}
	if !slices.ContainsFunc(related, func(t Topic) bool { return t.Label == "roam" }) {
		t.Fatalf("neighbour not reported: %+v", related)
	}
	if full, _, err := db.Recall("disengage", "", "", 1, 1); err != nil || len(full) != 1 || full[0].ViaGraph {
		t.Fatalf("graph filled a slot a direct match needed: %+v err=%v", full, err)
	}
}

func TestRecurringTypedEdgesOutrankCoMention(t *testing.T) {
	db := openTemp(t)
	if err := db.ReplaceEdges(map[ProjectEdge]int{
		{Project: "p", A: "bastion", B: "noise"}:                    9,
		{Project: "p", A: "bastion", B: "sentry", Kind: "replaces"}: 2,
		{Project: "p", A: "actual", B: "bastion", Kind: "owns"}:     1,
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := db.Related("bastion", "p", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 || hits[0].Term != "sentry" || hits[1].Term != "noise" || hits[2].Term != "actual" {
		t.Fatalf("typed ranking wrong: %+v", hits)
	}
}

func TestRelatedKeepsDirectionAndSourceEvidence(t *testing.T) {
	db := openTemp(t)
	insertMsg(t, db, "forward", 0, "Bastion replaces Sentry.")
	insertMsg(t, db, "reverse", 0, "Sentry replaces Bastion.")
	if _, _, err := db.IndexNew(1); err != nil {
		t.Fatal(err)
	}
	hits, err := db.Related("bastion", "p", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, h := range hits {
		if h.Kind != "replaces" {
			continue
		}
		if h.Term != "sentry" || !strings.Contains(h.Evidence, "#0: ") {
			t.Fatalf("typed edge lacks source evidence: %+v", h)
		}
		seen[h.From+"->"+h.To] = true
	}
	if !seen["bastion->sentry"] || !seen["sentry->bastion"] {
		t.Fatalf("opposite directions collapsed: %+v", hits)
	}
}

func TestIndexMigratesOldGraphToDirectedEvidence(t *testing.T) {
	db := openTemp(t)
	insertMsg(t, db, "legacy", 0, "Bastion replaces Sentry.")
	for _, q := range []string{
		`DROP PROPERTY GRAPH IF EXISTS topic_graph`,
		`DROP TABLE topic_edges`,
		`CREATE TABLE topic_edges(project TEXT, term_a TEXT, term_b TEXT, weight INTEGER, kind TEXT,
		 PRIMARY KEY(project, term_a, term_b, kind))`,
		`INSERT INTO meta(key, value) VALUES('edges_schema', '2') ON CONFLICT(key) DO UPDATE SET value='2'`,
		`UPDATE messages SET indexed=1`,
	} {
		if _, err := db.sql.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if n, _, err := db.IndexNew(1); err != nil || n != 1 {
		t.Fatalf("legacy graph was not reindexed: n=%d err=%v", n, err)
	}
	hits, err := db.Related("bastion", "p", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.Kind == "replaces" && h.From == "bastion" && h.To == "sentry" && strings.Contains(h.Evidence, "legacy#0:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("migration lost direction or evidence: %+v", hits)
	}
}

func TestDepth2ExpandsStrongestViasOnly(t *testing.T) {
	db := openTemp(t)
	edges := map[ProjectEdge]int{}
	for i := 1; i <= 9; i++ {
		edges[ProjectEdge{Project: "p", A: "hub", B: fmt.Sprintf("n%d", i)}] = 10 - i
	}
	edges[ProjectEdge{Project: "p", A: "n9", B: "far"}] = 1
	if err := db.ReplaceEdges(edges); err != nil {
		t.Fatal(err)
	}
	hits, err := db.Related("hub", "p", 2, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Term == "far" {
			t.Fatalf("weak-via two-hop leaked (maxVia=%d): %+v", maxVia, hits)
		}
	}
}

func TestOpenRewritesOldStorageWithZstd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.duckdb")
	old, err := sql.Open("duckdb", path+"?storage_compatibility_version=v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE sessions(session_id TEXT PRIMARY KEY, source TEXT NOT NULL, project TEXT NOT NULL DEFAULT '', started_at TIMESTAMPTZ, path TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE messages(session_id TEXT NOT NULL REFERENCES sessions(session_id), seq INTEGER NOT NULL, source TEXT NOT NULL, project TEXT NOT NULL DEFAULT '', role TEXT NOT NULL, text TEXT NOT NULL, created_at TIMESTAMPTZ, PRIMARY KEY(session_id, seq))`,
		`INSERT INTO sessions VALUES('s1', 'codex', 'p', NULL, 'f')`,
		`INSERT INTO messages SELECT 's1', i, 'codex', 'p', 'user', 'message body ' || i || repeat(' filler', 50), NULL FROM range(5000) t(i)`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.sql.QueryRow(`SELECT count(*) FROM messages`).Scan(&n); err != nil || n != 5000 {
		t.Fatalf("rows after upgrade: %d %v", n, err)
	}
	var compressions []string
	rows, err := db.sql.Query(`SELECT DISTINCT compression FROM pragma_storage_info('messages') WHERE column_name='text' AND segment_type='VARCHAR'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		compressions = append(compressions, c)
	}
	rows.Close()
	if !slices.Equal(compressions, []string{"ZSTD"}) {
		t.Fatalf("text column compression %v, want only ZSTD", compressions)
	}
}
