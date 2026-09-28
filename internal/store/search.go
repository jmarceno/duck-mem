package store

import (
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jmarceno/duck-mem/internal/embed"
)

// Hit is one matching message. Snippet is centered on the densest run of
// query terms.
type Hit struct {
	SessionID string
	Seq       int
	Source    string
	Project   string
	Role      string
	Snippet   string
	CreatedAt time.Time
	Score     float64
}

// tokenSQL splits text into lowercase words. The posting list and query
// terms both go through it, then through the Porter stemmer.
const tokenSQL = `regexp_split_to_array(lower(%s), '[^\p{L}\p{N}_]+')`

// insertTermsSQL fills msg_terms from a (session_id, seq, text) query.
// Stemming each distinct word once keeps a full backfill in seconds.
func insertTermsSQL(src string) string {
	return `INSERT INTO msg_terms(session_id, seq, term, tf) SELECT * FROM (
		WITH w AS (SELECT session_id, seq, unnest(` + fmt.Sprintf(tokenSQL, "text") + `) AS word FROM (` + src + `)),
		s AS (SELECT word, stem(word, 'porter') AS term FROM (SELECT DISTINCT word FROM w WHERE length(word) BETWEEN 2 AND 40))
		SELECT session_id, seq, term, count(*)::INTEGER FROM w JOIN s USING (word) GROUP BY ALL)`
}

// maxCosineDistance drops vector neighbors that share essentially no terms.
// array_cosine_distance is 0 for a match and about 1 for orthogonal vectors.
const maxCosineDistance = 0.65

// BM25 parameters (the usual defaults) and the reciprocal-rank-fusion constant.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
	rrfK   = 60
)

// Search ranks messages by the query's own words; see rank.
func (db *DB) Search(query, project, source string, limit int) ([]Hit, error) {
	terms, err := db.queryTerms(query)
	if err != nil {
		return nil, err
	}
	return db.rank(query, terms, nil, project, source, limit)
}

// Graph expansion: the most specific duckpgq neighbours of the query
// terms, searched at a fraction of a query term's weight. Blending them
// into every search lowered MRR on real recall queries (0.88 -> 0.77-0.84),
// so they only fill result slots the query's own words leave empty.
const (
	expansionTerms  = 6
	expansionWeight = 0.3
)

// rank fuses two rankings with reciprocal rank fusion:
//   - BM25 over stemmed terms (recall, word variants, long messages),
//     plus any expansion terms at expansionWeight;
//   - cosine distance on the vss HNSW index (phrase closeness). The
//     vectors are hashed terms, so a true neighbor always shares a word:
//     the vector ranking only reorders BM25 candidates, which drops hash
//     collisions between unrelated short messages.
//
// project and source only filter.
func (db *DB) rank(query string, terms []string, expand []Topic, project, source string, limit int) ([]Hit, error) {
	if limit < 1 {
		return nil, fmt.Errorf("limit must be positive")
	}
	if len(terms) == 0 {
		return nil, fmt.Errorf("query has no searchable terms")
	}
	weights := map[string]float64{}
	stems := slices.Clone(terms)
	for _, t := range terms {
		weights[t] = 1
	}
	for _, t := range expand {
		weights[t.Term] = expansionWeight
		stems = append(stems, t.Term)
	}
	fused := map[msgKey]float64{}
	ranked, err := db.keywordRank(weights, project, source, max(limit*10, 500))
	if err != nil {
		return nil, err
	}
	addRRF(fused, ranked)
	if vec := embed.Embed(query); vec != nil && len(fused) > 0 {
		ranked, err := db.vectorRank(vec, project, source, max(limit*5, 50))
		if err != nil {
			return nil, err
		}
		ranked = slices.DeleteFunc(ranked, func(r scored) bool {
			_, ok := fused[r.key]
			return !ok
		})
		addRRF(fused, ranked)
	}
	keys := make([]msgKey, 0, len(fused))
	for k := range fused {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if fused[keys[i]] != fused[keys[j]] {
			return fused[keys[i]] > fused[keys[j]]
		}
		if keys[i].sessionID != keys[j].sessionID {
			return keys[i].sessionID < keys[j].sessionID
		}
		return keys[i].seq < keys[j].seq
	})
	if len(keys) > limit {
		keys = keys[:limit]
	}
	msgs, err := db.messagesByKey(keys)
	if err != nil {
		return nil, err
	}
	out := make([]Hit, 0, len(keys))
	for _, k := range keys {
		m, ok := msgs[k]
		if !ok {
			continue
		}
		out = append(out, Hit{
			SessionID: m.SessionID, Seq: m.Seq, Source: m.Source, Project: m.Project, Role: m.Role,
			Snippet: Snippet(m.Text, stems, 240), CreatedAt: m.CreatedAt, Score: fused[k],
		})
	}
	return out, nil
}

type scored struct {
	key   msgKey
	score float64 // higher is better
}

// addRRF adds 1/(k+rank) per message. Equal scores share a rank, so a tie
// in one ranking leaves the other ranking to decide.
func addRRF(fused map[msgKey]float64, ranked []scored) {
	rank := 0
	for i, r := range ranked {
		if i == 0 || r.score != ranked[i-1].score {
			rank = i + 1
		}
		fused[r.key] += 1.0 / float64(rrfK+rank)
	}
}

// queryTerms stems the query exactly like the posting list.
func (db *DB) queryTerms(query string) ([]string, error) {
	rows, err := db.sql.Query(`SELECT DISTINCT stem(word, 'porter') FROM (SELECT unnest(`+fmt.Sprintf(tokenSQL, "?")+`) AS word)
		WHERE length(word) BETWEEN 2 AND 40`, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func filterSQL(alias, project, source string, args []any) (string, []any) {
	q := ""
	if project != "" {
		q += ` AND contains(lower(` + alias + `.project), lower(?))`
		args = append(args, project)
	}
	if source != "" {
		q += ` AND ` + alias + `.source = ?`
		args = append(args, source)
	}
	return q, args
}

// keywordRank scores messages with BM25 over msg_terms, each term's
// contribution scaled by its weight.
func (db *DB) keywordRank(weights map[string]float64, project, source string, limit int) ([]scored, error) {
	var n, total float64
	if err := db.sql.QueryRow(`SELECT (SELECT count(*) FROM messages), (SELECT coalesce(sum(tf), 0) FROM msg_terms)`).Scan(&n, &total); err != nil {
		if strings.Contains(err.Error(), "msg_terms") {
			return nil, fmt.Errorf("keyword index missing: restart the duck-mem daemon or run `duck-mem index` with this version")
		}
		return nil, err
	}
	if n == 0 || total == 0 {
		return nil, nil
	}
	vals := make([]string, 0, len(weights))
	for t, w := range weights {
		vals = append(vals, "("+sqlString(t)+", "+strconv.FormatFloat(w, 'f', -1, 64)+")")
	}
	filter, args := filterSQL("m", project, source, []any{n, total / n})
	q := `WITH qt AS (SELECT * FROM (VALUES ` + strings.Join(vals, ",") + `) v(term, qw)),
		df AS (SELECT term, any_value(qw) AS qw, count(*) AS df FROM msg_terms JOIN qt USING (term) GROUP BY term),
		cand AS (SELECT t.session_id, t.seq, t.term, t.tf, df.df, df.qw FROM msg_terms t JOIN df USING (term)),
		dl AS (SELECT t.session_id, t.seq, sum(t.tf) AS dl FROM msg_terms t
			JOIN (SELECT DISTINCT session_id, seq FROM cand) c USING (session_id, seq) GROUP BY ALL)
		SELECT c.session_id, c.seq,
			sum(c.qw * ln(1 + ($1 - c.df + 0.5) / (c.df + 0.5)) * c.tf * ` + strconv.FormatFloat(bm25K1+1, 'f', -1, 64) + ` /
				(c.tf + ` + strconv.FormatFloat(bm25K1, 'f', -1, 64) + ` * (` + strconv.FormatFloat(1-bm25B, 'f', -1, 64) + ` + ` +
		strconv.FormatFloat(bm25B, 'f', -1, 64) + ` * dl.dl / $2))) AS score
		FROM cand c JOIN dl USING (session_id, seq) JOIN messages m USING (session_id, seq)
		WHERE true` + positional(filter, 3) + `
		GROUP BY c.session_id, c.seq ORDER BY score DESC LIMIT ` + strconv.Itoa(limit)
	return db.scoredRows(q, args, false)
}

// positional rewrites ? placeholders to $start, $start+1, … so they can
// follow the numbered BM25 constants.
func positional(q string, start int) string {
	var b strings.Builder
	for _, r := range q {
		if r == '?' {
			b.WriteString("$" + strconv.Itoa(start))
			start++
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// vectorRank asks the HNSW index for the nearest messages.
func (db *DB) vectorRank(vec []float32, project, source string, limit int) ([]scored, error) {
	if err := db.requireHNSW(); err != nil {
		return nil, err
	}
	filter, args := filterSQL("messages", project, source, []any{vectorLiteral(vec)})
	q := `SELECT session_id, seq, array_cosine_distance(embedding, ?::FLOAT[` + strconv.Itoa(embed.Dim) + `]) AS dist
		FROM messages WHERE embedding IS NOT NULL` + filter + ` ORDER BY dist LIMIT ?`
	return db.scoredRows(q, append(args, limit), true)
}

func (db *DB) scoredRows(q string, args []any, distance bool) ([]scored, error) {
	rows, err := db.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scored
	for rows.Next() {
		var s scored
		if err := rows.Scan(&s.key.sessionID, &s.key.seq, &s.score); err != nil {
			return nil, err
		}
		if distance {
			if s.score > maxCosineDistance {
				continue
			}
			s.score = -s.score
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (db *DB) messagesByKey(keys []msgKey) (map[msgKey]Message, error) {
	out := map[msgKey]Message{}
	if len(keys) == 0 {
		return out, nil
	}
	vals := make([]string, len(keys))
	for i, k := range keys {
		vals[i] = fmt.Sprintf("(%s, %d)", sqlString(k.sessionID), k.seq)
	}
	rows, err := db.sql.Query(`SELECT m.session_id, m.seq, m.source, m.project, m.role, m.text, m.created_at
		FROM messages m JOIN (VALUES ` + strings.Join(vals, ",") + `) v(session_id, seq) USING (session_id, seq)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out[msgKey{m.SessionID, m.Seq}] = m
	}
	return out, rows.Err()
}

func scanMessage(rows *sql.Rows) (Message, error) {
	var m Message
	var at sql.NullTime
	err := rows.Scan(&m.SessionID, &m.Seq, &m.Source, &m.Project, &m.Role, &m.Text, &at)
	if at.Valid {
		m.CreatedAt = at.Time
	}
	return m, err
}

// SessionResult is one session with its best matching messages, in rank order.
type SessionResult struct {
	Session  Session
	Title    string // first real user prompt, whole text
	Messages int
	LastAt   time.Time
	Hits     []Hit
	ViaGraph bool // found only through graph neighbours of the query
}

// Recall groups ranked message hits by session, so one call answers
// "which conversations, when, about what, and where exactly". Sessions
// matching the query's own words come first, in rank order. If they
// leave slots open, sessions found through the query's topic-graph
// neighbours fill them, marked ViaGraph. It also returns those neighbours.
func (db *DB) Recall(query, project, source string, sessions, perSession int) ([]SessionResult, []Topic, error) {
	if sessions < 1 || perSession < 1 {
		return nil, nil, fmt.Errorf("limit must be positive")
	}
	terms, err := db.queryTerms(query)
	if err != nil {
		return nil, nil, err
	}
	related, err := db.graphExpand(terms, project, expansionTerms)
	if err != nil {
		return nil, nil, err
	}
	fetch := sessions * perSession * 4
	hits, err := db.rank(query, terms, nil, project, source, fetch)
	if err != nil {
		return nil, nil, err
	}
	var out []SessionResult
	index := map[string]int{}
	add := func(hits []Hit, viaGraph bool) {
		for _, h := range hits {
			i, ok := index[h.SessionID]
			if !ok {
				if len(out) == sessions {
					continue
				}
				i = len(out)
				index[h.SessionID] = i
				out = append(out, SessionResult{Session: Session{ID: h.SessionID}, ViaGraph: viaGraph})
			} else if out[i].ViaGraph != viaGraph {
				continue
			}
			// Plans pasted back as prompts repeat whole messages; show one copy.
			if len(out[i].Hits) < perSession && !slices.ContainsFunc(out[i].Hits, func(o Hit) bool { return o.Snippet == h.Snippet }) {
				out[i].Hits = append(out[i].Hits, h)
			}
		}
	}
	add(hits, false)
	// Widen only once the query's own matches are exhausted, so a graph
	// session never stands in for a direct match past the fetch window.
	if len(out) < sessions && len(hits) < fetch && len(related) > 0 {
		more, err := db.rank(query, terms, related, project, source, fetch)
		if err != nil {
			return nil, nil, err
		}
		add(more, true)
	}
	for i := range out {
		if err := db.describeSession(&out[i]); err != nil {
			return nil, nil, err
		}
	}
	return out, related, nil
}

// titleFilter skips injected context blocks (<environment_context>,
// <command-name>, AGENTS.md dumps) that precede the real first prompt.
const titleFilter = `role = 'user' AND NOT starts_with(ltrim(text), '<') AND NOT starts_with(ltrim(text), '# AGENTS.md')`

func (db *DB) describeSession(r *SessionResult) error {
	var started, last sql.NullTime
	var title sql.NullString
	err := db.sql.QueryRow(`SELECT s.source, s.project, s.path, s.started_at,
		(SELECT count(*) FROM messages m WHERE m.session_id = s.session_id),
		(SELECT max(created_at) FROM messages m WHERE m.session_id = s.session_id),
		(SELECT text FROM messages m WHERE m.session_id = s.session_id AND `+titleFilter+` ORDER BY seq LIMIT 1)
		FROM sessions s WHERE s.session_id = ?`, r.Session.ID).
		Scan(&r.Session.Source, &r.Session.Project, &r.Session.Path, &started, &r.Messages, &last, &title)
	if err != nil {
		return err
	}
	if started.Valid {
		r.Session.StartedAt = started.Time
	}
	if last.Valid {
		r.LastAt = last.Time
	}
	r.Title = title.String
	return nil
}

// ResolveSession finds a session by full ID or unique ID prefix.
func (db *DB) ResolveSession(ref string) (SessionResult, error) {
	rows, err := db.sql.Query(`SELECT session_id FROM sessions WHERE starts_with(session_id, ?) ORDER BY session_id LIMIT 6`, ref)
	if err != nil {
		return SessionResult{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return SessionResult{}, err
		}
		if id == ref {
			ids = []string{id}
			break
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return SessionResult{}, err
	}
	switch {
	case len(ids) == 0:
		return SessionResult{}, fmt.Errorf("no session matches %q", ref)
	case len(ids) > 1:
		return SessionResult{}, fmt.Errorf("session prefix %q is ambiguous: %s", ref, strings.Join(ids, ", "))
	}
	r := SessionResult{Session: Session{ID: ids[0]}}
	return r, db.describeSession(&r)
}

// Window returns a session's messages with from <= seq <= to, in order.
func (db *DB) Window(sessionID string, from, to int) ([]Message, error) {
	rows, err := db.sql.Query(`SELECT session_id, seq, source, project, role, text, created_at
		FROM messages WHERE session_id = ? AND seq BETWEEN ? AND ? ORDER BY seq`, sessionID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

type wordSpan struct {
	start, end int
	stem       int // index into the query stems, -1 when no match
}

// Snippet returns about width bytes of text, whitespace collapsed, placed
// where the most distinct query stems appear close together.
func Snippet(text string, stems []string, width int) string {
	var words []wordSpan
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		w := strings.ToLower(text[start:end])
		idx := -1
		for i, s := range stems {
			if stemMatches(w, s) {
				idx = i
				break
			}
		}
		if idx >= 0 {
			words = append(words, wordSpan{start, end, idx})
		}
		start = -1
	}
	for i, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			if start < 0 {
				start = i
			}
		} else {
			flush(i)
		}
	}
	flush(len(text))

	from := 0
	if len(words) > 0 {
		best, bestDistinct, bestCount := 0, 0, 0
		for i := range words {
			seen := map[int]bool{}
			count := 0
			for j := i; j < len(words) && words[j].end-words[i].start <= width*3/4; j++ {
				seen[words[j].stem] = true
				count++
			}
			if len(seen) > bestDistinct || (len(seen) == bestDistinct && count > bestCount) {
				best, bestDistinct, bestCount = i, len(seen), count
			}
		}
		from = words[best].start - width/5
		if from < 0 {
			from = 0
		}
	}
	if from > 0 {
		if sp := strings.LastIndexAny(text[max(0, from-20):from], " \n\t"); sp >= 0 {
			from = max(0, from-20) + sp + 1
		}
		for from < len(text) && !utf8.RuneStart(text[from]) {
			from++
		}
	}
	to := from + width
	if to >= len(text) {
		to = len(text)
	} else {
		for to > from && !utf8.RuneStart(text[to]) {
			to--
		}
	}
	out := strings.Join(strings.Fields(text[from:to]), " ")
	if from > 0 {
		out = "…" + out
	}
	if to < len(text) {
		out += "…"
	}
	return out
}

// stemMatches reports whether a surface word is a form of a Porter stem.
// Stems are usually word prefixes ("disengag"); some end in a rewritten
// letter ("enemi" for enemy), so long stems may differ in their last byte.
func stemMatches(word, stem string) bool {
	if strings.HasPrefix(word, stem) {
		return len(stem) >= 4 || len(word) <= len(stem)+1
	}
	return len(stem) >= 5 && strings.HasPrefix(word, stem[:len(stem)-1])
}
