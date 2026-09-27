// Package embed builds the fixed-width vectors stored for vss search.
//
// Each message becomes one unit vector: content terms and adjacent term
// pairs are feature-hashed into Dim floats. Cosine distance then ranks
// messages that share wording and phrases. There is no keyword scan.
package embed

import (
	"hash/fnv"
	"math"

	"duck-mem/internal/topics"
)

// Dim is the FLOAT[N] width of every stored embedding and of the HNSW index.
const Dim = 384

// Embed returns an L2-normalized vector, or nil when text has no content terms.
func Embed(text string) []float32 {
	toks := topics.Tokenize(text)
	if len(toks) == 0 {
		return nil
	}
	v := make([]float32, Dim)
	seen := map[string]bool{}
	for _, t := range toks {
		if seen[t] {
			continue
		}
		seen[t] = true
		add(v, "u:"+t, 1)
	}
	seenBi := map[string]bool{}
	for i := 1; i < len(toks); i++ {
		if toks[i] == toks[i-1] {
			continue
		}
		bi := "b:" + toks[i-1] + "\x00" + toks[i]
		if seenBi[bi] {
			continue
		}
		seenBi[bi] = true
		add(v, bi, 1)
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

func add(v []float32, feature string, w float32) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(feature))
	sum := h.Sum64()
	idx := int(sum % uint64(Dim))
	if sum&1 == 0 {
		v[idx] += w
	} else {
		v[idx] -= w
	}
}
