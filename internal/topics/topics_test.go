package topics

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestTermsKeepsContentWords(t *testing.T) {
	got := Terms("Bastion's Null Sentry turret fires at the nearest enemy!")
	for _, want := range []string{"bastion", "null", "sentry", "turret", "fires", "nearest", "enemy"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	for _, drop := range []string{"the", "at", "s"} {
		if slices.Contains(got, drop) {
			t.Errorf("glue %q kept in %v", drop, got)
		}
	}
}

func TestTermsDropsGlueVerbs(t *testing.T) {
	got := Terms("check the code running against dev server")
	if len(got) != 0 {
		t.Errorf("glue verbs kept: %v", got)
	}
}

func TestBuildEdgesCountsCoMentions(t *testing.T) {
	texts := []string{
		"bastion deploys null sentry turret",
		"bastion sanctuary replaced by sentry",
		"unrelated ships trading",
	}
	edges := BuildEdges(texts, 1)
	if edges[Edge{"bastion", "sentry"}] != 2 {
		t.Errorf("bastion-sentry weight = %d, want 2", edges[Edge{"bastion", "sentry"}])
	}
	if edges[Edge{"sanctuary", "sentry"}] != 1 {
		t.Errorf("sanctuary-sentry weight = %d, want 1", edges[Edge{"sanctuary", "sentry"}])
	}
	if _, ok := edges[Edge{"bastion", "ships"}]; ok {
		t.Error("cross-message pair bastion-ships should not exist")
	}
}

func TestSelectPairsKeepsTopDistinctive(t *testing.T) {
	df := map[string]int{}
	var words []string
	for i := 1; i <= 16; i++ {
		w := fmt.Sprintf("t%02d", i)
		df[w] = i
		words = append(words, w)
	}
	pairs := SelectPairs(strings.Join(words, " "), df, 1, 100)
	if len(pairs) != 66 { // C(12,2): top 12 of 16
		t.Fatalf("got %d pairs, want 66", len(pairs))
	}
	seen := map[string]bool{}
	for _, e := range pairs {
		seen[e[0]] = true
		seen[e[1]] = true
	}
	if !seen["t01"] || !seen["t12"] {
		t.Errorf("distinctive terms missing from pairs: %v", pairs)
	}
	for _, w := range []string{"t13", "t14", "t15", "t16"} {
		if seen[w] {
			t.Errorf("common term %s kept in narrow pairs", w)
		}
	}
}

func TestBuildEdgesDropsRareTerms(t *testing.T) {
	texts := []string{"bastion sentry", "bastion sentry turret"}
	edges := BuildEdges(texts, 2)
	if _, ok := edges[Edge{"sentry", "turret"}]; ok {
		t.Error("turret (df=1) should be dropped with minDF=2")
	}
	if edges[Edge{"bastion", "sentry"}] != 2 {
		t.Errorf("bastion-sentry weight = %d, want 2", edges[Edge{"bastion", "sentry"}])
	}
}
