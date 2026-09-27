// Package topics derives topic relationships from stored messages.
//
// Iteration: co-mention graph. Two terms are related when they appear in
// the same message; the edge weight is the number of messages where they
// co-occur. This is lexical, not semantic: "bastion-sentry" means the two
// are discussed together, not that one owns the other. Semantic relations
// (owns, replaces, fixes) are future work on top of this table, e.g. via
// duckpgq traversals or an extractor model.
package topics

import (
	"slices"
	"sort"
	"strings"
	"unicode"
)

// stopwords drop glue words that would otherwise dominate the graph.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true,
	"this": true, "from": true, "have": true, "has": true, "had": true,
	"were": true, "was": true, "are": true, "you": true, "your": true,
	"will": true, "would": true, "should": true, "could": true, "can": true,
	"not": true, "but": true, "all": true, "any": true, "they": true,
	"them": true, "their": true, "there": true, "here": true, "when": true,
	"what": true, "which": true, "who": true, "how": true, "why": true,
	"about": true, "into": true, "over": true, "after": true, "before": true,
	"then": true, "than": true, "also": true, "just": true, "like": true,
	"more": true, "most": true, "some": true, "such": true, "only": true,
	"its": true, "our": true, "out": true, "now": true, "new": true,
	"use": true, "used": true, "using": true, "get": true, "got": true,
	"make": true, "made": true, "let": true, "per": true, "via": true,
	"one": true, "two": true, "way": true, "well": true, "much": true,
	"many": true, "does": true, "doing": true, "done": true, "same": true,
	"still": true, "even": true, "back": true, "being": true, "been": true,
	"because": true, "while": true, "where": true, "each": true, "other": true,
	"another": true, "both": true, "few": true, "between": true, "during": true,
	"without": true, "within": true, "along": true, "across": true, "off": true,
	"against": true, "among": true, "around": true,
	"too": true, "very": true, "really": true, "quite": true, "rather": true,
	"either": true, "neither": true, "ever": true, "never": true, "always": true,
	"often": true, "else": true, "though": true, "although": true, "however": true,
	"therefore": true, "thus": true, "hence": true, "plus": true, "whether": true,
	"things": true, "something": true, "anything": true, "everything": true,
	"someone": true, "maybe": true, "please": true, "thanks": true, "hello": true,
	"yeah": true, "yes": true, "okay": true,
	"need": true, "needs": true, "want": true, "wants": true, "look": true,
	"looks": true, "see": true, "seems": true, "seem": true, "show": true,
	"shows": true, "find": true, "found": true, "check": true, "checking": true,
	"try": true, "tried": true, "sure": true, "thing": true, "stuff": true,
	"bit": true, "lot": true, "able": true, "going": true, "come": true,
	"take": true, "takes": true, "give": true, "put": true, "set": true,
	"run": true, "runs": true, "running": true, "work": true, "works": true,
	"working": true, "test": true, "tests": true, "tested": true, "testing": true,
	"code": true, "file": true, "files": true, "change": true, "changes": true,
	"changed": true, "update": true, "updated": true, "add": true, "added": true,
	"remove": true, "removed": true, "create": true, "created": true, "fix": true,
	"fixed": true, "issue": true, "error": true, "errors": true, "fail": true,
	"failed": true, "pass": true, "case": true, "part": true, "end": true,
	"start": true, "started": true, "game": true, "dev": true, "team": true,
	"server": true, "client": true, "app": true, "feature": true, "system": true,
}

// MaxDFRatio drops terms appearing in more than this fraction of messages:
// hub words ("turn", "user", "agent") connect everything to everything.
const MaxDFRatio = 0.25

// Terms extracts ordered unique content terms from text.
func Terms(text string) []string {
	var raw []string
	var cur strings.Builder
	flush := func() {
		w := cur.String()
		cur.Reset()
		if len(w) < 3 || stopwords[w] {
			return
		}
		if isDigits(w) {
			return
		}
		raw = append(raw, w)
	}
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	seen := map[string]bool{}
	out := []string{}
	for _, w := range raw {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

func isDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// Edge is an unordered term pair; A < B canonicalized.
type Edge = [2]string

func canonical(a, b string) Edge {
	if a > b {
		a, b = b, a
	}
	return Edge{a, b}
}

// TopTermsPerMessage bounds pair generation: only the most distinctive
// terms per message become pairs (12 terms -> at most 66 pairs), so edges
// are created narrowly instead of combinatorially. Too narrow buries
// mid-frequency topic words under slightly rarer filler in long messages.
const TopTermsPerMessage = 12

// HubCap returns the df above which a term counts as a hub for a corpus
// of total messages. Never below minDF (tiny corpora skip the cap).
func HubCap(total, minDF int) int {
	if maxDF := int(float64(total) * MaxDFRatio); maxDF >= minDF {
		return maxDF
	}
	return total
}

// DocFreq counts per-message term frequency over a corpus.
func DocFreq(texts []string) map[string]int {
	df := map[string]int{}
	for _, t := range texts {
		seen := map[string]bool{}
		for _, w := range Terms(t) {
			if !seen[w] {
				seen[w] = true
				df[w]++
			}
		}
	}
	return df
}

// SelectTop keeps qualifying terms, distinctive-first (df ascending,
// alphabetical tiebreak), capped at top.
func SelectTop(terms []string, df map[string]int, minDF, maxDF, top int) []string {
	var cand []string
	for _, w := range terms {
		if df[w] >= minDF && df[w] <= maxDF && !slices.Contains(cand, w) {
			cand = append(cand, w)
		}
	}
	sort.Slice(cand, func(i, j int) bool {
		if df[cand[i]] != df[cand[j]] {
			return df[cand[i]] < df[cand[j]]
		}
		return cand[i] < cand[j]
	})
	if len(cand) > top {
		cand = cand[:top]
	}
	return cand
}

// Pairs lists every unordered pair of kept terms.
func Pairs(kept []string) []Edge {
	var out []Edge
	for i := 0; i < len(kept); i++ {
		for j := i + 1; j < len(kept); j++ {
			out = append(out, canonical(kept[i], kept[j]))
		}
	}
	return out
}

// Relation kinds produced by rule extraction.
const (
	KindOwns     = "owns"
	KindReplaces = "replaces"
)

// TypedPair is one rule-extracted relation. A/B are canonical for graph
// lookup; From/To preserve the direction stated by the source sentence.
type TypedPair struct {
	A, B     string
	From, To string
	Kind     string
	Evidence string
}

// replaceVerbs mark a replacement relationship between the nearest
// content words on each side.
var replaceVerbs = map[string]bool{
	"replace": true, "replaces": true, "replaced": true, "replacing": true,
	"supersede": true, "supersedes": true, "superseded": true, "superseding": true,
}

func isContent(w string) bool {
	return len(w) >= 3 && !stopwords[w] && !isDigits(w)
}

// contentTokens lowercases and splits text, keeping order and
// duplicates (unlike Terms): extraction needs positions.
func contentTokens(text string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if w := cur.String(); w != "" {
			out = append(out, w)
		}
		cur.Reset()
	}
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else if r == '\'' {
			flush()
			out = append(out, "'")
		} else {
			flush()
		}
	}
	flush()
	return out
}

// ExtractTyped finds rule-based relations in text, sentence by sentence:
// possessive "X's Y" -> owns, replacement verbs and "instead of" ->
// replaces. Both endpoints must be content words.
func ExtractTyped(text string) []TypedPair {
	var out []TypedPair
	for _, sent := range strings.FieldsFunc(text, func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == ';' || r == '\n'
	}) {
		out = append(out, extractSent(contentTokens(sent), strings.TrimSpace(sent))...)
	}
	return out
}

func typedPair(from, to, kind, evidence string) TypedPair {
	a, b := from, to
	if a > b {
		a, b = b, a
	}
	runes := []rune(evidence)
	if len(runes) > 500 {
		evidence = string(runes[:500]) + "…"
	}
	return TypedPair{A: a, B: b, From: from, To: to, Kind: kind, Evidence: evidence}
}

func extractSent(toks []string, evidence string) []TypedPair {
	var out []TypedPair
	// Possessive: X ' s Y.
	for i := 0; i+3 < len(toks); i++ {
		if toks[i+1] == "'" && toks[i+2] == "s" && isContent(toks[i]) && isContent(toks[i+3]) {
			out = append(out, typedPair(toks[i], toks[i+3], KindOwns, evidence))
		}
	}
	// Replacement verbs: nearest content word each side.
	isInsteadOf := func(i int) bool {
		return toks[i] == "instead" && i+1 < len(toks) && toks[i+1] == "of"
	}
	for i, t := range toks {
		verb := replaceVerbs[t]
		if !verb && !(t == "instead" && isInsteadOf(i)) {
			continue
		}
		after := i + 1
		if t == "instead" {
			after = i + 2 // skip "of"
		}
		subj, obj := "", ""
		for j := i - 1; j >= 0; j-- {
			if isContent(toks[j]) {
				subj = toks[j]
				break
			}
		}
		for j := after; j < len(toks); j++ {
			if isContent(toks[j]) {
				obj = toks[j]
				break
			}
		}
		if subj == "" || obj == "" || subj == obj {
			continue
		}
		if (t == "replaced" || t == "superseded") && after < len(toks) && toks[after] == "by" {
			subj, obj = obj, subj
		}
		out = append(out, typedPair(subj, obj, KindReplaces, evidence))
	}
	return out
}

// SelectPairs extracts the narrow pair set for one message against
// corpus df counts: top distinctive qualifying terms, paired.
func SelectPairs(text string, df map[string]int, minDF, maxDF int) []Edge {
	return Pairs(SelectTop(Terms(text), df, minDF, maxDF, TopTermsPerMessage))
}

// BuildEdges counts co-mentions across message texts (batch path used by
// full rebuilds). Terms below minDF are noise, above the hub cap are hubs.
func BuildEdges(texts []string, minDF int) map[Edge]int {
	df := DocFreq(texts)
	maxDF := HubCap(len(texts), minDF)
	edges := map[Edge]int{}
	for _, t := range texts {
		for _, e := range SelectPairs(t, df, minDF, maxDF) {
			edges[e]++
		}
	}
	return edges
}

// TopTerms returns the highest-df terms, useful for sanity checks.
func TopTerms(texts []string, n int) []string {
	df := map[string]int{}
	for _, t := range texts {
		seen := map[string]bool{}
		for _, w := range Terms(t) {
			if !seen[w] {
				seen[w] = true
				df[w]++
			}
		}
	}
	type kv struct {
		k string
		v int
	}
	var all []kv
	for k, v := range df {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
	var out []string
	for i := 0; i < n && i < len(all); i++ {
		out = append(out, all[i].k)
	}
	return out
}
