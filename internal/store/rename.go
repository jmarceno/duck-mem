package store

import (
	"context"
	"database/sql/driver"
	"fmt"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"github.com/jmarceno/duck-mem/internal/project"
)

// named converts positional ? values to the slice go-duckdb's ExecContext takes.
func named(args ...any) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, a := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return out
}

// execOn runs one statement with positional parameters on conn.
func execOn(conn *duckdb.Conn, q string, args ...any) error {
	_, err := conn.ExecContext(context.Background(), q, named(args...))
	return err
}

// RenameProject replaces the exact project name old with new in every table
// that stores it (sessions, messages, checkpoints, and the topic graph) and
// returns the number of sessions renamed. The target name must be unused, so
// a rename never merges two projects. The graph rows are restaged under new
// IDs because topic and edge IDs embed the project name.
func (db *DB) RenameProject(old, new string) (int, error) {
	if old == "" || new == "" {
		return 0, fmt.Errorf("rename needs both a current and a new project name")
	}
	if old == new {
		return 0, fmt.Errorf("current and new project names are the same")
	}
	var n int
	if err := db.sql.QueryRow(`SELECT count(*) FROM sessions WHERE project=?`, old).Scan(&n); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("no project named %q", old)
	}
	var m int
	if err := db.sql.QueryRow(`SELECT count(*) FROM sessions WHERE project=?`, new).Scan(&m); err != nil {
		return 0, err
	}
	if m > 0 {
		return 0, fmt.Errorf("a project named %q already exists; a rename would merge them", new)
	}
	return n, db.withTx(func(conn *duckdb.Conn) error {
		// Plain tables store the project as a value.
		for _, q := range []string{
			`UPDATE sessions SET project=? WHERE project=?`,
			`UPDATE messages SET project=? WHERE project=?`,
			`UPDATE term_df SET project=? WHERE project=?`,
			`UPDATE msg_edges SET project=? WHERE project=?`,
			`UPDATE file_checkpoints SET project=? WHERE project=?`,
			`UPDATE project_aliases SET new_name=? WHERE new_name=?`,
			`UPDATE project_identities SET project=? WHERE project=?`,
		} {
			if err := execOn(conn, q, new, old); err != nil {
				return err
			}
		}
		// Sources keep reporting the original name; remember where it went.
		if err := execOn(conn, `DELETE FROM project_aliases WHERE old_name=?`, new); err != nil {
			return err
		}
		if err := execOn(conn, `INSERT INTO project_aliases(old_name, new_name) VALUES(?, ?)
			ON CONFLICT(old_name) DO UPDATE SET new_name=excluded.new_name`, old, new); err != nil {
			return err
		}
		// topic_id = project + chr(31) + term; edge_id embeds both topic
		// IDs with chr(30) separators. Stage the project's rows under the
		// new name plus the surviving rows of every other project, then
		// swap the graph tables wholesale. Deleting children before
		// parents in one transaction is impossible: DuckDB's index-backed
		// FK check still sees rows deleted earlier in the same
		// transaction.
		if err := execOn(conn, `DROP TABLE IF EXISTS rename_topics`); err != nil {
			return err
		}
		if err := execOn(conn, `CREATE TEMP TABLE rename_topics AS
			SELECT ? || chr(31) || term AS topic_id, ? AS project, term, label
			FROM topics WHERE project = ?`, new, new, old); err != nil {
			return err
		}
		if err := execOn(conn, `DROP TABLE IF EXISTS rename_edges`); err != nil {
			return err
		}
		// term_a/term_b do not encode direction: both directions of an
		// edge store the same pair. Rebuild the IDs from the directed
		// src_id/dst_id columns instead: strip the old project prefix,
		// reattach the new one.
		if err := execOn(conn, `CREATE TEMP TABLE rename_edges AS
			SELECT
				(? || chr(31) || substr(src_id, length(?) + 2)) || chr(30) || (? || chr(31) || substr(dst_id, length(?) + 2)) || chr(30) || kind || chr(30) || from_term || chr(30) || to_term AS edge_id,
				? || chr(31) || substr(src_id, length(?) + 2) AS src_id,
				? || chr(31) || substr(dst_id, length(?) + 2) AS dst_id,
				? AS project,
				term_a, term_b, weight, kind, from_term, to_term, evidence
			FROM topic_edges WHERE project = ?`, new, old, new, old, new, old, new, old, new, old); err != nil {
			return err
		}
		if err := execOn(conn, `DROP TABLE IF EXISTS keep_topics`); err != nil {
			return err
		}
		if err := execOn(conn, `CREATE TEMP TABLE keep_topics AS SELECT * FROM topics WHERE project <> ?`, old); err != nil {
			return err
		}
		if err := execOn(conn, `DROP TABLE IF EXISTS keep_edges`); err != nil {
			return err
		}
		if err := execOn(conn, `CREATE TEMP TABLE keep_edges AS SELECT * FROM topic_edges WHERE project <> ?`, old); err != nil {
			return err
		}
		// Same drop/recreate as a full rebuild.
		if err := resetGraphTables(conn); err != nil {
			return err
		}
		if err := execOn(conn, `INSERT INTO topics(topic_id, project, term, label)
			SELECT topic_id, project, term, label FROM rename_topics
			UNION ALL SELECT topic_id, project, term, label FROM keep_topics`); err != nil {
			return err
		}
		if err := execOn(conn, `INSERT INTO topic_edges(edge_id, src_id, dst_id, project, term_a, term_b, weight, kind, from_term, to_term, evidence)
			SELECT edge_id, src_id, dst_id, project, term_a, term_b, weight, kind, from_term, to_term, evidence FROM rename_edges
			UNION ALL SELECT edge_id, src_id, dst_id, project, term_a, term_b, weight, kind, from_term, to_term, evidence FROM keep_edges`); err != nil {
			return err
		}
		for _, q := range []string{
			`DROP TABLE rename_topics`, `DROP TABLE rename_edges`,
			`DROP TABLE keep_topics`, `DROP TABLE keep_edges`,
		} {
			if err := execOn(conn, q); err != nil {
				return err
			}
		}
		return ensurePropertyGraphConn(conn, false)
	})
}

// ResolveProject returns the current name of a project that sources still
// report under a name it was renamed from. The incoming value is normalized
// first, so a trailing separator cannot escape the alias map.
func (db *DB) ResolveProject(name string) (string, error) {
	name = project.Clean(name)
	if name == "" {
		return "", nil
	}
	aliases, err := db.projectAliases(context.Background())
	if err != nil {
		return name, err
	}
	if next, ok := aliases[project.Fold(name)]; ok {
		return followAlias(next, aliases), nil
	}
	return name, nil
}
