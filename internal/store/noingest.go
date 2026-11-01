package store

import (
	duckdb "github.com/duckdb/duckdb-go/v2"
)

// MarkNoIngest records a session as permanently excluded. Re-marking
// refreshes the path and tag.
func (db *DB) MarkNoIngest(s Session, tag string) error {
	_, err := db.sql.Exec(`INSERT INTO no_ingest(session_id, path, tag) VALUES(?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET path=excluded.path, tag=excluded.tag`,
		s.ID, s.Path, tag)
	return err
}

// IsNoIngest reports whether a session is marked. Marks are permanent:
// they outlive the log file that carried the tag.
func (db *DB) IsNoIngest(sessionID string) (bool, error) {
	var n int
	err := db.sql.QueryRow(`SELECT count(*) FROM no_ingest WHERE session_id=?`, sessionID).Scan(&n)
	return n > 0, err
}

// PurgeSession removes a session's stored rows, subtracting its graph
// contribution first so the topic graph and term frequencies stay honest.
// DuckDB checks a parent delete against the committed state, so the
// referencing rows go in one transaction and the sessions row follows in
// the next. A crash in between leaves a session without messages; the
// next cycle purges it again, so the split is idempotent.
func (db *DB) PurgeSession(sessionID string) error {
	undo, err := db.sessionContribution(sessionID)
	if err != nil {
		return err
	}
	if err := db.withTx(func(conn *duckdb.Conn) error {
		if err := undo.apply(conn, sessionID); err != nil {
			return err
		}
		for _, table := range []string{"msg_terms", "messages", "session_copies"} {
			if err := execConn(conn, `DELETE FROM `+table+` WHERE session_id=`+sqlString(sessionID)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	_, err = db.sql.Exec(`DELETE FROM sessions WHERE session_id=` + sqlString(sessionID))
	return err
}
