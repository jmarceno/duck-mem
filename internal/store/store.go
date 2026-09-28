package store

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"strconv"

	"github.com/jmarceno/duck-mem/internal/embed"
	"github.com/jmarceno/duck-mem/internal/topics"

	duckdb "github.com/marcboeker/go-duckdb/v2"
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

// DB wraps the DuckDB handle. readOnly sessions attach the file from an
// in-memory connection so duckpgq can build its property graph without
// writing the memory file.
type DB struct {
	sql      *sql.DB
	readOnly bool
}

// FileCheckpoint records the file state already reflected in messages.
type FileCheckpoint struct {
	Path          string
	Size          int64
	ModTimeNS     int64
	TailHash      string
	EndsLine      bool
	SessionID     string
	Project       string
	ParserVersion int
}

// Open creates/opens the DuckDB file and initializes the schema.
// vss and duckpgq are required: search and related do not run without them.
func Open(path string) (*DB, error) {
	sdb, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(1)
	db := &DB{sql: sdb}
	if err := db.init(); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return db, nil
}

// OpenReadOnly attaches the database file read-only from an in-memory
// DuckDB. Query and related then run vss and duckpgq against that file
// without taking its write lock. A missing database is an error.
func OpenReadOnly(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("database not found: %s (run ingest or daemon first)", path)
	}
	sdb, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(1)
	db := &DB{sql: sdb, readOnly: true}
	if err := loadExtensions(sdb); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	if _, err := sdb.Exec(fmt.Sprintf(`ATTACH '%s' AS src (READ_ONLY)`, strings.ReplaceAll(path, `'`, `''`))); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	// The property graph lives on this connection. Creating it before USE
	// writes duckpgq's catalog into memory, not into the attached file.
	if err := db.ensurePropertyGraph(); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	if _, err := sdb.Exec(`USE src`); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	if err := sdb.Ping(); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return db, nil
}

func loadExtensions(sdb *sql.DB) error {
	for _, q := range []string{
		`INSTALL vss`,
		`LOAD vss`,
		`INSTALL duckpgq FROM community`,
		`INSTALL fts`,
		`LOAD fts`,
		`LOAD duckpgq`,
		`SET hnsw_enable_experimental_persistence = true`,
		`SET hnsw_ef_search = 256`,
	} {
		if _, err := sdb.Exec(q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}

func (db *DB) init() error {
	if err := loadExtensions(db.sql); err != nil {
		return err
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
		// msg_terms is the BM25 posting list: stemmed term frequencies
		// per message, maintained with the messages themselves.
		`CREATE TABLE IF NOT EXISTS msg_terms(
			session_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			term TEXT NOT NULL,
			tf INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS file_checkpoints(
			path TEXT PRIMARY KEY,
			size BIGINT NOT NULL,
			mtime_ns BIGINT NOT NULL,
			tail_hash TEXT NOT NULL,
			ends_line BOOLEAN NOT NULL,
			session_id TEXT NOT NULL,
			project TEXT NOT NULL,
			parser_version INTEGER NOT NULL)`,
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
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS embedding FLOAT[` + strconv.Itoa(embed.Dim) + `]`,
		`UPDATE messages SET indexed=1 WHERE indexed IS NULL`,
	} {
		if _, err := db.sql.Exec(q); err != nil {
			return err
		}
	}
	if err := db.ensureEdgesSchema(); err != nil {
		return err
	}
	if err := db.ensureTermsSchema(); err != nil {
		return err
	}
	if err := db.ensureVectorIndex(); err != nil {
		return err
	}
	return db.ensurePropertyGraph()
}

const termsSchemaVersion = "1"

// ensureTermsSchema backfills the posting list for databases created
// before keyword search, or rebuilt after a tokenizer change.
func (db *DB) ensureTermsSchema() error {
	var v string
	_ = db.sql.QueryRow(`SELECT value FROM meta WHERE key='terms_schema'`).Scan(&v)
	if v == termsSchemaVersion {
		return nil
	}
	return db.withTx(func(conn *duckdb.Conn) error {
		if err := execConn(conn, `DELETE FROM msg_terms`); err != nil {
			return err
		}
		if err := execConn(conn, insertTermsSQL(`SELECT session_id, seq, text FROM messages`)); err != nil {
			return err
		}
		return execConn(conn, `INSERT INTO meta(key, value) VALUES('terms_schema', '`+termsSchemaVersion+`')
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	})
}

func (db *DB) ensureVectorIndex() error {
	_, err := db.sql.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_hnsw ON messages USING HNSW (embedding) WITH (metric = 'cosine')`)
	return err
}

// Close releases the handle.
func (db *DB) Close() error { return db.sql.Close() }

func (db *DB) GetCheckpoint(path string) (*FileCheckpoint, error) {
	var c FileCheckpoint
	err := db.sql.QueryRow(`SELECT path, size, mtime_ns, tail_hash, ends_line, session_id, project, parser_version
		FROM file_checkpoints WHERE path=?`, path).
		Scan(&c.Path, &c.Size, &c.ModTimeNS, &c.TailHash, &c.EndsLine, &c.SessionID, &c.Project, &c.ParserVersion)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (db *DB) SaveCheckpoint(c FileCheckpoint) error {
	_, err := db.sql.Exec(`INSERT INTO file_checkpoints(path, size, mtime_ns, tail_hash, ends_line, session_id, project, parser_version)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime_ns=excluded.mtime_ns,
		tail_hash=excluded.tail_hash, ends_line=excluded.ends_line,
		session_id=excluded.session_id, project=excluded.project, parser_version=excluded.parser_version`,
		c.Path, c.Size, c.ModTimeNS, c.TailHash, c.EndsLine, c.SessionID, c.Project, c.ParserVersion)
	return err
}

func (db *DB) GetSession(id string) (Session, error) {
	var s Session
	var started sql.NullTime
	err := db.sql.QueryRow(`SELECT session_id, source, project, started_at, path FROM sessions WHERE session_id=?`, id).
		Scan(&s.ID, &s.Source, &s.Project, &started, &s.Path)
	if started.Valid {
		s.StartedAt = started.Time
	}
	return s, err
}

func (db *DB) LastMessage(sessionID string) (*Message, error) {
	var m Message
	var at sql.NullTime
	err := db.sql.QueryRow(`SELECT session_id, seq, source, project, role, text, created_at
		FROM messages WHERE session_id=? ORDER BY seq DESC LIMIT 1`, sessionID).
		Scan(&m.SessionID, &m.Seq, &m.Source, &m.Project, &m.Role, &m.Text, &at)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if at.Valid {
		m.CreatedAt = at.Time
	}
	return &m, nil
}

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
// changed. Reparsed sessions can gain, lose, or reorder messages; the old
// snapshot's graph contributions are then subtracted and the new rows
// index incrementally, so a rewrite costs one session, not the corpus.
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
	start := 0
	if prefixMatches {
		start = len(old)
	}
	for _, m := range msgs[start:] {
		if m.SessionID != sessionID {
			return fmt.Errorf("message session %q does not match %q", m.SessionID, sessionID)
		}
	}
	fresh := msgs[start:]
	var undo graphUndo
	if !prefixMatches && len(old) > 0 {
		if undo, err = db.sessionContribution(sessionID); err != nil {
			return err
		}
	}
	return db.withTx(func(conn *duckdb.Conn) error {
		if !prefixMatches {
			if err := undo.apply(conn, sessionID); err != nil {
				return err
			}
			if err := execConn(conn, `DELETE FROM messages WHERE session_id=`+sqlString(sessionID)); err != nil {
				return err
			}
			if err := execConn(conn, `DELETE FROM msg_terms WHERE session_id=`+sqlString(sessionID)); err != nil {
				return err
			}
		}
		return copyMessages(conn, stageMessages(fresh, false), false)
	})
}

// InsertMessages stores messages; re-ingesting the same (session_id, seq)
// is a no-op so ingest runs are idempotent. New rows enter unindexed
// (indexed=0) for the next incremental IndexNew run.
func (db *DB) InsertMessages(msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	rows := stageMessages(msgs, true)
	return db.withTx(func(conn *duckdb.Conn) error {
		return copyMessages(conn, rows, true)
	})
}

func (db *DB) requireHNSW() error {
	var name string
	err := db.sql.QueryRow(`SELECT index_name FROM duckdb_indexes() WHERE index_name = 'idx_messages_hnsw'`).Scan(&name)
	if err == sql.ErrNoRows || name == "" {
		return fmt.Errorf("vss HNSW index idx_messages_hnsw is missing")
	}
	return err
}

// TextDoc is one message text plus its project, for per-project indexing.
type TextDoc struct {
	SessionID string
	Seq       int
	Project   string
	Text      string
}

// ProjectEdge is one weighted co-mention or directed typed relation.
type ProjectEdge struct {
	Project  string
	A, B     string
	From, To string
	// Kind names the relation type. Empty means 'co-mention'.
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
	rows, err := db.sql.Query(`SELECT session_id, seq, project, text FROM messages`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TextDoc
	for rows.Next() {
		var d TextDoc
		if err := rows.Scan(&d.SessionID, &d.Seq, &d.Project, &d.Text); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ReplaceEdges rebuilds the co-occurrence graph wholesale. Terms are graph
// terms: Porter stems, as IndexNew and IndexFull store them.
func (db *DB) ReplaceEdges(edges map[ProjectEdge]int) error {
	if err := db.ensureEdgesSchema(); err != nil {
		return err
	}
	topics, staged := collectEdges(edges, nil, nil)
	return db.withTx(func(conn *duckdb.Conn) error {
		if err := resetGraphTables(conn); err != nil {
			return err
		}
		if err := copyTopicsAndEdges(conn, topics, staged, false); err != nil {
			return err
		}
		return ensurePropertyGraphConn(conn, false)
	})
}

// v4 stores directed endpoints so duckpgq can traverse topic_edges.
const edgesSchemaVersion = "6"

// ensureEdgesSchema recreates outdated topic_edges tables (which carry no
// state worth migrating: a rebuild restores them).
func (db *DB) ensureEdgesSchema() error {
	var v string
	_ = db.sql.QueryRow(`SELECT value FROM meta WHERE key='edges_schema'`).Scan(&v)
	if v == edgesSchemaVersion {
		return nil
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DROP PROPERTY GRAPH IF EXISTS topic_graph`,
		`DROP TABLE IF EXISTS topic_edges`,
		`DROP TABLE IF EXISTS topics`,
		`CREATE TABLE topics(
			topic_id TEXT PRIMARY KEY,
			project TEXT NOT NULL,
			term TEXT NOT NULL,
			label TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE topic_edges(
			edge_id TEXT PRIMARY KEY,
			src_id TEXT NOT NULL REFERENCES topics(topic_id),
			dst_id TEXT NOT NULL REFERENCES topics(topic_id),
			project TEXT NOT NULL DEFAULT '',
			term_a TEXT NOT NULL,
			term_b TEXT NOT NULL,
			weight INTEGER NOT NULL,
			kind TEXT NOT NULL DEFAULT 'co-mention',
			from_term TEXT NOT NULL DEFAULT '',
			to_term TEXT NOT NULL DEFAULT '',
			evidence TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO meta(key, value) VALUES('edges_schema', '` + edgesSchemaVersion + `')
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		`DROP TABLE IF EXISTS msg_edges`,
		// msg_edges logs each edge contribution per message (weight 1 per
		// row), so a rewritten session can be subtracted exactly.
		`CREATE TABLE msg_edges(
			session_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			project TEXT NOT NULL,
			term_a TEXT NOT NULL,
			term_b TEXT NOT NULL,
			kind TEXT NOT NULL,
			from_term TEXT NOT NULL,
			to_term TEXT NOT NULL)`,
		// The graph was just dropped: every message needs indexing again.
		`UPDATE messages SET indexed=0`,
		`DELETE FROM term_df`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.ensurePropertyGraph()
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
	rows, err := db.sql.Query(`SELECT session_id, seq, project, text, embedding IS NULL FROM messages WHERE indexed=0`)
	if err != nil {
		return 0, 0, err
	}
	type doc struct {
		sid, proj, text string
		seq             int
		missing         bool
	}
	var docs []doc
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.sid, &d.seq, &d.proj, &d.text, &d.missing); err != nil {
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
	texts := make([]string, len(docs))
	missing := make([]bool, len(docs))
	for i, d := range docs {
		texts[i] = d.text
		missing[i] = d.missing
	}
	scanned, vecs := prepareMessages(texts, missing)
	labels, err := db.stemDocs(scanned)
	if err != nil {
		return 0, 0, err
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
	for i, d := range docs {
		for _, w := range scanned[i].Terms {
			dfDelta[[2]string{d.proj, w}]++
			getDF(d.proj)[w]++
		}
	}
	pairDelta := map[ProjectEdge]int{}
	evidence := map[ProjectEdge]string{}
	var log []edgeLogRow
	for i, d := range docs {
		maxDF := topics.HubCap(totals[d.proj], minDF)
		for _, e := range topics.SelectPairsFromTerms(scanned[i].Terms, getDF(d.proj), minDF, maxDF) {
			pe := ProjectEdge{Project: d.proj, A: e[0], B: e[1]}
			pairDelta[pe]++
			log = append(log, edgeLogRow{d.sid, d.seq, pe})
		}
		for _, t := range scanned[i].Typed {
			e := ProjectEdge{Project: d.proj, A: t.A, B: t.B, From: t.From, To: t.To, Kind: t.Kind}
			pairDelta[e]++
			log = append(log, edgeLogRow{d.sid, d.seq, e})
			if evidence[e] == "" {
				evidence[e] = fmt.Sprintf("%s#%d: %s", d.sid, d.seq, t.Evidence)
			}
		}
	}
	topicRows, edgeRows := collectEdges(pairDelta, evidence, labels)
	dfRows := make([]stagedDF, 0, len(dfDelta))
	for pt, n := range dfDelta {
		dfRows = append(dfRows, stagedDF{project: pt[0], term: pt[1], msgs: int32(n)})
	}
	var embRows []stagedEmb
	for i, d := range docs {
		if vecs[i] != nil {
			embRows = append(embRows, stagedEmb{sessionID: d.sid, seq: int32(d.seq), vec: vecs[i]})
		}
	}
	if err := db.withTx(func(conn *duckdb.Conn) error {
		if err := copyTopicsAndEdges(conn, topicRows, edgeRows, true); err != nil {
			return err
		}
		if err := copyTermDF(conn, dfRows, true); err != nil {
			return err
		}
		if err := appendEdgeLog(conn, log); err != nil {
			return err
		}
		if err := copyEmbeddings(conn, embRows); err != nil {
			return err
		}
		return execConn(conn, `UPDATE messages SET indexed=1 WHERE indexed=0`)
	}); err != nil {
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
	missingSet, err := db.missingEmbeddings()
	if err != nil {
		return err
	}
	groups := map[string][]int{}
	texts := make([]string, len(docs))
	missing := make([]bool, len(docs))
	for i, d := range docs {
		texts[i] = d.Text
		groups[d.Project] = append(groups[d.Project], i)
		_, missing[i] = missingSet[msgKey{d.SessionID, d.Seq}]
	}
	scanned, vecs := prepareMessages(texts, missing)
	labels, err := db.stemDocs(scanned)
	if err != nil {
		return err
	}
	edgeW := map[ProjectEdge]int{}
	evidence := map[ProjectEdge]string{}
	var dfRows []stagedDF
	var log []edgeLogRow
	for proj, idxs := range groups {
		df := map[string]int{}
		for _, i := range idxs {
			for _, w := range scanned[i].Terms {
				df[w]++
			}
		}
		maxDF := topics.HubCap(len(idxs), minDF)
		for _, i := range idxs {
			d := docs[i]
			for _, e := range topics.SelectPairsFromTerms(scanned[i].Terms, df, minDF, maxDF) {
				pe := ProjectEdge{Project: proj, A: e[0], B: e[1]}
				edgeW[pe]++
				log = append(log, edgeLogRow{d.SessionID, d.Seq, pe})
			}
			for _, tp := range scanned[i].Typed {
				e := ProjectEdge{Project: proj, A: tp.A, B: tp.B, From: tp.From, To: tp.To, Kind: tp.Kind}
				edgeW[e]++
				log = append(log, edgeLogRow{d.SessionID, d.Seq, e})
				if evidence[e] == "" {
					evidence[e] = fmt.Sprintf("%s#%d: %s", d.SessionID, d.Seq, tp.Evidence)
				}
			}
		}
		for w, n := range df {
			dfRows = append(dfRows, stagedDF{project: proj, term: w, msgs: int32(n)})
		}
	}
	topicRows, edgeRows := collectEdges(edgeW, evidence, labels)
	var embRows []stagedEmb
	for i, d := range docs {
		if vecs[i] != nil {
			embRows = append(embRows, stagedEmb{sessionID: d.SessionID, seq: int32(d.Seq), vec: vecs[i]})
		}
	}
	return db.withTx(func(conn *duckdb.Conn) error {
		if err := resetGraphTables(conn); err != nil {
			return err
		}
		if err := execConn(conn, `DELETE FROM term_df`); err != nil {
			return err
		}
		if err := execConn(conn, `DELETE FROM msg_edges`); err != nil {
			return err
		}
		if err := appendEdgeLog(conn, log); err != nil {
			return err
		}
		if err := copyTopicsAndEdges(conn, topicRows, edgeRows, false); err != nil {
			return err
		}
		if err := ensurePropertyGraphConn(conn, false); err != nil {
			return err
		}
		if err := copyTermDF(conn, dfRows, false); err != nil {
			return err
		}
		if err := copyEmbeddings(conn, embRows); err != nil {
			return err
		}
		if err := execConn(conn, `UPDATE messages SET indexed=1`); err != nil {
			return err
		}
		return execConn(conn, `DELETE FROM meta WHERE key='graph_dirty'`)
	})
}

type msgKey struct {
	sessionID string
	seq       int
}

func (db *DB) missingEmbeddings() (map[msgKey]struct{}, error) {
	rows, err := db.sql.Query(`SELECT session_id, seq FROM messages WHERE embedding IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[msgKey]struct{}{}
	for rows.Next() {
		var k msgKey
		if err := rows.Scan(&k.sessionID, &k.seq); err != nil {
			return nil, err
		}
		out[k] = struct{}{}
	}
	return out, rows.Err()
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
	Term     string
	Weight   int
	Via      string
	Kind     string
	From, To string
	Evidence string
}

// Related returns topics reached from term by a duckpgq traversal.
// Empty project searches all projects; otherwise vertices are filtered
// by project substring. Depth 1 ranks direct neighbors; depth 2 adds
// neighbors-of-neighbors, scored by the two-hop weight sum.
func (db *DB) Related(term, project string, depth, limit int) ([]RelatedHit, error) {
	if limit < 1 || (depth != 1 && depth != 2) {
		return nil, fmt.Errorf("related needs a positive limit and depth 1 or 2")
	}
	term = strings.ToLower(strings.TrimSpace(term))
	if term == "" {
		return nil, fmt.Errorf("related needs a term")
	}
	// Graph nodes are Porter stems; look up the stem of what was typed.
	stems, err := db.stemWords([]string{term})
	if err != nil {
		return nil, err
	}
	term = stems[term]
	seen := map[string]RelatedHit{}
	direct := map[string]int{}
	first, err := db.graphNeighbors(term, project)
	if err != nil {
		return nil, err
	}
	for _, h := range first {
		seen[relatedKey(h)] = h
		if w, ok := direct[h.Term]; !ok || h.Weight > w {
			direct[h.Term] = h.Weight
		}
	}
	if depth >= 2 {
		directKeys := map[string]bool{}
		for key := range seen {
			directKeys[key] = true
		}
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
		vias := map[string]int{}
		for _, v := range ordered {
			vias[v.Term] = v.Weight
		}
		second, err := db.graphTwoHop(term, project)
		if err != nil {
			return nil, err
		}
		for _, h := range second {
			w1, ok := vias[h.Via]
			if !ok || h.Term == term {
				continue
			}
			key := relatedKey(h)
			if directKeys[key] {
				continue
			}
			h.Weight += w1
			if cur, ok := seen[key]; !ok || h.Weight > cur.Weight {
				seen[key] = h
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
	return out, db.labelHits(out)
}

func (db *DB) graphNeighbors(term, project string) ([]RelatedHit, error) {
	q := `SELECT g.term, sum(g.weight)::INTEGER, g.kind, g.from_term, g.to_term, min(g.evidence) FROM (
		SELECT * FROM GRAPH_TABLE (topic_graph
			MATCH (a:topics)-[e:Rel]->(b:topics)
			COLUMNS (
				a.term AS src,
				a.project AS project,
				b.term AS term,
				e.weight AS weight,
				e.kind AS kind,
				e.from_term AS from_term,
				e.to_term AS to_term,
				e.evidence AS evidence
			)
		)
	) g WHERE g.src = ` + sqlString(term)
	if project != "" {
		q += ` AND contains(lower(g.project), lower(` + sqlString(project) + `))`
	}
	q += ` GROUP BY g.term, g.kind, g.from_term, g.to_term`
	rows, err := db.sql.Query(q)
	if err != nil {
		return nil, fmt.Errorf("duckpgq traversal: %w", err)
	}
	defer rows.Close()
	var out []RelatedHit
	for rows.Next() {
		var h RelatedHit
		if err := rows.Scan(&h.Term, &h.Weight, &h.Kind, &h.From, &h.To, &h.Evidence); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (db *DB) graphTwoHop(term, project string) ([]RelatedHit, error) {
	q := `SELECT g.via, g.term, sum(g.weight)::INTEGER, g.kind, g.from_term, g.to_term, min(g.evidence) FROM (
		SELECT * FROM GRAPH_TABLE (topic_graph
			MATCH (a:topics)-[e1:Rel]->(hop:topics)-[e2:Rel]->(b:topics)
			COLUMNS (
				a.term AS src,
				a.project AS project,
				hop.term AS via,
				b.term AS term,
				e2.weight AS weight,
				e2.kind AS kind,
				e2.from_term AS from_term,
				e2.to_term AS to_term,
				e2.evidence AS evidence
			)
		)
	) g WHERE g.src = ` + sqlString(term) + ` AND g.term <> ` + sqlString(term)
	if project != "" {
		q += ` AND contains(lower(g.project), lower(` + sqlString(project) + `))`
	}
	q += ` GROUP BY g.via, g.term, g.kind, g.from_term, g.to_term`
	rows, err := db.sql.Query(q)
	if err != nil {
		return nil, fmt.Errorf("duckpgq traversal: %w", err)
	}
	defer rows.Close()
	var out []RelatedHit
	for rows.Next() {
		var h RelatedHit
		if err := rows.Scan(&h.Via, &h.Term, &h.Weight, &h.Kind, &h.From, &h.To, &h.Evidence); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// maxVia bounds depth-2 expansion to the strongest direct neighbors.
const maxVia = 8

func relatedKey(h RelatedHit) string {
	return h.Term + "\x00" + h.Kind + "\x00" + h.From + "\x00" + h.To
}

// minTypedWeight is how often a rule-extracted relation must recur before
// it outranks co-mentions. A single possessive or "replace" sentence is
// too often a misparse to lead the list.
const minTypedWeight = 2

// sortHits ranks recurring typed relations above co-mentions, then by
// weight. One-off typed relations rank by weight like co-mentions.
func sortHits(h []RelatedHit) {
	sort.Slice(h, func(i, j int) bool {
		a, b := h[i], h[j]
		ta := a.Kind != "" && a.Kind != "co-mention" && a.Weight >= minTypedWeight
		tb := b.Kind != "" && b.Kind != "co-mention" && b.Weight >= minTypedWeight
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
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
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

func topicID(project, term string) string {
	return project + "\x1f" + term
}

func edgeID(src, dst, kind, from, to string) string {
	return src + "\x1e" + dst + "\x1e" + kind + "\x1e" + from + "\x1e" + to
}

func (db *DB) ensurePropertyGraph() error {
	if db.readOnly {
		return db.withTx(func(conn *duckdb.Conn) error {
			return ensurePropertyGraphConn(conn, true)
		})
	}
	_, err := db.sql.Exec(propertyGraphSQL(false))
	if err != nil {
		return fmt.Errorf("duckpgq property graph: %w", err)
	}
	return nil
}

func ensurePropertyGraphConn(conn *duckdb.Conn, readOnly bool) error {
	if err := execConn(conn, propertyGraphSQL(readOnly)); err != nil {
		return fmt.Errorf("duckpgq property graph: %w", err)
	}
	return nil
}

func propertyGraphSQL(readOnly bool) string {
	topics, edges := "topics", "topic_edges"
	if readOnly {
		topics, edges = "src.topics", "src.topic_edges"
	}
	return fmt.Sprintf(`CREATE OR REPLACE PROPERTY GRAPH topic_graph
		VERTEX TABLES (%s)
		EDGE TABLES (
			%s
				SOURCE KEY (src_id) REFERENCES %s (topic_id)
				DESTINATION KEY (dst_id) REFERENCES %s (topic_id)
				LABEL Rel
		)`, topics, edges, topics, topics)
}

func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 8)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'f', 6, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
