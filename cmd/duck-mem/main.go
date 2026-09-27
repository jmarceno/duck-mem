// Command duck-mem ingests agent session logs into DuckDB (iteration 1:
// everything minus tool calls) and searches them (iteration 2 CLI).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
	case "index":
		indexCmd(os.Args[2:])
	case "related":
		relatedCmd(os.Args[2:])
	case "daemon":
		daemonCmd(os.Args[2:])
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
  duck-mem index [--db PATH] [--min-df N] rebuild the topic co-mention graph
  duck-mem related [--db PATH] [--depth 1|2] [--limit N] <term>
  duck-mem daemon [--db PATH] [--interval 5m] [ROOT...]
`)
}

// splitArgs lets flags appear before or after positionals (Go's flag
// package stops parsing at the first positional argument). The map marks
// which known flags take a value (false = boolean switch); everything
// else is positional query/root text.
func splitArgs(args []string, takesValue map[string]bool) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name := a
		if j := strings.IndexByte(a, '='); j >= 0 {
			name = a[:j]
		}
		takes, known := takesValue[name]
		if !known {
			positional = append(positional, a)
			continue
		}
		if !takes || strings.Contains(a, "=") {
			flags = append(flags, a)
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
	db, err := openTolerant(*dbPath, false)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	files, nSess, nMsg, nSkip := runCycle(db, roots)
	fmt.Printf("files=%d sessions=%d messages=%d skipped=%d db=%s\n", files, nSess, nMsg, nSkip, *dbPath)
	newMsgs, newPairs, err := db.IndexNew(2)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("index: +%d messages, +%d pairs\n", newMsgs, newPairs)
}

func daemonCmd(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	dbPath := fs.String("db", defaultDB(), "DuckDB file")
	every := fs.String("interval", "5m", "ingest cadence (Go duration: 30s, 5m, 1h)")
	flagArgs, positional := splitArgs(args, map[string]bool{"--db": true, "--interval": true})
	_ = fs.Parse(flagArgs)
	fs.Parse(positional)
	interval, err := time.ParseDuration(*every)
	if err != nil || interval <= 0 {
		fmt.Fprintln(os.Stderr, "daemon needs a positive --interval duration")
		os.Exit(2)
	}
	roots := fs.Args()
	if len(roots) == 0 {
		home, _ := os.UserHomeDir()
		roots = ingest.DefaultRoots(home)
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("%s daemon start db=%s interval=%s roots=%d\n",
		time.Now().UTC().Format(time.RFC3339), *dbPath, interval, len(roots))
	cycle := func(n int) {
		// Open per cycle and close right after: DuckDB allows one
		// process at a time, so holding the file would block queries
		// for the daemon's whole lifetime.
		db, err := store.Open(*dbPath)
		if err != nil {
			fmt.Printf("%s cycle=%d open error: %v\n", time.Now().UTC().Format(time.RFC3339), n, err)
			return
		}
		files, nSess, nMsg, nSkip := runCycle(db, roots)
		newMsgs, newPairs, err := db.IndexNew(2)
		_ = db.Close()
		if err != nil {
			fmt.Printf("%s cycle=%d error: %v\n", time.Now().UTC().Format(time.RFC3339), n, err)
			return
		}
		fmt.Printf("%s cycle=%d files=%d sessions=%d messages=%d skipped=%d indexed=+%d pairs=+%d\n",
			time.Now().UTC().Format(time.RFC3339), n, files, nSess, nMsg, nSkip, newMsgs, newPairs)
	}
	cycle(0)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			fmt.Printf("%s daemon stop after %d cycles\n", time.Now().UTC().Format(time.RFC3339), n)
			return
		case <-ticker.C:
			cycle(n)
		}
	}
}

// runCycle ingests every discoverable file. Inserts are idempotent and
// IndexNew skips indexed messages, so repeated cycles only pay for what
// changed (plus a directory walk).
func runCycle(db *store.DB, roots []string) (files, nSess, nMsg, nSkip int) {
	cursorProjects := map[string]map[string]string{}
	for _, f := range ingest.Discover(roots) {
		files++
		var projects map[string]string
		if ingest.Classify(f) == ingest.KindCursorTranscript {
			home := ingest.CursorHome(f)
			var ok bool
			projects, ok = cursorProjects[home]
			if !ok {
				projects = ingest.LoadCursorProjects(home)
				cursorProjects[home] = projects
			}
		}
		sess, msgs, err := ingest.IngestFileWithCursorProjects(f, projects)
		if err != nil || sess.ID == "" {
			nSkip++
			continue
		}
		if err := db.UpsertSession(sess); err != nil {
			nSkip++
			continue
		}
		if err := db.SyncMessages(sess.ID, msgs); err != nil {
			nSkip++
			continue
		}
		nSess++
		nMsg += len(msgs)
	}
	return files, nSess, nMsg, nSkip
}

func indexCmd(args []string) {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	dbPath := fs.String("db", defaultDB(), "DuckDB file")
	minDF := fs.Int("min-df", 2, "min messages a term must appear in")
	full := fs.Bool("full", false, "full rebuild: exact thresholds, heals drift (run weekly)")
	flagArgs, _ := splitArgs(args, map[string]bool{"--db": true, "--min-df": true, "--full": false})
	_ = fs.Parse(flagArgs)
	db, err := openTolerant(*dbPath, false)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	if *full {
		if err := db.IndexFull(*minDF); err != nil {
			fatal(err)
		}
		fmt.Println("topic graph rebuilt (full)")
		return
	}
	newMsgs, newPairs, err := db.IndexNew(*minDF)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("index: +%d messages, +%d pairs\n", newMsgs, newPairs)
}

func relatedCmd(args []string) {
	fs := flag.NewFlagSet("related", flag.ExitOnError)
	dbPath := fs.String("db", defaultDB(), "DuckDB file")
	project := fs.String("project", "", "restrict to project (substring match)")
	depth := fs.Int("depth", 1, "1: direct co-mentions, 2: include neighbors-of-neighbors")
	limit := fs.Int("limit", 20, "max topics")
	known := map[string]bool{"--db": true, "--project": true, "--depth": true, "--limit": true}
	flagArgs, positional := splitArgs(args, known)
	_ = fs.Parse(flagArgs)
	fs.Parse(positional)
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "related needs a term")
		os.Exit(2)
	}
	db, err := openTolerant(*dbPath, true)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	hits, err := db.Related(joinArgs(fs.Args()), *project, *depth, *limit)
	if err != nil {
		fatal(err)
	}
	for _, h := range hits {
		rel := ""
		if h.Kind != "" && h.Kind != "co-mention" {
			rel = h.Kind + ", "
		}
		if h.Via == "" {
			fmt.Printf("%s (%s%d)\n", h.Term, rel, h.Weight)
		} else {
			fmt.Printf("%s (%s%d, via %s)\n", h.Term, rel, h.Weight, h.Via)
		}
	}
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
	db, err := openTolerant(*dbPath, true)
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

// openTolerant retries opens that fail only because another process
// (usually the daemon mid-cycle) holds DuckDB's single-process lock.
// Any other error returns immediately.
func openTolerant(path string, readOnly bool) (*store.DB, error) {
	var err error
	var db *store.DB
	for i := 0; i < 12; i++ {
		if readOnly {
			db, err = store.OpenReadOnly(path)
		} else {
			db, err = store.Open(path)
		}
		if err == nil {
			return db, nil
		}
		if !strings.Contains(err.Error(), "Could not set lock") {
			return nil, err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, err
}
