package store

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"duck-mem/internal/embed"
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

func TestSearchPlanUsesHNSW(t *testing.T) {
	db := openTemp(t)
	insertMsg(t, db, "s", 0, "zephyrturbine spins")
	vec := vectorLiteral(mustEmbed(t, "zephyrturbine"))
	var key, plan string
	err := db.sql.QueryRow(`EXPLAIN SELECT session_id FROM messages
		WHERE embedding IS NOT NULL AND contains(lower(project), lower(?))
		ORDER BY array_cosine_distance(embedding, ?::FLOAT[`+strconv.Itoa(embedDim())+`])
		LIMIT 5`, "p", vec).Scan(&key, &plan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "HNSW_INDEX_SCAN") {
		t.Fatalf("similarity search did not use the vss index:\n%s", plan)
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
		{Project: "p", A: "zephyrturbine", B: "read"}: 2,
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
	related, err := reader.Related("zephyrturbine", "p", 1, 5)
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

func edgeWeight(t *testing.T, db *DB, a, b string) int {
	t.Helper()
	var w int
	err := db.sql.QueryRow(`SELECT weight FROM topic_edges WHERE term_a=? AND term_b=?`, a, b).Scan(&w)
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
	if _, _, err := db.IndexNew(1); err != nil {
		t.Fatal(err)
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
	if err := db.SyncMessages(s.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.IndexNew(1); err != nil {
		t.Fatal(err)
	}
	hits, err = db.Related("bastion", "", 1, 10)
	if err != nil || len(hits) != 0 {
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

func TestTypedEdgesOutrankCoMention(t *testing.T) {
	db := openTemp(t)
	if err := db.ReplaceEdges(map[ProjectEdge]int{
		{Project: "p", A: "bastion", B: "noise"}:                    9,
		{Project: "p", A: "bastion", B: "sentry", Kind: "replaces"}: 1,
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := db.Related("bastion", "p", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Term != "sentry" || hits[0].Kind != "replaces" {
		t.Fatalf("typed edge should rank first: %+v", hits)
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
