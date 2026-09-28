package store

import (
	"sort"
	"strconv"
	"strings"

	"github.com/jmarceno/duck-mem/internal/topics"
)

// stemDocs rewrites each doc's terms and typed endpoints to Porter stems,
// the stemmer behind the BM25 posting list, so graph nodes and query terms
// meet ("enemies" and "enemy" are one node). It returns a display label
// per stem: its most frequent surface word in this batch.
func (db *DB) stemDocs(docs []topics.Doc) (map[string]string, error) {
	freq := map[string]int{}
	for _, d := range docs {
		for _, w := range d.Terms {
			freq[w]++
		}
		for _, t := range d.Typed {
			freq[t.From] += 0
			freq[t.To] += 0
		}
	}
	words := make([]string, 0, len(freq))
	for w := range freq {
		words = append(words, w)
	}
	stems, err := db.stemWords(words)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	for w, n := range freq {
		s := stems[w]
		l, ok := labels[s]
		if !ok || n > freq[l] || (n == freq[l] && (len(w) < len(l) || (len(w) == len(l) && w < l))) {
			labels[s] = w
		}
	}
	for i := range docs {
		d := &docs[i]
		seen := map[string]bool{}
		terms := d.Terms[:0]
		for _, w := range d.Terms {
			if s := stems[w]; !seen[s] {
				seen[s] = true
				terms = append(terms, s)
			}
		}
		d.Terms = terms
		typed := d.Typed[:0]
		for _, t := range d.Typed {
			t.From, t.To = stems[t.From], stems[t.To]
			if t.From == t.To {
				continue
			}
			t.A, t.B = t.From, t.To
			if t.A > t.B {
				t.A, t.B = t.B, t.A
			}
			typed = append(typed, t)
		}
		d.Typed = typed
	}
	return labels, nil
}

// stemWords maps words to DuckDB's Porter stems. Words never contain
// newlines (the tokenizers split on them), so one joined parameter carries
// the whole batch.
func (db *DB) stemWords(words []string) (map[string]string, error) {
	out := make(map[string]string, len(words))
	if len(words) == 0 {
		return out, nil
	}
	rows, err := db.sql.Query(`SELECT w, stem(w, 'porter') FROM (SELECT unnest(string_split(?, chr(10))) AS w)`,
		strings.Join(words, "\n"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var w, s string
		if err := rows.Scan(&w, &s); err != nil {
			return nil, err
		}
		out[w] = s
	}
	return out, rows.Err()
}

// topicLabels returns the display word for each stemmed term that has one.
func (db *DB) topicLabels(terms []string) (map[string]string, error) {
	out := map[string]string{}
	if len(terms) == 0 {
		return out, nil
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = sqlString(t)
	}
	rows, err := db.sql.Query(`SELECT term, max(label) FROM topics
		WHERE term IN (` + strings.Join(quoted, ",") + `) AND label <> '' GROUP BY term`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t, l string
		if err := rows.Scan(&t, &l); err != nil {
			return nil, err
		}
		out[t] = l
	}
	return out, rows.Err()
}

// Topic is a graph neighbour of a query. Links counts the query terms it
// is connected to; Weight sums those edges; Lift sums weight divided by
// the neighbour's message count, so specific associates beat common words.
type Topic struct {
	Term   string // stem, as stored in the graph and the posting list
	Label  string // surface word to show
	Links  int
	Weight int
	Lift   float64
}

// graphExpand walks one duckpgq hop from the query stems over co-mention
// edges and returns the most specific neighbours: first those linked to
// more query terms, then by lift. Ranking by raw weight instead picks
// words that co-occur with everything ("plan", "implementation").
// Recurring edges (weight >= 2) are used when the query terms have any.
// Three quarters of edges are one-off co-mentions, and for a rare word
// they are all there is ("disengage": passive, roam, sight), so those are
// the fallback. Query stems themselves are excluded.
func (db *DB) graphExpand(stems []string, project string, n int) ([]Topic, error) {
	if len(stems) == 0 || n < 1 {
		return nil, nil
	}
	out, err := db.graphNeighbours(stems, project, n, 2)
	if err != nil || len(out) > 0 {
		return out, err
	}
	return db.graphNeighbours(stems, project, n, 1)
}

func (db *DB) graphNeighbours(stems []string, project string, n, minWeight int) ([]Topic, error) {
	quoted := make([]string, len(stems))
	for i, s := range stems {
		quoted[i] = sqlString(s)
	}
	in := strings.Join(quoted, ",")
	q := `SELECT g.term, count(DISTINCT g.src)::INTEGER, sum(g.weight)::INTEGER,
		sum(g.weight::DOUBLE / greatest(d.msgs, 1)) AS lift FROM (
		SELECT * FROM GRAPH_TABLE (topic_graph
			MATCH (a:topics)-[e:Rel]->(b:topics)
			COLUMNS (a.term AS src, a.project AS project, b.term AS term, e.weight AS weight, e.kind AS kind)
		)
	) g JOIN term_df d ON d.project = g.project AND d.term = g.term
	WHERE g.src IN (` + in + `) AND g.term NOT IN (` + in + `)
		AND g.kind = 'co-mention' AND g.weight >= ` + strconv.Itoa(minWeight)
	if project != "" {
		q += ` AND contains(lower(g.project), lower(` + sqlString(project) + `))`
	}
	q += ` GROUP BY g.term ORDER BY 2 DESC, 4 DESC, 1 LIMIT ` + strconv.Itoa(n)
	rows, err := db.sql.Query(q)
	if err != nil {
		return nil, err
	}
	var out []Topic
	for rows.Next() {
		var t Topic
		if err := rows.Scan(&t.Term, &t.Links, &t.Weight, &t.Lift); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	terms := make([]string, len(out))
	for i, t := range out {
		terms[i] = t.Term
	}
	labels, err := db.topicLabels(terms)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Label = labelOr(labels, out[i].Term)
	}
	return out, nil
}

func labelOr(labels map[string]string, term string) string {
	if l := labels[term]; l != "" {
		return l
	}
	return term
}

// labelHits swaps stems for display words in related results.
func (db *DB) labelHits(hits []RelatedHit) error {
	set := map[string]bool{}
	for _, h := range hits {
		for _, t := range []string{h.Term, h.Via, h.From, h.To} {
			if t != "" {
				set[t] = true
			}
		}
	}
	terms := make([]string, 0, len(set))
	for t := range set {
		terms = append(terms, t)
	}
	sort.Strings(terms)
	labels, err := db.topicLabels(terms)
	if err != nil {
		return err
	}
	for i := range hits {
		h := &hits[i]
		h.Term = labelOr(labels, h.Term)
		if h.Via != "" {
			h.Via = labelOr(labels, h.Via)
		}
		if h.From != "" {
			h.From = labelOr(labels, h.From)
		}
		if h.To != "" {
			h.To = labelOr(labels, h.To)
		}
	}
	return nil
}
