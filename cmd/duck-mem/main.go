// Command duck-mem ingests agent session logs into DuckDB (iteration 1:
// everything minus tool calls) and searches them (iteration 2 CLI).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"duck-mem/internal/ingest"
	"duck-mem/internal/store"
)

func defaultDB() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "duck-mem", "memory.duckdb")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "ingest":
		ingestCmd(os.Args[2:])
	case "query":
		queryCmd(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  duck-mem ingest [--db PATH] [ROOT...]   ingest session logs (default roots when omitted)
  duck-mem query [--db PATH] [--project P] [--source S] [--limit N] <text...>
`)
}

// splitArgs lets flags appear before or after positionals (Go's flag
// package stops parsing at the first positional argument). Only known
// flags are recognized; everything else is positional query/root text.
func splitArgs(args []string, known map[string]bool) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name := a
		if j := strings.IndexByte(a, '='); j >= 0 {
			name = a[:j]
		}
		if !known[name] || strings.Contains(a, "=") {
			if known[name] {
				flags = append(flags, a)
			} else {
				positional = append(positional, a)
			}
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, a, args[i+1])
			i++
		} else {
			positional = append(positional, a)
		}
	}
	return flags, positional
}

func ingestCmd(args []string) {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	dbPath := fs.String("db", defaultDB(), "DuckDB file")
	flagArgs, positional := splitArgs(args, map[string]bool{"--db": true})
	_ = fs.Parse(flagArgs)

	roots := append(fs.Args(), positional...)
	if len(roots) == 0 {
		home, _ := os.UserHomeDir()
		roots = ingest.DefaultRoots(home)
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		fatal(err)
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	files := ingest.Discover(roots)
	var nSess, nMsg, nSkip int
	for _, f := range files {
		sess, msgs, err := ingest.IngestFile(f)
		if err != nil || sess.ID == "" {
			nSkip++
			continue
		}
		if err := db.UpsertSession(sess); err != nil {
			nSkip++
			continue
		}
		if err := db.InsertMessages(msgs); err != nil {
			nSkip++
			continue
		}
		nSess++
		nMsg += len(msgs)
	}
	fmt.Printf("files=%d sessions=%d messages=%d skipped=%d db=%s\n", len(files), nSess, nMsg, nSkip, *dbPath)
}

func queryCmd(args []string) {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	dbPath := fs.String("db", defaultDB(), "DuckDB file")
	project := fs.String("project", "", "filter by project (substring match)")
	source := fs.String("source", "", "filter by source: codex|claude|cursor|muse")
	limit := fs.Int("limit", 20, "max results")
	known := map[string]bool{"--db": true, "--project": true, "--source": true, "--limit": true}
	flagArgs, positional := splitArgs(args, known)
	_ = fs.Parse(flagArgs)
	fs.Parse(positional)
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "query needs search text")
		os.Exit(2)
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	q := joinArgs(fs.Args())
	hits, err := db.Search(q, *project, *source, *limit)
	if err != nil {
		fatal(err)
	}
	for _, h := range hits {
		fmt.Printf("[%s] %s %s#%d (%s): %s\n", h.Source, h.Project, h.SessionID, h.Seq, h.Role, oneLine(h.Snippet))
	}
}

func joinArgs(a []string) string {
	out := ""
	for i, s := range a {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}

func oneLine(s string) string {
	out := ""
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			out += " "
		} else {
			out += string(r)
		}
		if len(out) > 240 {
			return out + "…"
		}
	}
	return out
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
