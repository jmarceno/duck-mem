// Package topics derives topic relationships from stored messages.
//
// Co-mention edges record terms discussed in the same message. Typed edges
// (owns, replaces) come from rules. Traversal of those edges is duckpgq's
// job in the store, not a second walk over the rows.
package topics

import (
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
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
	// Adverbs attach to any topic, so as graph neighbours they say nothing.
	"anymore": true, "simply": true, "certain": true, "actually": true, "basically": true,
	"probably": true, "currently": true, "already": true, "exactly": true, "properly": true,
	"instead": true, "otherwise": true, "anyway": true, "likely": true, "usually": true,
	"especially": true, "entirely": true, "completely": true, "fully": true, "mostly": true,
	"mainly": true, "clearly": true, "directly": true, "immediately": true, "correctly": true,
	"possibly": true, "possible": true, "almost": true, "enough": true, "again": true,
}

// MaxDFRatio drops terms appearing in more than this fraction of messages:
// hub words ("turn", "user", "agent") connect everything to everything.
const MaxDFRatio = 0.25

// Doc is one message scanned once: content tokens (with repeats), the unique
// terms in that order, and the typed relations.
type Doc struct {
	Tokens []string
	Terms  []string
	Typed  []TypedPair
}

// Analyze scans text once for tokens, terms, and typed relations.
func Analyze(text string) Doc {
	var d Doc
	seen := map[string]bool{}
	scanSentences(text, func(raw string) {
		toks := contentTokens(strings.ToLower(raw))
		if len(toks) == 0 {
			return
		}
		d.Typed = append(d.Typed, extractSent(toks, strings.TrimSpace(raw))...)
		for _, w := range toks {
			if !isContent(w) {
				continue
			}
			d.Tokens = append(d.Tokens, w)
			if !seen[w] {
				seen[w] = true
				d.Terms = append(d.Terms, w)
			}
		}
	})
	return d
}

// AnalyzeMany scans texts independently, preserving order.
func AnalyzeMany(texts []string) []Doc {
	out := make([]Doc, len(texts))
	if len(texts) < 64 {
		for i, t := range texts {
			out[i] = Analyze(t)
		}
		return out
	}
	workers := runtime.GOMAXPROCS(0)
	if workers > len(texts) {
		workers = len(texts)
	}
	var wg sync.WaitGroup
	jobs := make(chan int, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				out[i] = Analyze(texts[i])
			}
		}()
	}
	for i := range texts {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out
}

// Tokenize returns content tokens in order, keeping repeats so adjacent
// phrases stay visible to the embedder.
func Tokenize(text string) []string {
	return Analyze(text).Tokens
}

// Terms extracts ordered unique content terms from text.
func Terms(text string) []string {
	return Analyze(text).Terms
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
	for _, d := range AnalyzeMany(texts) {
		for _, w := range d.Terms {
			df[w]++
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

// weakEndpoints are modifiers that follow a possessive or sit next to a
// replacement verb without naming a thing: "barricade's actual shape",
// "the enemy's own materials", "game-side code to be replaced".
var weakEndpoints = map[string]bool{
	"actual": true, "own": true, "standard": true, "side": true, "first": true,
	"last": true, "next": true, "current": true, "main": true, "whole": true,
	"full": true, "real": true, "old": true, "entire": true, "overall": true,
	"best": true, "general": true, "specific": true, "proper": true, "original": true,
	"previous": true, "final": true, "little": true, "big": true, "small": true,
	"large": true, "good": true, "bad": true, "great": true, "top": true,
	"bottom": true, "left": true, "right": true, "internal": true, "external": true,
	"existing": true, "various": true, "certain": true, "several": true, "different": true,
	"similar": true, "latest": true, "earlier": true, "later": true, "single": true,
	"multiple": true, "total": true, "primary": true, "secondary": true, "simple": true,
	"basic": true, "common": true, "exact": true, "correct": true, "wrong": true,
	"second": true, "third": true, "initial": true, "default": true, "usual": true,
	"entry": true, "whatever": true, "everyone": true, "nobody": true, "somewhere": true,
}

// isEndpoint reports whether w can name one end of a typed relation.
func isEndpoint(w string) bool {
	return isContent(w) && !weakEndpoints[w]
}

func isContent(w string) bool {
	return len(w) >= 3 && !stopwords[w] && !isDigits(w)
}

// scanSentences visits each sentence slice of text. Delimiters are the same
// as ExtractTyped historically split on, and they are not part of the slice.
func scanSentences(text string, fn func(raw string)) {
	start := 0
	for i, r := range text {
		if r == '.' || r == '!' || r == '?' || r == ';' || r == '\n' {
			fn(text[start:i])
			start = i + utf8.RuneLen(r)
		}
	}
	fn(text[start:])
}

// contentTokens splits already-lowercased text, keeping order and
// duplicates (unlike Terms): extraction needs positions.
func contentTokens(text string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		out = append(out, cur.String())
		cur.Reset()
	}
	for _, r := range text {
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
// replaces. Both endpoints must be content words that name a thing.
func ExtractTyped(text string) []TypedPair {
	return Analyze(text).Typed
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
		if toks[i+1] == "'" && toks[i+2] == "s" && isEndpoint(toks[i]) && isEndpoint(toks[i+3]) {
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
		// The nearest content word decides; a weak one means no relation,
		// not a reach for a farther word.
		if !isEndpoint(subj) || !isEndpoint(obj) {
			continue
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
	return SelectPairsFromTerms(Terms(text), df, minDF, maxDF)
}

// SelectPairsFromTerms pairs an already-scanned term list.
func SelectPairsFromTerms(terms []string, df map[string]int, minDF, maxDF int) []Edge {
	return Pairs(SelectTop(terms, df, minDF, maxDF, TopTermsPerMessage))
}

// BuildEdges counts co-mentions across message texts (batch path used by
// full rebuilds). Terms below minDF are noise, above the hub cap are hubs.
func BuildEdges(texts []string, minDF int) map[Edge]int {
	docs := AnalyzeMany(texts)
	df := map[string]int{}
	for _, d := range docs {
		for _, w := range d.Terms {
			df[w]++
		}
	}
	maxDF := HubCap(len(texts), minDF)
	edges := map[Edge]int{}
	for _, d := range docs {
		for _, e := range SelectPairsFromTerms(d.Terms, df, minDF, maxDF) {
			edges[e]++
		}
	}
	return edges
}

// TopTerms returns the highest-df terms, useful for sanity checks.
func TopTerms(texts []string, n int) []string {
	df := map[string]int{}
	for _, d := range AnalyzeMany(texts) {
		for _, w := range d.Terms {
			df[w]++
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
