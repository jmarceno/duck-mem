package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"

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

// RenameResult reports a finished rename. Old is the stored name the
// request resolved to, which may differ from what the caller typed.
type RenameResult struct {
	Sessions int    `json:"sessions"`
	Old      string `json:"old"`
	New      string `json:"new"`
}

// RenameProject replaces the project old with new in every table that stores
// it (sessions, messages, checkpoints, and the topic graph). old resolves the
// way --project does, minus the tiers that can select several projects: the
// exact value ignoring case, a renamed alias, or a unique basename. The
// target name must be unused, so a rename never merges two projects. The
// graph rows are restaged under new IDs because topic and edge IDs embed the
// project name.
func (db *DB) RenameProject(request, new string) (RenameResult, error) {
	new = project.Clean(new)
	if strings.TrimSpace(request) == "" || new == "" {
		return RenameResult{}, fmt.Errorf("rename needs both a current and a new project name")
	}
	old, err := db.renameSource(request)
	if err != nil {
		return RenameResult{}, err
	}
	if old == new {
		return RenameResult{}, fmt.Errorf("current and new project names are the same")
	}
	var n int
	if err := db.sql.QueryRow(`SELECT count(*) FROM sessions WHERE project=?`, old).Scan(&n); err != nil {
		return RenameResult{}, err
	}
	if n == 0 {
		return RenameResult{}, fmt.Errorf("no project named %q", request)
	}
	// Matching folds case, so a name differing only in case is taken too;
	// changing the case of the project itself is allowed.
	var m int
	if err := db.sql.QueryRow(`SELECT count(*) FROM sessions WHERE lower(project)=lower(?) AND project<>?`, new, old).Scan(&m); err != nil {
		return RenameResult{}, err
	}
	if m > 0 {
		return RenameResult{}, fmt.Errorf("a project named %q already exists; a rename would merge them", new)
	}
	res := RenameResult{Sessions: n, Old: old, New: new}
	return res, db.withTx(func(conn *duckdb.Conn) error {
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
		if err := execOn(conn, `DELETE FROM project_aliases WHERE lower(old_name)=lower(?)`, new); err != nil {
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

// renameSource resolves the name a rename was asked for to exactly one
// stored project, or explains why it cannot.
func (db *DB) renameSource(request string) (string, error) {
	ctx := context.Background()
	stats, err := db.ProjectStats(ctx)
	if err != nil {
		return "", err
	}
	aliases, err := db.projectAliases(ctx)
	if err != nil {
		return "", err
	}
	clean := project.Clean(request)
	exact := func(name string) []ProjectStat {
		var out []ProjectStat
		for _, st := range stats {
			if project.Fold(project.Clean(st.Project)) == project.Fold(project.Clean(name)) {
				out = append(out, st)
			}
		}
		return out
	}
	matched := exact(clean)
	if len(matched) == 0 {
		if target, ok := resolveAlias(clean, aliases); ok {
			matched = exact(target)
		}
	}
	if len(matched) == 0 {
		matched = matchBasename(clean, stats)
	}
	// Several stored values: prefer the one spelled exactly as asked.
	if len(matched) > 1 {
		for _, st := range matched {
			if st.Project == clean {
				return st.Project, nil
			}
		}
	}
	switch len(matched) {
	case 1:
		return matched[0].Project, nil
	case 0:
		msg := fmt.Sprintf("no project named %q", request)
		if c := projectCandidates(clean, stats, 3); len(c) > 0 {
			names := make([]string, len(c))
			for i, cand := range c {
				names[i] = cand.Project
			}
			msg += "; did you mean " + strings.Join(names, ", ") + "?"
		}
		return "", errors.New(msg)
	default:
		names := projectValues(matched)
		return "", fmt.Errorf("%q matches several projects (%s); pass the full name", request, strings.Join(names, ", "))
	}
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
