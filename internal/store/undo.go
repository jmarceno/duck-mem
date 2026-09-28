package store

import (
	"database/sql/driver"

	"github.com/jmarceno/duck-mem/internal/topics"

	duckdb "github.com/marcboeker/go-duckdb/v2"
)

// edgeLogRow is one edge contribution of one indexed message.
type edgeLogRow struct {
	sessionID string
	seq       int
	edge      ProjectEdge
}

func appendEdgeLog(conn *duckdb.Conn, rows []edgeLogRow) error {
	vals := make([][]driver.Value, len(rows))
	for i, r := range rows {
		e := r.edge
		vals[i] = []driver.Value{r.sessionID, int32(r.seq), e.Project, e.A, e.B, e.Kind, e.From, e.To}
	}
	return appendTable(conn, "msg_edges", vals)
}

// graphUndo is what a session's indexed messages added to the graph:
// edge weights (from msg_edges) and document frequencies (recomputed from
// the stored text, which is deterministic).
type graphUndo struct {
	edges []stagedEdge
	df    []stagedDF
}

// sessionContribution reads a session's graph contribution before its
// rows are replaced. Unindexed rows contributed nothing.
func (db *DB) sessionContribution(sessionID string) (graphUndo, error) {
	var u graphUndo
	rows, err := db.sql.Query(`SELECT project, text FROM messages WHERE session_id=? AND indexed=1`, sessionID)
	if err != nil {
		return u, err
	}
	var projects, texts []string
	for rows.Next() {
		var p, t string
		if err := rows.Scan(&p, &t); err != nil {
			rows.Close()
			return u, err
		}
		projects = append(projects, p)
		texts = append(texts, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return u, err
	}
	docs := topics.AnalyzeMany(texts)
	if _, err := db.stemDocs(docs); err != nil {
		return u, err
	}
	df := map[[2]string]int{}
	for i, d := range docs {
		for _, w := range d.Terms {
			df[[2]string{projects[i], w}]++
		}
	}
	for k, n := range df {
		u.df = append(u.df, stagedDF{project: k[0], term: k[1], msgs: int32(n)})
	}

	rows, err = db.sql.Query(`SELECT project, term_a, term_b, kind, from_term, to_term, count(*)
		FROM msg_edges WHERE session_id=? GROUP BY ALL`, sessionID)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	weights := map[ProjectEdge]int{}
	for rows.Next() {
		var e ProjectEdge
		var n int
		if err := rows.Scan(&e.Project, &e.A, &e.B, &e.Kind, &e.From, &e.To, &n); err != nil {
			return u, err
		}
		weights[e] = n
	}
	_, u.edges = collectEdges(weights, nil, nil)
	return u, rows.Err()
}

// apply subtracts the contribution and drops edges and df rows that reach
// zero. Nodes left without edges stay; traversal never reaches them.
func (u graphUndo) apply(conn *duckdb.Conn, sessionID string) error {
	if len(u.edges) > 0 {
		if err := execConn(conn, `CREATE OR REPLACE TEMP TABLE stage_undo_edges(edge_id TEXT, weight INTEGER)`); err != nil {
			return err
		}
		vals := make([][]driver.Value, len(u.edges))
		for i, e := range u.edges {
			vals[i] = []driver.Value{e.id, e.weight}
		}
		if err := appendTable(conn, "stage_undo_edges", vals); err != nil {
			return err
		}
		for _, q := range []string{
			`UPDATE topic_edges SET weight = topic_edges.weight - u.weight
				FROM stage_undo_edges u WHERE topic_edges.edge_id = u.edge_id`,
			`DELETE FROM topic_edges WHERE weight <= 0`,
		} {
			if err := execConn(conn, q); err != nil {
				return err
			}
		}
	}
	if len(u.df) > 0 {
		if err := execConn(conn, `CREATE OR REPLACE TEMP TABLE stage_undo_df(project TEXT, term TEXT, msgs INTEGER)`); err != nil {
			return err
		}
		vals := make([][]driver.Value, len(u.df))
		for i, r := range u.df {
			vals[i] = []driver.Value{r.project, r.term, r.msgs}
		}
		if err := appendTable(conn, "stage_undo_df", vals); err != nil {
			return err
		}
		for _, q := range []string{
			`UPDATE term_df SET msgs = term_df.msgs - u.msgs
				FROM stage_undo_df u WHERE term_df.project = u.project AND term_df.term = u.term`,
			`DELETE FROM term_df WHERE msgs <= 0`,
		} {
			if err := execConn(conn, q); err != nil {
				return err
			}
		}
	}
	return execConn(conn, `DELETE FROM msg_edges WHERE session_id=`+sqlString(sessionID))
}
