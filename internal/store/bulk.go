package store

import (
	"context"
	"database/sql/driver"
	"fmt"

	"duck-mem/internal/embed"
	"duck-mem/internal/topics"

	duckdb "github.com/marcboeker/go-duckdb/v2"
)

// withTx runs fn on the DuckDB connection inside one transaction.
// The appender writes on that connection, so they commit or roll back together.
// fn must not use db.sql: this holds the only pooled connection.
func (db *DB) withTx(fn func(*duckdb.Conn) error) error {
	c, err := db.sql.Conn(context.Background())
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Raw(func(dc any) error {
		conn, ok := dc.(*duckdb.Conn)
		if !ok {
			return fmt.Errorf("duckdb connection required, got %T", dc)
		}
		tx, err := conn.Begin()
		if err != nil {
			return err
		}
		if err := fn(conn); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	})
}

func execConn(conn *duckdb.Conn, query string) error {
	_, err := conn.ExecContext(context.Background(), query, nil)
	return err
}

func appendTable(conn *duckdb.Conn, table string, rows [][]driver.Value) error {
	if len(rows) == 0 {
		return nil
	}
	a, err := duckdb.NewAppenderFromConn(conn, "", table)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := a.AppendRow(row...); err != nil {
			_ = a.Close()
			return err
		}
	}
	return a.Close()
}

type stagedMessage struct {
	sessionID, source, project, role, text string
	seq                                    int32
	created                                any
	vec                                    []float32
}

func stageMessages(msgs []Message, ignoreDupes bool) []stagedMessage {
	rows := make([]stagedMessage, 0, len(msgs))
	seen := map[[2]string]bool{}
	for _, m := range msgs {
		if ignoreDupes {
			key := [2]string{m.SessionID, fmt.Sprint(m.Seq)}
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		rows = append(rows, stagedMessage{
			sessionID: m.SessionID,
			seq:       int32(m.Seq),
			source:    m.Source,
			project:   m.Project,
			role:      m.Role,
			text:      m.Text,
			created:   nullableTime(m.CreatedAt),
			vec:       embed.Embed(m.Text),
		})
	}
	return rows
}

func copyMessages(conn *duckdb.Conn, rows []stagedMessage, ignoreConflict bool) error {
	if len(rows) == 0 {
		return nil
	}
	if err := execConn(conn, `DROP TABLE IF EXISTS stage_messages`); err != nil {
		return err
	}
	if err := execConn(conn, `CREATE TEMP TABLE stage_messages(
		session_id TEXT, seq INTEGER, source TEXT, project TEXT, role TEXT, text TEXT,
		created_at TIMESTAMPTZ, indexed INTEGER, embedding FLOAT[])`); err != nil {
		return err
	}
	vals := make([][]driver.Value, len(rows))
	for i, r := range rows {
		var vec any
		if r.vec != nil {
			vec = r.vec
		}
		vals[i] = []driver.Value{r.sessionID, r.seq, r.source, r.project, r.role, r.text, r.created, int32(0), vec}
	}
	if err := appendTable(conn, "stage_messages", vals); err != nil {
		return err
	}
	q := `INSERT INTO messages(session_id, seq, source, project, role, text, created_at, indexed, embedding)
		SELECT session_id, seq, source, project, role, text, created_at, indexed,
			CASE WHEN embedding IS NULL THEN NULL ELSE embedding::FLOAT[` + fmt.Sprint(embed.Dim) + `] END
		FROM stage_messages`
	if ignoreConflict {
		q += ` ON CONFLICT(session_id, seq) DO NOTHING`
	}
	return execConn(conn, q)
}

type stagedTopic struct {
	id, project, term string
}

type stagedEdge struct {
	id, src, dst, project, a, b, kind, from, to, evidence string
	weight                                                int32
}

func collectEdges(edges map[ProjectEdge]int, evidence map[ProjectEdge]string) ([]stagedTopic, []stagedEdge) {
	topicsSeen := map[string]stagedTopic{}
	var out []stagedEdge
	for e, w := range edges {
		ev := ""
		if evidence != nil {
			ev = evidence[e]
		}
		ends := [][2]string{{e.A, e.B}, {e.B, e.A}}
		for _, end := range ends {
			src, dst := topicID(e.Project, end[0]), topicID(e.Project, end[1])
			topicsSeen[src] = stagedTopic{src, e.Project, end[0]}
			topicsSeen[dst] = stagedTopic{dst, e.Project, end[1]}
			out = append(out, stagedEdge{
				id: edgeID(src, dst, e.kind(), e.From, e.To), src: src, dst: dst,
				project: e.Project, a: e.A, b: e.B, weight: int32(w), kind: e.kind(),
				from: e.From, to: e.To, evidence: ev,
			})
		}
	}
	topics := make([]stagedTopic, 0, len(topicsSeen))
	for _, t := range topicsSeen {
		topics = append(topics, t)
	}
	return topics, out
}

func resetGraphTables(conn *duckdb.Conn) error {
	for _, q := range []string{
		`DROP PROPERTY GRAPH IF EXISTS topic_graph`,
		`DROP TABLE IF EXISTS topic_edges`,
		`DROP TABLE IF EXISTS topics`,
		`CREATE TABLE topics(
			topic_id TEXT PRIMARY KEY,
			project TEXT NOT NULL,
			term TEXT NOT NULL)`,
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
	} {
		if err := execConn(conn, q); err != nil {
			return err
		}
	}
	return nil
}

func copyTopicsAndEdges(conn *duckdb.Conn, topics []stagedTopic, edges []stagedEdge, addWeight bool) error {
	if err := execConn(conn, `DROP TABLE IF EXISTS stage_topics`); err != nil {
		return err
	}
	if err := execConn(conn, `DROP TABLE IF EXISTS stage_edges`); err != nil {
		return err
	}
	if err := execConn(conn, `CREATE TEMP TABLE stage_topics(topic_id TEXT, project TEXT, term TEXT)`); err != nil {
		return err
	}
	if err := execConn(conn, `CREATE TEMP TABLE stage_edges(
		edge_id TEXT, src_id TEXT, dst_id TEXT, project TEXT, term_a TEXT, term_b TEXT,
		weight INTEGER, kind TEXT, from_term TEXT, to_term TEXT, evidence TEXT)`); err != nil {
		return err
	}
	topicVals := make([][]driver.Value, len(topics))
	for i, t := range topics {
		topicVals[i] = []driver.Value{t.id, t.project, t.term}
	}
	if err := appendTable(conn, "stage_topics", topicVals); err != nil {
		return err
	}
	edgeVals := make([][]driver.Value, len(edges))
	for i, e := range edges {
		edgeVals[i] = []driver.Value{e.id, e.src, e.dst, e.project, e.a, e.b, e.weight, e.kind, e.from, e.to, e.evidence}
	}
	if err := appendTable(conn, "stage_edges", edgeVals); err != nil {
		return err
	}
	topicSQL := `INSERT INTO topics(topic_id, project, term) SELECT topic_id, project, term FROM stage_topics`
	edgeSQL := `INSERT INTO topic_edges(edge_id, src_id, dst_id, project, term_a, term_b, weight, kind, from_term, to_term, evidence)
		SELECT edge_id, src_id, dst_id, project, term_a, term_b, weight, kind, from_term, to_term, evidence FROM stage_edges`
	if addWeight {
		topicSQL += ` ON CONFLICT(topic_id) DO NOTHING`
		edgeSQL += ` ON CONFLICT(edge_id) DO UPDATE SET weight = topic_edges.weight + excluded.weight`
	}
	if err := execConn(conn, topicSQL); err != nil {
		return err
	}
	return execConn(conn, edgeSQL)
}

type stagedDF struct {
	project, term string
	msgs          int32
}

func copyTermDF(conn *duckdb.Conn, rows []stagedDF, add bool) error {
	if len(rows) == 0 {
		return nil
	}
	if err := execConn(conn, `DROP TABLE IF EXISTS stage_df`); err != nil {
		return err
	}
	if err := execConn(conn, `CREATE TEMP TABLE stage_df(project TEXT, term TEXT, msgs INTEGER)`); err != nil {
		return err
	}
	vals := make([][]driver.Value, len(rows))
	for i, r := range rows {
		vals[i] = []driver.Value{r.project, r.term, r.msgs}
	}
	if err := appendTable(conn, "stage_df", vals); err != nil {
		return err
	}
	q := `INSERT INTO term_df(project, term, msgs) SELECT project, term, msgs FROM stage_df`
	if add {
		q += ` ON CONFLICT(project, term) DO UPDATE SET msgs = term_df.msgs + excluded.msgs`
	}
	return execConn(conn, q)
}

type stagedEmb struct {
	sessionID string
	seq       int32
	vec       []float32
}

func copyEmbeddings(conn *duckdb.Conn, rows []stagedEmb) error {
	if len(rows) == 0 {
		return nil
	}
	// Rebuilding the HNSW index once beats updating it per row.
	if err := execConn(conn, `DROP INDEX IF EXISTS idx_messages_hnsw`); err != nil {
		return err
	}
	if err := execConn(conn, `DROP TABLE IF EXISTS stage_emb`); err != nil {
		return err
	}
	if err := execConn(conn, `CREATE TEMP TABLE stage_emb(session_id TEXT, seq INTEGER, embedding FLOAT[])`); err != nil {
		return err
	}
	vals := make([][]driver.Value, len(rows))
	for i, r := range rows {
		vals[i] = []driver.Value{r.sessionID, r.seq, r.vec}
	}
	if err := appendTable(conn, "stage_emb", vals); err != nil {
		return err
	}
	if err := execConn(conn, `UPDATE messages SET embedding = stage_emb.embedding::FLOAT[`+fmt.Sprint(embed.Dim)+`]
		FROM stage_emb
		WHERE messages.session_id = stage_emb.session_id AND messages.seq = stage_emb.seq`); err != nil {
		return err
	}
	return execConn(conn, `CREATE INDEX idx_messages_hnsw ON messages USING HNSW (embedding) WITH (metric = 'cosine')`)
}

func prepareMessages(texts []string, missing []bool) ([]topics.Doc, [][]float32) {
	docs := topics.AnalyzeMany(texts)
	vecs := make([][]float32, len(texts))
	for i, miss := range missing {
		if miss {
			vecs[i] = embed.EmbedTokens(docs[i].Tokens)
		}
	}
	return docs, vecs
}
