package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jmarceno/duck-mem/internal/owner"
	"github.com/jmarceno/duck-mem/internal/store"
)

func relocateCmd(args []string) {
	fs := flag.NewFlagSet("relocate", flag.ExitOnError)
	dbPath := fs.String("db", defaultDB(), "DuckDB file")
	flagArgs, positional := splitArgs(args, map[string]bool{"--db": true})
	_ = fs.Parse(flagArgs)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "relocate needs the new location: duck-mem relocate <new-path>")
		os.Exit(2)
	}
	if err := runRelocate(*dbPath, positional[0], systemIndexer, os.Stdin, os.Stdout); err != nil {
		fatal(err)
	}
}

// indexerControl stops and restarts the installed indexer around the move.
// Tests replace it so the suite never touches the user's services.
type indexerControl struct {
	stop  func() (bool, error) // true means a restart is owed afterwards
	start func() error
}

var systemIndexer = indexerControl{stop: stopDuckMemService, start: startDuckMemService}

// stopDuckMemService stops the installed indexer only when it is running.
func stopDuckMemService() (bool, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false, nil
	}
	if err := exec.Command("systemctl", "--user", "is-active", "--quiet", serviceName).Run(); err != nil {
		return false, nil // inactive or missing unit: nothing to stop
	}
	if err := systemctl("stop", serviceName); err != nil {
		return false, err
	}
	return true, nil
}

func startDuckMemService() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	return systemctl("start", serviceName)
}

// runRelocate moves the database to a new location without risking the data
// in it: the copy is verified table by table and opened for a smoke run
// before anything is recorded or deleted, and the old file is trashed, not
// removed. With no database yet it only records the location. The user is
// asked before the old file goes; answering no (or a closed stdin) keeps it.
func runRelocate(sourceArg, targetArg string, control indexerControl, in io.Reader, out io.Writer) error {
	source, err := expandHome(sourceArg)
	if err != nil {
		return err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	target, err := expandHome(targetArg)
	if err != nil {
		return err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	if filepath.Clean(source) == filepath.Clean(target) {
		return fmt.Errorf("new location is the current one: %s", source)
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("destination already exists: %s", target)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}

	if _, err := os.Stat(source); errors.Is(err, fs.ErrNotExist) {
		// Fresh location: nothing to move, record it for every command.
		if err := writeConfiguredDB(target); err != nil {
			return err
		}
		fmt.Fprintf(out, "database location set to %s; it will be created on the first ingest\n", target)
		return nil
	} else if err != nil {
		return err
	}

	// The daemon would keep the file locked and keep writing to the old
	// location, so when the lock is held, stop the installed indexer for
	// the duration and restart it afterwards.
	var restart bool
	unlock, err := owner.TryLock(source)
	if errors.Is(err, owner.ErrBusy) {
		var stopped bool
		if stopped, err = control.stop(); err != nil {
			return err
		}
		restart = stopped
		if stopped {
			unlock, err = owner.TryLock(source)
		}
	}
	if err != nil {
		if restart {
			_ = control.start()
		}
		if errors.Is(err, owner.ErrBusy) {
			return fmt.Errorf("database is in use; stop the daemon and retry")
		}
		return err
	}
	defer unlock()

	// Fold the WAL into the file so the copy contains every write, and
	// remember the totals the copy must hold.
	srcDB, err := store.Open(source)
	if err != nil {
		return err
	}
	wantSessions, wantMessages, err := srcDB.Count()
	if err == nil {
		err = srcDB.Checkpoint()
	}
	if closeErr := srcDB.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}

	// Repack verifies every table (row counts and row hashes) before the
	// new file becomes visible; a failure here leaves the old file alone.
	if err := store.Repack(source, target); err != nil {
		fmt.Fprintf(out, "copy failed; the database is still at %s\n", source)
		return fmt.Errorf("copy to %s failed: %w", target, err)
	}

	if err := smokeRun(target, wantSessions, wantMessages); err != nil {
		_ = os.Remove(target)
		_ = os.Remove(target + ".wal")
		fmt.Fprintf(out, "smoke run failed; the copy at %s was removed and the database is still at %s\n", target, source)
		return err
	}
	if err := writeConfiguredDB(target); err != nil {
		_ = os.Remove(target)
		_ = os.Remove(target + ".wal")
		return fmt.Errorf("recording the new location failed: %w", err)
	}
	fmt.Fprintf(out, "relocated %s -> %s; the copy passed the smoke run\n", source, target)

	if askTrash(in, out, source) {
		if err := trashPath(source); err != nil {
			fmt.Fprintf(out, "could not move the old database to the trash; it is still at %s: %v\n", source, err)
		} else {
			fmt.Fprintf(out, "moved the old database to the trash\n")
			// Siblings of a checkpointed file hold no data: the WAL was
			// folded before the copy, the rest are runtime artifacts.
			for _, p := range []string{source + ".wal", source + ".sock", source + ".owner"} {
				if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
					fmt.Fprintf(out, "removing %s failed: %v\n", p, err)
				}
			}
		}
	} else {
		fmt.Fprintf(out, "kept the old database at %s; delete it yourself once the new one has proven itself\n", source)
	}

	if restart {
		if err := control.start(); err != nil {
			return fmt.Errorf("relocated to %s, but restarting duck-mem.service failed: %w", target, err)
		}
		fmt.Fprintf(out, "restarted duck-mem.service on the new location\n")
	}
	return nil
}

// smokeRun proves the copy behaves like the real database: it opens with the
// full schema init, holds exactly the sessions and messages that were
// counted in the source, and answers a search through the graph.
func smokeRun(target string, wantSessions, wantMessages int) error {
	db, err := store.Open(target)
	if err != nil {
		return fmt.Errorf("smoke run: %w", err)
	}
	sessions, messages, err := db.Count()
	if err == nil && (sessions != wantSessions || messages != wantMessages) {
		err = fmt.Errorf("smoke run: copy holds %d sessions/%d messages, the source had %d/%d",
			sessions, messages, wantSessions, wantMessages)
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), owner.AgentReadTimeout)
		defer cancel()
		if _, _, err := db.RecallFiltered(ctx, "memory", store.ProjectFilter{}, "", 1, 1); err != nil {
			err = fmt.Errorf("smoke run: search failed: %w", err)
		}
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	return err
}

// askTrash asks before removing the old database and defaults to keeping it.
func askTrash(in io.Reader, out io.Writer, path string) bool {
	fmt.Fprintf(out, "move the old database %s to the trash? [y/N]: ", path)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		fmt.Fprintln(out, "reading the answer failed; keeping the old database")
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// expandHome resolves a leading ~ so a quoted "~/..." location works.
func expandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
	}
	return path, nil
}
