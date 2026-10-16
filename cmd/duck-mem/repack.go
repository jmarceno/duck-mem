package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jmarceno/duck-mem/internal/owner"
	"github.com/jmarceno/duck-mem/internal/store"
)

func repackCmd(args []string) {
	fs := flag.NewFlagSet("repack", flag.ExitOnError)
	dbPath := fs.String("db", defaultDB(), "DuckDB file")
	out := fs.String("out", "", "write a verified copy here instead of repacking in place")
	flagArgs, positional := splitArgs(args, map[string]bool{"--db": true, "--out": true})
	_ = fs.Parse(flagArgs)
	if len(positional) != 0 {
		fmt.Fprintln(os.Stderr, "repack takes no positional arguments")
		os.Exit(2)
	}
	start := time.Now()
	res, err := runRepack(*dbPath, *out)
	duration := time.Since(start).Round(time.Millisecond)
	// A repack that failed after the swap still reports the new size first.
	if res.Out != "" {
		fmt.Printf("repacked=%s bytes=%d duration=%s source_unchanged=true\n", res.Out, res.After, duration)
	} else if res.After > 0 {
		fmt.Printf("repacked=%s bytes=%d->%d duration=%s\n", *dbPath, res.Before, res.After, duration)
	}
	if err != nil {
		fatal(err)
	}
}

// runRepack prefers the daemon: it closes its handle for the duration, so
// the database can be rebuilt while the indexer is up. Without a daemon
// this process takes the owner lock and repacks the file itself. Out is
// made absolute so a daemon in another working directory writes it where
// the caller meant.
func runRepack(dbPath, out string) (store.RepackResult, error) {
	if out != "" {
		abs, err := filepath.Abs(out)
		if err != nil {
			return store.RepackResult{}, err
		}
		out = abs
	}
	client, err := dialOwner(dbPath)
	if err == nil {
		defer client.Close()
		return client.Repack(out)
	}
	if !errors.Is(err, errNoDaemon) {
		return store.RepackResult{}, err
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return store.RepackResult{}, err
	}
	unlock, err := owner.TryLock(abs)
	if errors.Is(err, owner.ErrBusy) {
		return store.RepackResult{}, fmt.Errorf("database is in use; retry in a moment")
	}
	if err != nil {
		return store.RepackResult{}, err
	}
	defer unlock()
	if out != "" {
		return store.RepackTo(abs, out)
	}
	return store.RepackSwap(abs)
}
