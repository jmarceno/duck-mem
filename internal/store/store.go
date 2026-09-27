package store

import (
	"database/sql"
	"strings"
	"time"

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
// Extensions (vss, duckpgq) load best-effort: iteration 1 search is plain
// ILIKE, so a missing extension (e.g. offline machine) must not fail.
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

func (db *DB) init() error {
	for _, q := range []string{
		`INSTALL vss`,
		`LOAD vss`,
		`INSTALL duckpgq`,
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
	} {
		if _, err := db.sql.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// Close releases the handle.
func (db *DB) Close() error { return db.sql.Close() }

// UpsertSession records a session; re-ingests update path/started_at.
func (db *DB) UpsertSession(s Session) error {
	_, err := db.sql.Exec(
		`INSERT INTO sessions(session_id, source, project, started_at, path)
		 VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET path=excluded.path, started_at=excluded.started_at`,
		s.ID, s.Source, s.Project, nullableTime(s.StartedAt), s.Path)
	return err
}

// InsertMessages stores messages; re-ingesting the same (session_id, seq)
// is a no-op so ingest runs are idempotent.
func (db *DB) InsertMessages(msgs []Message) error {
	for _, m := range msgs {
		if _, err := db.sql.Exec(
			`INSERT INTO messages(session_id, seq, source, project, role, text, created_at)
			 VALUES(?, ?, ?, ?, ?, ?, ?)
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
	toks := strings.Fields(query)
	var sb strings.Builder
	args := []any{}
	sb.WriteString(`SELECT session_id, seq, source, project, role, substr(text, 1, 300)
		FROM messages WHERE 1=1`)
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
