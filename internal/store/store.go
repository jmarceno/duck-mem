package store

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"duck-mem/internal/topics"

	_ "github.com/marcboeker/go-duckdb/v2"
)

// Message is one kept utterance from a session. Tool calls and tool
// outputs are never stored (spec: dump everything minus tool calls).
type Message struct {
	SessionID string
	Seq       int // order within the session, assigned at ingest
	Source    string
	Project   string
	Role      string // user | assistant | system | note
	Text      string
	CreatedAt time.Time // zero when the source format carries no timestamp
}

// Session groups messages. Project is a plain TEXT value, not a column:
// DuckDB columns are schema, so one column per project would need DDL per
// project and break every query. A project column value filters with a
// WHERE clause and an index instead.
type Session struct {
	ID        string
	Source    string
	Project   string
	Path      string
	StartedAt time.Time
}

// Hit is one search result.
type Hit struct {
	SessionID string
	Seq       int
	Source    string
	Project   string
	Role      string
	Snippet   string
}

// DB wraps the DuckDB handle.
type DB struct{ sql *sql.DB }

// Open creates/opens the DuckDB file and initializes the schema.
// Already installed extensions load best-effort. Opening a database must not
// download extensions: neither is needed by the current search or index.
func Open(path string) (*DB, error) {
	sdb, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, err
	}
	db := &DB{sql: sdb}
	if err := db.init(); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return db, nil
}

// OpenReadOnly opens the database without taking the write lock, so
// query/related work while the daemon holds the file. Skips schema init:
// a missing database is an error, not an empty store. This also enforces
// the agents-read-only contract at the API level.
func OpenReadOnly(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("database not found: %s (run ingest or daemon first)", path)
	}
	sdb, err := sql.Open("duckdb", path+"?access_mode=READ_ONLY")
	if err != nil {
		return nil, err
	}
	// sql.Open is lazy. Force the connection now so callers can retry a
	// transient writer lock instead of failing on their first query.
	if err := sdb.Ping(); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return &DB{sql: sdb}, nil
}

func (db *DB) init() error {
	for _, q := range []string{
		`LOAD vss`,
		`LOAD duckpgq`,
	} {
		_, _ = db.sql.Exec(q)
	}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS sessions(
			session_id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			project TEXT NOT NULL DEFAULT '',
			started_at TIMESTAMPTZ,
			path TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS messages(
			session_id TEXT NOT NULL REFERENCES sessions(session_id),
			seq INTEGER NOT NULL,
			source TEXT NOT NULL,
			project TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL,
			text TEXT NOT NULL,
			created_at TIMESTAMPTZ,
			PRIMARY KEY(session_id, seq))`,
		`CREATE INDEX IF NOT EXISTS idx_messages_project ON messages(project)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_source ON messages(source)`,
		`CREATE TABLE IF NOT EXISTS topic_edges(
			project TEXT NOT NULL DEFAULT '',
			term_a TEXT NOT NULL,
			term_b TEXT NOT NULL,
			weight INTEGER NOT NULL,
			kind TEXT NOT NULL DEFAULT 'co-mention',
			PRIMARY KEY(project, term_a, term_b, kind))`,
		// term_df holds per-project document frequencies so incremental
		// runs make the same keep/drop decisions as a full rebuild.
		`CREATE TABLE IF NOT EXISTS term_df(
			project TEXT NOT NULL,
			term TEXT NOT NULL,
			msgs INTEGER NOT NULL,
			PRIMARY KEY(project, term))`,
		`CREATE TABLE IF NOT EXISTS meta(
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL)`,
	} {
		if _, err := db.sql.Exec(q); err != nil {
			return err
		}
	}
	// Non-destructive migrations for databases created earlier.
	// (DuckDB cannot ADD COLUMN with constraints, so add nullable and
	// backfill.) Rows predating the indexed flag were indexed under the
	// old full rebuild, so mark them: the next full rebuild heals any
	// mismatch.
	for _, q := range []string{
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS indexed INTEGER`,
		`ALTER TABLE topic_edges ADD COLUMN IF NOT EXISTS kind TEXT`,
		`UPDATE messages SET indexed=1 WHERE indexed IS NULL`,
		`UPDATE topic_edges SET kind='co-mention' WHERE kind IS NULL`,
	} {
		if _, err := db.sql.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// Close releases the handle.
func (db *DB) Close() error { return db.sql.Close() }

// UpsertSession records a session; re-ingests refresh its metadata.
func (db *DB) UpsertSession(s Session) error {
	_, err := db.sql.Exec(
		`INSERT INTO sessions(session_id, source, project, started_at, path)
		 VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET source=excluded.source, project=excluded.project,
		 path=excluded.path, started_at=excluded.started_at`,
		s.ID, s.Source, s.Project, nullableTime(s.StartedAt), s.Path)
	return err
}

// SyncMessages replaces a session's stored snapshot only when its content
// changed. Reparsed sessions can gain, lose, or reorder messages; indexed
// edges from the old snapshot then require a full rebuild.
func (db *DB) SyncMessages(sessionID string, msgs []Message) error {
	rows, err := db.sql.Query(`SELECT seq, source, project, role, text FROM messages WHERE session_id=? ORDER BY seq`, sessionID)
	if err != nil {
		return err
	}
	type stored struct {
		seq                         int
		source, project, role, text string
	}
	var old []stored
	for rows.Next() {
		var m stored
		if err := rows.Scan(&m.seq, &m.source, &m.project, &m.role, &m.text); err != nil {
			rows.Close()
			return err
		}
		old = append(old, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	prefixMatches := len(old) <= len(msgs)
	if prefixMatches {
		for i := range old {
			m := msgs[i]
			if m.SessionID != sessionID || old[i] != (stored{m.Seq, m.Source, m.Project, m.Role, m.Text}) {
				prefixMatches = false
				break
			}
		}
		if prefixMatches && len(old) == len(msgs) {
			return nil
		}
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !prefixMatches {
		if _, err := tx.Exec(`DELETE FROM messages WHERE session_id=?`, sessionID); err != nil {
			return err
		}
	}
	start := 0
	if prefixMatches {
		start = len(old)
	}
	for _, m := range msgs[start:] {
		if m.SessionID != sessionID {
			return fmt.Errorf("message session %q does not match %q", m.SessionID, sessionID)
		}
		if _, err := tx.Exec(`INSERT INTO messages(session_id, seq, source, project, role, text, created_at, indexed)
			VALUES(?, ?, ?, ?, ?, ?, ?, 0)`, m.SessionID, m.Seq, m.Source, m.Project, m.Role, m.Text, nullableTime(m.CreatedAt)); err != nil {
			return err
		}
	}
	if len(old) > 0 && !prefixMatches {
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES('graph_dirty', '1')
			ON CONFLICT(key) DO UPDATE SET value='1'`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// InsertMessages stores messages; re-ingesting the same (session_id, seq)
// is a no-op so ingest runs are idempotent. New rows enter unindexed
// (indexed=0) for the next incremental IndexNew run.
func (db *DB) InsertMessages(msgs []Message) error {
	for _, m := range msgs {
		if _, err := db.sql.Exec(
			`INSERT INTO messages(session_id, seq, source, project, role, text, created_at, indexed)
			 VALUES(?, ?, ?, ?, ?, ?, ?, 0)
			 ON CONFLICT(session_id, seq) DO NOTHING`,
			m.SessionID, m.Seq, m.Source, m.Project, m.Role, m.Text, nullableTime(m.CreatedAt)); err != nil {
			return err
		}
	}
	return nil
}

// Search AND-matches whitespace-separated tokens case-insensitively
// against message text. Empty project/source means no filter.
func (db *DB) Search(query, project, source string, limit int) ([]Hit, error) {
	if limit < 1 {
		return nil, fmt.Errorf("limit must be positive")
	}
	toks := strings.Fields(query)
	var sb strings.Builder
	args := []any{}
	sb.WriteString(`SELECT session_id, seq, source, project, role, `)
	if len(toks) > 0 {
		// Anchor the snippet at a matched token, not the beginning of a
		// potentially multi-megabyte message.
		sb.WriteString(`substr(text, greatest(1, strpos(lower(text), lower(?))-80), 300)`)
		args = append(args, toks[0])
	} else {
		sb.WriteString(`substr(text, 1, 300)`)
	}
	sb.WriteString(` FROM messages WHERE 1=1`)
	for _, t := range toks {
		sb.WriteString(` AND text ILIKE '%'||?||'%' ESCAPE '\'`)
		args = append(args, escapeLike(t))
	}
	if project != "" {
		sb.WriteString(` AND project ILIKE '%'||?||'%' ESCAPE '\'`)
		args = append(args, escapeLike(project))
	}
	if source != "" {
		sb.WriteString(` AND source = ?`)
		args = append(args, source)
	}
	sb.WriteString(` ORDER BY session_id, seq LIMIT ?`)
	args = append(args, limit)
	rows, err := db.sql.Query(sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.SessionID, &h.Seq, &h.Source, &h.Project, &h.Role, &h.Snippet); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// TextDoc is one message text plus its project, for per-project indexing.
type TextDoc struct {
	Project string
	Text    string
}

// ProjectEdge is one weighted co-mention within a project.
type ProjectEdge struct {
	Project string
	A, B    string
	// Kind names the relation type. Empty means 'co-mention'; typed
	// relations (owns, replaces, fixes) are future extraction work.
	Kind string
}

func (e ProjectEdge) kind() string {
	if e.Kind == "" {
		return "co-mention"
	}
	return e.Kind
}

// AllTexts returns every message text, for (re)building the topic graph.
func (db *DB) AllTexts() ([]TextDoc, error) {
	rows, err := db.sql.Query(`SELECT project, text FROM messages`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TextDoc
	for rows.Next() {
		var d TextDoc
		if err := rows.Scan(&d.Project, &d.Text); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ReplaceEdges rebuilds the co-occurrence graph wholesale.
func (db *DB) ReplaceEdges(edges map[ProjectEdge]int) error {
	if err := db.ensureEdgesSchema(); err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM topic_edges`); err != nil {
		return err
	}
	for e, w := range edges {
		if _, err := tx.Exec(`INSERT INTO topic_edges(project, term_a, term_b, weight, kind) VALUES(?, ?, ?, ?, ?)`,
			e.Project, e.A, e.B, w, e.kind()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// edgesSchemaVersion tracks the topic_edges layout. v2: kind is part of
// the primary key so one pair can hold co-mention and typed edges side
// by side.
const edgesSchemaVersion = "2"

// ensureEdgesSchema recreates outdated topic_edges tables (which carry no
// state worth migrating: a rebuild restores them).
func (db *DB) ensureEdgesSchema() error {
	var v string
	_ = db.sql.QueryRow(`SELECT value FROM meta WHERE key='edges_schema'`).Scan(&v)
	if v == edgesSchemaVersion {
		return nil
	}
	for _, q := range []string{
		`DROP TABLE IF EXISTS topic_edges`,
		`CREATE TABLE topic_edges(
			project TEXT NOT NULL DEFAULT '',
			term_a TEXT NOT NULL,
			term_b TEXT NOT NULL,
			weight INTEGER NOT NULL,
			kind TEXT NOT NULL DEFAULT 'co-mention',
			PRIMARY KEY(project, term_a, term_b, kind))`,
		`INSERT INTO meta(key, value) VALUES('edges_schema', '2')
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		// The graph was just dropped: every message needs indexing again.
		`UPDATE messages SET indexed=0`,
		`DELETE FROM term_df`,
	} {
		if _, err := db.sql.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// IndexNew indexes only messages not yet indexed and advances the graph
// incrementally: pair weights grow by co-mention delta, term_df tracks
// document frequencies for threshold decisions. Runs are idempotent:
// already-indexed messages are skipped, so re-running changes nothing.
//
// Approximation (healed by IndexFull): keep/drop decisions use df counts
// as of this run, so a term crossing minDF later undercounts its early
// pairs, and a new hub keeps its early edges.
func (db *DB) IndexNew(minDF int) (newMsgs, newPairs int, err error) {
	if err := db.ensureEdgesSchema(); err != nil {
		return 0, 0, err
	}
	var dirty string
	if err := db.sql.QueryRow(`SELECT value FROM meta WHERE key='graph_dirty'`).Scan(&dirty); err != nil && err != sql.ErrNoRows {
		return 0, 0, err
	}
	if dirty == "1" {
		if err := db.sql.QueryRow(`SELECT count(*) FROM messages WHERE indexed=0`).Scan(&newMsgs); err != nil {
			return 0, 0, err
		}
		if err := db.IndexFull(minDF); err != nil {
			return 0, 0, err
		}
		return newMsgs, 0, nil
	}
	df, totals, err := db.loadDF()
	if err != nil {
		return 0, 0, err
	}
	rows, err := db.sql.Query(`SELECT session_id, seq, project, text FROM messages WHERE indexed=0`)
	if err != nil {
		return 0, 0, err
	}
	type doc struct {
		sid, proj, text string
		seq             int
	}
	var docs []doc
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.sid, &d.seq, &d.proj, &d.text); err != nil {
			rows.Close()
			return 0, 0, err
		}
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(docs) == 0 {
		return 0, 0, nil
	}
	// Per-project df views over the shared flat map.
	projDF := map[string]map[string]int{}
	getDF := func(proj string) map[string]int {
		m, ok := projDF[proj]
		if !ok {
			m = dfView(df, proj)
			projDF[proj] = m
		}
		return m
	}
	// First pass: fold the new messages into df so pair decisions use
	// batch-final counts, matching a full rebuild over the same corpus
	// (modulo pre-existing drift). Totals already include the new rows:
	// they were inserted before indexing.
	dfDelta := map[[2]string]int{}
	for _, d := range docs {
		seen := map[string]bool{}
		for _, w := range topics.Terms(d.text) {
			if !seen[w] {
				seen[w] = true
				dfDelta[[2]string{d.proj, w}]++
				getDF(d.proj)[w]++
			}
		}
	}
	pairDelta := map[ProjectEdge]int{}
	for _, d := range docs {
		for _, e := range topics.SelectPairs(d.text, getDF(d.proj), minDF, topics.HubCap(totals[d.proj], minDF)) {
			pairDelta[ProjectEdge{Project: d.proj, A: e[0], B: e[1]}]++
		}
		for _, t := range topics.ExtractTyped(d.text) {
			pairDelta[ProjectEdge{Project: d.proj, A: t.A, B: t.B, Kind: t.Kind}]++
		}
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	for e, n := range pairDelta {
		if _, err := tx.Exec(
			`INSERT INTO topic_edges(project, term_a, term_b, weight, kind) VALUES(?, ?, ?, ?, ?)
			 ON CONFLICT(project, term_a, term_b, kind) DO UPDATE SET weight=topic_edges.weight+excluded.weight`,
			e.Project, e.A, e.B, n, e.kind()); err != nil {
			return 0, 0, err
		}
	}
	for pt, n := range dfDelta {
		if _, err := tx.Exec(
			`INSERT INTO term_df(project, term, msgs) VALUES(?, ?, ?)
			 ON CONFLICT(project, term) DO UPDATE SET msgs=term_df.msgs+excluded.msgs`,
			pt[0], pt[1], n); err != nil {
			return 0, 0, err
		}
	}
	for _, d := range docs {
		if _, err := tx.Exec(`UPDATE messages SET indexed=1 WHERE session_id=? AND seq=?`, d.sid, d.seq); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return len(docs), len(pairDelta), nil
}

// IndexFull rebuilds the graph from scratch: exact thresholds, healed
// drift. Run on a schedule (e.g. weekly cron); IndexNew covers the days.
func (db *DB) IndexFull(minDF int) error {
	if err := db.ensureEdgesSchema(); err != nil {
		return err
	}
	docs, err := db.AllTexts()
	if err != nil {
		return err
	}
	groups := map[string][]string{}
	for _, d := range docs {
		groups[d.Project] = append(groups[d.Project], d.Text)
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM topic_edges`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM term_df`); err != nil {
		return err
	}
	for proj, texts := range groups {
		df := topics.DocFreq(texts)
		maxDF := topics.HubCap(len(texts), minDF)
		edgeW := map[ProjectEdge]int{}
		for _, t := range texts {
			for _, e := range topics.SelectPairs(t, df, minDF, maxDF) {
				edgeW[ProjectEdge{Project: proj, A: e[0], B: e[1]}]++
			}
			for _, tp := range topics.ExtractTyped(t) {
				edgeW[ProjectEdge{Project: proj, A: tp.A, B: tp.B, Kind: tp.Kind}]++
			}
		}
		for e, w := range edgeW {
			if _, err := tx.Exec(
				`INSERT INTO topic_edges(project, term_a, term_b, weight, kind) VALUES(?, ?, ?, ?, ?)`,
				proj, e.A, e.B, w, e.kind()); err != nil {
				return err
			}
		}
		for w, n := range df {
			if _, err := tx.Exec(`INSERT INTO term_df(project, term, msgs) VALUES(?, ?, ?)`, proj, w, n); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`UPDATE messages SET indexed=1`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM meta WHERE key='graph_dirty'`); err != nil {
		return err
	}
	return tx.Commit()
}

// loadDF returns term document frequencies keyed project+"\x00"+term,
// plus per-project message totals.
func (db *DB) loadDF() (map[string]int, map[string]int, error) {
	df := map[string]int{}
	rows, err := db.sql.Query(`SELECT project, term, msgs FROM term_df`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var p, t string
		var n int
		if err := rows.Scan(&p, &t, &n); err != nil {
			rows.Close()
			return nil, nil, err
		}
		df[p+"\x00"+t] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	totals := map[string]int{}
	rows, err = db.sql.Query(`SELECT project, count(*) FROM messages GROUP BY project`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			return nil, nil, err
		}
		totals[p] = n
	}
	return df, totals, rows.Err()
}

// dfView projects the flat df map onto one project's term counts.
func dfView(df map[string]int, proj string) map[string]int {
	out := map[string]int{}
	for k, n := range df {
		if len(k) > len(proj)+1 && k[:len(proj)] == proj && k[len(proj)] == 0 {
			out[k[len(proj)+1:]] = n
		}
	}
	return out
}

// RelatedHit is one related topic. Via is empty at depth 1; at depth 2 it
// names the intermediate topic (term -via-> hit). Kind is the relation
// type ('co-mention' unless a rule extracted better).
type RelatedHit struct {
	Term   string
	Weight int
	Via    string
	Kind   string
}

// Related returns topics co-mentioned with term. Empty project searches
// all projects; otherwise edges are filtered by project substring match.
// Depth 1 ranks direct neighbors by co-mention count; depth 2 adds
// neighbors-of-neighbors, scored by the two-hop weight sum.
func (db *DB) Related(term, project string, depth, limit int) ([]RelatedHit, error) {
	if limit < 1 || (depth != 1 && depth != 2) {
		return nil, fmt.Errorf("related needs a positive limit and depth 1 or 2")
	}
	term = strings.ToLower(term)
	neighbors := func(t string) ([]RelatedHit, error) {
		q := `SELECT CASE WHEN term_a=? THEN term_b ELSE term_a END AS other, sum(weight), kind
			 FROM topic_edges WHERE (term_a=? OR term_b=?)`
		args := []any{t, t, t}
		if project != "" {
			q += ` AND project ILIKE '%'||?||'%' ESCAPE '\'`
			args = append(args, escapeLike(project))
		}
		q += ` GROUP BY other, kind`
		rows, err := db.sql.Query(q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []RelatedHit
		for rows.Next() {
			var h RelatedHit
			if err := rows.Scan(&h.Term, &h.Weight, &h.Kind); err != nil {
				return nil, err
			}
			out = append(out, h)
		}
		return out, rows.Err()
	}
	seen := map[string]RelatedHit{}
	direct := map[string]int{}
	first, err := neighbors(term)
	if err != nil {
		return nil, err
	}
	for _, h := range first {
		seen[h.Term+"\x00"+h.Kind] = h
		if w, ok := direct[h.Term]; !ok || h.Weight > w {
			direct[h.Term] = h.Weight
		}
	}
	if depth >= 2 {
		// Expand only the strongest direct neighbors: expanding every
		// neighbor lets glue verbs (check, want, see) flood two-hop results.
		ordered := make([]RelatedHit, 0, len(direct))
		for t, w := range direct {
			ordered = append(ordered, RelatedHit{Term: t, Weight: w})
		}
		sortHits(ordered)
		if len(ordered) > maxVia {
			ordered = ordered[:maxVia]
		}
		for _, v := range ordered {
			via, w1 := v.Term, v.Weight
			second, err := neighbors(via)
			if err != nil {
				return nil, err
			}
			for _, h := range second {
				if h.Term == term {
					continue
				}
				key := h.Term + "\x00" + h.Kind
				if cur, ok := seen[key]; !ok || w1+h.Weight > cur.Weight {
					seen[key] = RelatedHit{Term: h.Term, Weight: w1 + h.Weight, Via: via, Kind: h.Kind}
				}
			}
		}
	}
	out := make([]RelatedHit, 0, len(seen))
	for _, h := range seen {
		out = append(out, h)
	}
	sortHits(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// maxVia bounds depth-2 expansion to the strongest direct neighbors.
const maxVia = 8

// sortHits ranks typed relations above co-mentions, then by weight: a
// rule-extracted link outranks any number of bare co-mentions.
func sortHits(h []RelatedHit) {
	sort.Slice(h, func(i, j int) bool {
		a, b := h[i], h[j]
		ta, tb := a.Kind != "" && a.Kind != "co-mention", b.Kind != "" && b.Kind != "co-mention"
		if ta != tb {
			return ta
		}
		if a.Weight != b.Weight {
			return a.Weight > b.Weight
		}
		if a.Term != b.Term {
			return a.Term < b.Term
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Via < b.Via
	})
}

// Count returns (sessions, messages) totals, for ingest reporting.
func (db *DB) Count() (sessions, messages int, err error) {
	if err = db.sql.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		return
	}
	err = db.sql.QueryRow(`SELECT count(*) FROM messages`).Scan(&messages)
	return
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
