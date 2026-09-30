// Package embed builds the fixed-width vectors stored for similarity search.
//
// Each message becomes one unit vector: content terms and adjacent term
// pairs are feature-hashed into Dim floats. Cosine distance then ranks
// messages that share wording and phrases. There is no keyword scan.
package embed

import (
	"hash"
	"hash/fnv"
	"math"

	"github.com/jmarceno/duck-mem/internal/topics"
)

// Dim is the FLOAT[N] width of every stored embedding.
const Dim = 384

// Embed returns an L2-normalized vector, or nil when text has no content terms.
func Embed(text string) []float32 {
	return EmbedTokens(topics.Tokenize(text))
}

// EmbedTokens builds the vector from content tokens already produced by topics.Analyze.
func EmbedTokens(toks []string) []float32 {
	if len(toks) == 0 {
		return nil
	}
	v := make([]float32, Dim)
	h := fnv.New64a()
	seen := map[string]struct{}{}
	for _, t := range toks {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		add(h, v, "u:", t, "")
	}
	seenBi := map[[2]string]struct{}{}
	for i := 1; i < len(toks); i++ {
		if toks[i] == toks[i-1] {
			continue
		}
		key := [2]string{toks[i-1], toks[i]}
		if _, ok := seenBi[key]; ok {
			continue
		}
		seenBi[key] = struct{}{}
		add(h, v, "b:", key[0], key[1])
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return nil
	}
	norm := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= norm
	}
	return v
}

func add(h hash.Hash64, v []float32, prefix, a, b string) {
	h.Reset()
	_, _ = h.Write([]byte(prefix))
	_, _ = h.Write([]byte(a))
	if b != "" {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(b))
	}
	sum := h.Sum64()
	idx := int(sum % uint64(Dim))
	if sum&1 == 0 {
		v[idx]++
	} else {
		v[idx]--
	}
}
