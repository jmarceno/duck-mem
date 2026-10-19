// Package project normalizes project values (the directory or short name a
// session is filed under), decides whether two of them are the same git
// repository, and scores how close two names are for advisory suggestions.
package project

import (
	"os"
	"path"
	"strings"
)

// Clean returns the canonical form of a project value: trimmed, ~ expanded,
// backslashes converted to slashes, and path-cleaned so a trailing separator
// or a repeated separator cannot create a second project value. Casing is
// left alone: matching folds case, storage keeps what the source reported.
func Clean(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			p = home + p[1:]
		}
	}
	// Harnesses that run on Windows report backslashes; one store keeps one
	// shape. Project values are paths, so a literal backslash is vanishingly
	// unlikely, and the cross-platform input is worth it.
	p = strings.ReplaceAll(p, `\`, "/")
	unc := strings.HasPrefix(p, "//")
	cleaned := path.Clean(p)
	if unc && !strings.HasPrefix(cleaned, "//") {
		cleaned = "/" + cleaned
	}
	return cleaned
}

// Fold lowercases a project value for case-insensitive comparison.
func Fold(p string) string { return strings.ToLower(p) }

// BaseName returns the last element of a project value: the directory name
// for a path, or the value itself for a short name. Empty values, filesystem
// roots and cursor slugs with nothing after the colon return "".
func BaseName(p string) string {
	p = Clean(p)
	if p == "" {
		return ""
	}
	base := path.Base(p)
	if base == "/" || base == "." || base == ".." {
		return ""
	}
	return base
}

// MinNameScore is the similarity a suggestion needs to be worth printing.
const MinNameScore = 0.8

// NameScore compares two project names for the did-you-mean ranking. It
// scores case-folded basenames with Jaro-Winkler, which is prefix-weighted,
// and also credits a shared prefix of five or more characters: the full
// paths share /home/<user>/Projects/, so edit distance over them says
// nothing about the names.
func NameScore(a, b string) float64 {
	a, b = strings.ToLower(BaseName(a)), strings.ToLower(BaseName(b))
	if a == "" || b == "" {
		return 0
	}
	score := JaroWinkler(a, b)
	if n := sharedPrefix(a, b); n >= 5 {
		if p := MinNameScore + 0.04*float64(n-5); p > score {
			score = p
		}
	}
	return score
}

// JaroWinkler returns the Jaro-Winkler similarity of two strings, with the
// usual prefix scale of 0.1 capped at four characters.
func JaroWinkler(a, b string) float64 {
	ar, br := []rune(a), []rune(b)
	j := jaro(ar, br)
	prefix := 0
	for prefix < len(ar) && prefix < len(br) && prefix < 4 && ar[prefix] == br[prefix] {
		prefix++
	}
	return j + float64(prefix)*0.1*(1-j)
}

func jaro(a, b []rune) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	window := max(len(a), len(b))/2 - 1
	if window < 0 {
		window = 0
	}
	aMatch := make([]bool, len(a))
	bMatch := make([]bool, len(b))
	matches := 0
	for i, r := range a {
		lo := max(0, i-window)
		hi := min(i+window+1, len(b))
		for j := lo; j < hi; j++ {
			if bMatch[j] || b[j] != r {
				continue
			}
			aMatch[i], bMatch[j] = true, true
			matches++
			break
		}
	}
	if matches == 0 {
		return 0
	}
	transpositions := 0
	j := 0
	for i := range a {
		if !aMatch[i] {
			continue
		}
		for !bMatch[j] {
			j++
		}
		if a[i] != b[j] {
			transpositions++
		}
		j++
	}
	m := float64(matches)
	return (m/float64(len(a)) + m/float64(len(b)) + (m-float64(transpositions)/2)/m) / 3
}

func sharedPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}
