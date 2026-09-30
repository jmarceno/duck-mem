// Command duck-mem ingests agent session logs into DuckDB (iteration 1:
// everything minus tool calls) and searches them (iteration 2 CLI).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/jmarceno/duck-mem/internal/ingest"
	"github.com/jmarceno/duck-mem/internal/store"
)

const parserVersion = 1
const openCodeParserVersion = 1

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
	case "--install":
		if err := installUser(); err != nil {
			fatal(err)
		}
	case "--uninstall":
		if err := uninstallUser(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "tray":
		if err := runTray(); err != nil {
			fatal(err)
		}
	case "ingest":
		ingestCmd(os.Args[2:])
	case "query":
		queryCmd(os.Args[2:])
	case "show":
		showCmd(os.Args[2:])
	case "index":
		indexCmd(os.Args[2:])
	case "related":
		relatedCmd(os.Args[2:])
	case "daemon":
		daemonCmd(os.Args[2:])
	case "repack":
		repackCmd(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  duck-mem --install                    install CLI, daemon, and tray for this user
  duck-mem --uninstall [--keep-data|--purge-data]
  duck-mem ingest [--db PATH] [ROOT...]   ingest session logs (default roots when omitted)
  duck-mem query [--db PATH] [--project P] [--source S] [--limit N] [--hits N] <text...>
                                        best-matching sessions, each with its matching messages
  duck-mem show [--db PATH] [--context N] [--from A] [--to B] [--full] <session>[#seq]
                                        read a session's messages around a hit
  duck-mem index [--db PATH] [--min-df N] [--full] rebuild the topic graph
  duck-mem related [--db PATH] [--project P] [--depth 1|2] [--limit N] <term>
  duck-mem daemon [--db PATH] [--interval 5m] [ROOT...]
  duck-mem repack [--db PATH] --out PATH  write a verified ZSTD copy; source remains unchanged
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
		logSyncFailure("all", "Database error", err)
		fatal(err)
	}
	db, err := openTolerant(*dbPath, false)
	if err != nil {
		logSyncFailure("all", "Database error", err)
		fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logSyncFailure("all", "Database error", err)
		}
	}()

	files, nSess, nMsg, nSkip := runCycle(db, roots)
	fmt.Printf("files=%d sessions=%d messages=%d skipped=%d db=%s\n", files, nSess, nMsg, nSkip, *dbPath)
	newMsgs, newPairs, err := db.IndexNew(2)
	if err != nil {
		logSyncFailure("all", "Database error", err)
		fatal(err)
	}
	fmt.Printf("index: +%d messages, +%d pairs\n", newMsgs, newPairs)
	if err := writeSyncStatus(*dbPath, syncStatus{CompletedAt: time.Now(), Files: files, Skipped: nSkip}); err != nil {
		logSyncFailure("all", "Other", err)
		fmt.Fprintln(os.Stderr, "sync status:", err)
	}
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
		logSyncFailure("all", "Database error", err)
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
			logSyncFailure("all", "Database error", err)
			fmt.Printf("%s cycle=%d open error: %v\n", time.Now().UTC().Format(time.RFC3339), n, err)
			return
		}
		files, nSess, nMsg, nSkip := runCycle(db, roots)
		newMsgs, newPairs, err := db.IndexNew(2)
		err = errors.Join(err, db.Close())
		if err != nil {
			logSyncFailure("all", "Database error", err)
			fmt.Printf("%s cycle=%d error: %v\n", time.Now().UTC().Format(time.RFC3339), n, err)
			return
		}
		if err := writeSyncStatus(*dbPath, syncStatus{CompletedAt: time.Now(), Files: files, Skipped: nSkip}); err != nil {
			logSyncFailure("all", "Other", err)
			fmt.Printf("%s cycle=%d status error: %v\n", time.Now().UTC().Format(time.RFC3339), n, err)
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

// runCycle skips unchanged files and reads only the appended bytes of
// append-only JSONL files. Rewrites take the full reconciliation path.
func runCycle(db *store.DB, roots []string) (files, nSess, nMsg, nSkip int) {
	cursorProjects := map[string]map[string]string{}
	entries := map[string]syncLogEntry{}
	defer func() {
		keys := make([]string, 0, len(entries))
		for key := range entries {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := appendSyncLog(entries[key]); err != nil {
				fmt.Fprintln(os.Stderr, "sync log:", err)
			}
		}
	}()
	for _, f := range ingest.DiscoverWithErrors(roots, func(path string, err error) {
		logSyncFailure("all", "Other", fmt.Errorf("discover %s: %w", path, err))
		nSkip++
	}) {
		files++
		func() {
			entry := syncLogEntry{Harness: harnessName(ingest.Classify(f)), Result: "success"}
			reason := "Other"
			var failure error
			defer func() {
				if failure != nil {
					entry.Result, entry.Reason, entry.Detail = "failure", reason, failure.Error()
				}
				key := entry.Harness + "\x00" + entry.Reason + "\x00" + entry.Detail
				previous := entries[key]
				entry.Sessions += previous.Sessions
				entry.Lines += previous.Lines
				entries[key] = entry
			}()
			if ingest.Classify(f) == ingest.KindOpenCode {
				sessions, messages, err := ingestOpenCode(db, f)
				entry.Sessions, entry.Lines = sessions, messages
				nSess += sessions
				nMsg += messages
				if err != nil {
					failure, reason = err, extractionReason(err)
					var dbErr *syncDatabaseError
					if errors.As(err, &dbErr) {
						reason = "Database error"
					}
					fmt.Fprintf(os.Stderr, "skip %s: %v\n", f, err)
					nSkip++
				}
				return
			}
			info, err := os.Stat(f)
			if err != nil {
				failure = err
				nSkip++
				return
			}
			var projects map[string]string
			kind := ingest.Classify(f)
			if kind == ingest.KindCursorTranscript {
				home := ingest.CursorHome(f)
				var ok bool
				projects, ok = cursorProjects[home]
				if !ok {
					projects = ingest.LoadCursorProjects(home)
					cursorProjects[home] = projects
				}
			}
			reason = "Database error"
			checkpoint, err := db.GetCheckpoint(f)
			if err != nil {
				failure = err
				nSkip++
				return
			}
			projectMatches := kind != ingest.KindCursorTranscript ||
				(checkpoint != nil && checkpoint.Project == ingest.CursorProject(f, projects))
			if checkpoint != nil && checkpoint.ParserVersion == parserVersion && projectMatches &&
				checkpoint.Size == info.Size() && checkpoint.ModTimeNS == info.ModTime().UnixNano() {
				return
			}
			var sess store.Session
			var msgs []store.Message
			appended := false
			if checkpoint != nil && checkpoint.ParserVersion == parserVersion && projectMatches &&
				kind != ingest.KindCursorPlan && checkpoint.EndsLine && info.Size() > checkpoint.Size {
				if oldHash, err := hashTail(f, checkpoint.Size); err == nil && oldHash == checkpoint.TailHash {
					previous, err := db.GetSession(checkpoint.SessionID)
					if err != nil {
						logSyncFailure(entry.Harness, "Database error", err)
					}
					if err == nil {
						last, err := db.LastMessage(checkpoint.SessionID)
						if err != nil {
							logSyncFailure(entry.Harness, "Database error", err)
						}
						if err == nil {
							sess, msgs, err = ingest.IngestAppended(f, checkpoint.Size, previous, last, projects)
							appended = err == nil && sess.ID == checkpoint.SessionID && sess.Project == checkpoint.Project
						}
					}
				}
			}
			reason = "Other"
			if !appended {
				sess, msgs, err = ingest.IngestFileWithCursorProjects(f, projects)
			}
			if err == nil && sess.ID == "" {
				err = ingest.ErrUnexpectedFormat
			}
			if err != nil || sess.ID == "" {
				reason = extractionReason(err)
				failure = err
				nSkip++
				return
			}
			reason = "Database error"
			if err := db.UpsertSession(sess); err != nil {
				failure = err
				nSkip++
				return
			}
			if appended {
				err = db.InsertMessages(msgs)
			} else {
				err = db.SyncMessages(sess.ID, msgs)
			}
			if err != nil {
				failure = err
				nSkip++
				return
			}
			reason = "Other"
			state, err := fileState(f)
			if err != nil {
				failure = err
				nSkip++
			}
			if err == nil && state.Size == info.Size() && state.ModTimeNS == info.ModTime().UnixNano() {
				state.SessionID, state.Project, state.ParserVersion = sess.ID, sess.Project, parserVersion
				if err := db.SaveCheckpoint(state); err != nil {
					failure, reason = err, "Database error"
					nSkip++
				}
			}
			entry.Sessions, entry.Lines = 1, len(msgs)
			nSess++
			nMsg += len(msgs)
		}()
	}
	return files, nSess, nMsg, nSkip
}

func ingestOpenCode(db *store.DB, path string) (int, int, error) {
	before, err := sqliteState(path)
	if err != nil {
		return 0, 0, err
	}
	checkpoint, err := db.GetCheckpoint(path)
	if err != nil {
		return 0, 0, &syncDatabaseError{err}
	}
	if checkpoint != nil && checkpoint.ParserVersion == openCodeParserVersion &&
		checkpoint.Size == before.Size && checkpoint.ModTimeNS == before.ModTimeNS &&
		checkpoint.TailHash == before.TailHash {
		return 0, 0, nil
	}
	sessions, err := ingest.IngestOpenCode(path)
	if err != nil {
		if errors.Is(err, ingest.ErrUnexpectedFormat) {
			return 0, 0, err
		}
		return 0, 0, &syncDatabaseError{err}
	}
	count, completed := 0, 0
	for _, s := range sessions {
		if err := db.UpsertSession(s.Session); err != nil {
			return completed, count, &syncDatabaseError{err}
		}
		if err := db.SyncMessages(s.Session.ID, s.Messages); err != nil {
			return completed, count, &syncDatabaseError{err}
		}
		count += len(s.Messages)
		completed++
	}
	after, err := sqliteState(path)
	if err != nil {
		return completed, count, err
	}
	if err == nil && before.Size == after.Size && before.ModTimeNS == after.ModTimeNS && before.TailHash == after.TailHash {
		after.ParserVersion = openCodeParserVersion
		if err := db.SaveCheckpoint(after); err != nil {
			return completed, count, &syncDatabaseError{err}
		}
	}
	return len(sessions), count, nil
}

// SQLite may keep recent writes entirely in its WAL; checkpoint both files.
func sqliteState(path string) (store.FileCheckpoint, error) {
	main, err := fileState(path)
	if err != nil {
		return store.FileCheckpoint{}, err
	}
	wal, err := fileState(path + "-wal")
	if os.IsNotExist(err) {
		return main, nil
	}
	if err != nil {
		return store.FileCheckpoint{}, err
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%s:%d:%d:%s", main.Size, main.ModTimeNS, main.TailHash, wal.Size, wal.ModTimeNS, wal.TailHash)))
	main.Size += wal.Size
	if wal.ModTimeNS > main.ModTimeNS {
		main.ModTimeNS = wal.ModTimeNS
	}
	main.TailHash = hex.EncodeToString(hash[:])
	return main, nil
}

func hashTail(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	start := size - 4096
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	if len(buf) > 0 {
		if n, err := f.ReadAt(buf, start); err != nil || n != len(buf) {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return "", err
		}
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

func fileState(path string) (store.FileCheckpoint, error) {
	info, err := os.Stat(path)
	if err != nil {
		return store.FileCheckpoint{}, err
	}
	hash, err := hashTail(path, info.Size())
	if err != nil {
		return store.FileCheckpoint{}, err
	}
	state := store.FileCheckpoint{Path: path, Size: info.Size(), ModTimeNS: info.ModTime().UnixNano(), TailHash: hash}
	if info.Size() > 0 {
		f, err := os.Open(path)
		if err != nil {
			return store.FileCheckpoint{}, err
		}
		defer f.Close()
		var last [1]byte
		if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
			return store.FileCheckpoint{}, err
		}
		state.EndsLine = last[0] == '\n'
	}
	return state, nil
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
	hits, err := db.Related(strings.Join(fs.Args(), " "), *project, *depth, *limit)
	if err != nil {
		fatal(err)
	}
	for _, h := range hits {
		rel := fmt.Sprintf("%d", h.Weight)
		if h.Kind != "" && h.Kind != "co-mention" {
			rel = fmt.Sprintf("%s: %s -> %s, %d", h.Kind, h.From, h.To, h.Weight)
		}
		if h.Via == "" {
			fmt.Printf("%s (%s)", h.Term, rel)
		} else {
			fmt.Printf("%s (%s, via %s)", h.Term, rel, h.Via)
		}
		if h.Evidence != "" {
			fmt.Printf(" [%s]", clip(h.Evidence, 240))
		}
		fmt.Println()
	}
}

func queryCmd(args []string) {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	dbPath := fs.String("db", defaultDB(), "DuckDB file")
	project := fs.String("project", "", "filter by project path (substring match)")
	source := fs.String("source", "", "filter by source: codex|claude|cursor|muse|opencode")
	limit := fs.Int("limit", 5, "max sessions")
	perSession := fs.Int("hits", 3, "max matching messages shown per session")
	known := map[string]bool{"--db": true, "--project": true, "--source": true, "--limit": true, "--hits": true}
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

	q := strings.Join(fs.Args(), " ")
	results, related, err := db.Recall(q, *project, *source, *limit, *perSession)
	if err != nil {
		fatal(err)
	}
	scope := ""
	if *project != "" {
		scope += " in projects matching " + strconv.Quote(*project)
	}
	if *source != "" {
		scope += " from " + *source
	}
	if len(results) == 0 {
		fmt.Printf("no matches for %q%s\n", q, scope)
		if *project != "" {
			if others, _, err := db.Recall(q, "", *source, 5, 1); err == nil && len(others) > 0 {
				var projects []string
				seen := map[string]bool{}
				for _, r := range others {
					if !seen[r.Session.Project] {
						seen[r.Session.Project] = true
						projects = append(projects, r.Session.Project)
					}
				}
				fmt.Printf("matches exist in other projects: %s (drop --project to see them)\n", strings.Join(projects, ", "))
			}
		}
		fmt.Println("tip: use a few distinctive words (names, identifiers, error text); word forms are matched (enemy/enemies)")
		return
	}
	fmt.Printf("%d sessions for %q%s, best first. Messages are numbered #seq.\n\n", len(results), q, scope)
	for i, r := range results {
		fmt.Printf("[%d] %s  %s · %s · %d msgs · %s\n", i+1, r.Session.ID, r.Session.Source,
			dateRange(r.Session.StartedAt, r.LastAt), r.Messages, r.Session.Project)
		if r.ViaGraph {
			fmt.Println("    (no query words here; found through the topic-graph neighbours below)")
		}
		if r.Title != "" {
			fmt.Printf("    opened with: %q\n", clip(r.Title, 160))
		}
		for _, h := range r.Hits {
			fmt.Printf("    #%d %s%s: %s\n", h.Seq, h.Role, stamp(" ", h.CreatedAt), h.Snippet)
		}
		fmt.Println()
	}
	if len(related) > 0 {
		words := make([]string, len(related))
		for i, t := range related {
			words[i] = fmt.Sprintf("%s (%d)", t.Label, t.Weight)
		}
		fmt.Printf("Topic graph: often discussed with these words: %s\n", strings.Join(words, ", "))
		fmt.Println("  Add one to the query to widen it, or map the area: duck-mem related " + projectFlag(*project) + related[0].Label)
	}
	top := results[0]
	fmt.Printf("Read a hit with the messages around it: duck-mem show %s#%d\n", top.Session.ID, top.Hits[0].Seq)
}

func showCmd(args []string) {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	dbPath := fs.String("db", defaultDB(), "DuckDB file")
	ctxN := fs.Int("context", 3, "messages before and after #seq")
	from := fs.Int("from", -1, "first message #seq to show")
	to := fs.Int("to", -1, "last message #seq to show")
	full := fs.Bool("full", false, "print whole messages instead of truncating long ones")
	known := map[string]bool{"--db": true, "--context": true, "--from": true, "--to": true, "--full": false}
	flagArgs, positional := splitArgs(args, known)
	_ = fs.Parse(flagArgs)
	fs.Parse(positional)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "show needs one session reference: <session-id>[#seq] (an ID prefix works)")
		os.Exit(2)
	}
	ref, target := fs.Arg(0), -1
	if i := strings.LastIndexByte(ref, '#'); i >= 0 {
		n, err := strconv.Atoi(ref[i+1:])
		if err != nil || n < 0 {
			fmt.Fprintf(os.Stderr, "bad message number in %q: want <session-id>#<seq>\n", ref)
			os.Exit(2)
		}
		ref, target = ref[:i], n
	}
	db, err := openTolerant(*dbPath, true)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	s, err := db.ResolveSession(ref)
	if err != nil {
		fatal(err)
	}
	lo, hi := showWindow(target, *ctxN, *from, *to, s.Messages)
	msgs, err := db.Window(s.Session.ID, lo, hi)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("session %s · %s · %s\n", s.Session.ID, s.Session.Source, s.Session.Project)
	fmt.Printf("%s · %d messages (#0–#%d) · log %s\n", dateRange(s.Session.StartedAt, s.LastAt), s.Messages, s.Messages-1, s.Session.Path)
	if len(msgs) == 0 {
		fmt.Printf("no messages in #%d–#%d\n", lo, hi)
		return
	}
	var more []string
	if lo > 0 {
		more = append(more, fmt.Sprintf("earlier: --from %d --to %d", max(0, lo-(hi-lo+1)), lo-1))
	}
	if hi < s.Messages-1 {
		more = append(more, fmt.Sprintf("later: --from %d --to %d", hi+1, min(s.Messages-1, hi+(hi-lo+1))))
	}
	if !*full {
		more = append(more, "whole messages: --full")
	}
	fmt.Printf("showing #%d–#%d; %s\n", msgs[0].Seq, msgs[len(msgs)-1].Seq, strings.Join(more, "; "))
	for _, m := range msgs {
		mark := ""
		if m.Seq == target {
			mark = "  <- hit"
		}
		fmt.Printf("\n--- #%d %s%s%s\n", m.Seq, m.Role, stamp(" · ", m.CreatedAt), mark)
		text := strings.TrimSpace(m.Text)
		limit := 1500
		if m.Seq == target {
			limit = 4000
		}
		if !*full && utf8.RuneCountInString(text) > limit {
			cut := []rune(text)
			text = string(cut[:limit]) + fmt.Sprintf(" … [+%d chars; --full]", len(cut)-limit)
		}
		fmt.Println(text)
	}
}

// showWindow picks the #seq range: explicit --from/--to first, then
// --context around the hit, else the session's opening messages.
func showWindow(target, context, from, to, total int) (int, int) {
	lo, hi := 0, 19
	if target >= 0 {
		lo, hi = target-context, target+context
	}
	if from >= 0 {
		lo = from
		if to < 0 {
			hi = from + 19
		}
	}
	if to >= 0 {
		hi = to
		if from < 0 && target < 0 {
			lo = to - 19
		}
	}
	return max(0, lo), min(hi, max(0, total-1))
}

func projectFlag(project string) string {
	if project == "" {
		return ""
	}
	return "--project " + project + " "
}

func dateRange(start, last time.Time) string {
	if start.IsZero() {
		start = last
	}
	if start.IsZero() {
		return "undated"
	}
	out := start.Local().Format("2006-01-02")
	if !last.IsZero() && last.Local().Format("2006-01-02") != out {
		out += " → " + last.Local().Format("2006-01-02")
	}
	return out
}

func stamp(sep string, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return sep + t.Local().Format("2006-01-02 15:04")
}

// clip collapses whitespace and cuts s to n runes.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
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
