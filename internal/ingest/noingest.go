package ingest

import (
	"strings"

	"github.com/jmarceno/duck-mem/internal/store"
)

// NoIngestTags are the words that keep a session out of memory when one
// opens its first user prompt. The match is the first word only, so
// "private the launch plans" blocks while "privacy of the endpoint" does not.
var NoIngestTags = []string{"private", "noingest", "noduck"}

// tagCutset trims the punctuation a prompt can glue to its first word.
const tagCutset = ",:;.!?\\\"'`#()[]{}<>"

// ExclusionTag returns the tag opening the session's first user message,
// case-insensitive and ignoring surrounding punctuation, or "" when the
// prompt carries none.
func ExclusionTag(msgs []store.Message) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		words := strings.Fields(m.Text)
		if len(words) == 0 {
			return ""
		}
		word := strings.ToLower(strings.Trim(words[0], tagCutset))
		for _, tag := range NoIngestTags {
			if word == tag {
				return tag
			}
		}
		return ""
	}
	return ""
}
