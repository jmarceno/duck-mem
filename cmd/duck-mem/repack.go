package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jmarceno/duck-mem/internal/owner"
	"github.com/jmarceno/duck-mem/internal/store"
)

func repackCmd(args []string) {
	fs := flag.NewFlagSet("repack", flag.ExitOnError)
	source := fs.String("db", defaultDB(), "source database (read only)")
	out := fs.String("out", "", "new database path; must not exist")
	_ = fs.Parse(args)
	if *out == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "repack requires --out PATH")
		os.Exit(2)
	}
	unlock, err := owner.TryLock(*source)
	if errors.Is(err, owner.ErrBusy) {
		fatal(fmt.Errorf("stop duck-mem.service before repack; it holds the database"))
	}
	if err != nil {
		fatal(err)
	}
	defer unlock()
	start := time.Now()
	if err := store.Repack(*source, *out); err != nil {
		fatal(err)
	}
	info, err := os.Stat(*out)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("repacked=%s bytes=%d duration=%s source_unchanged=true\n", *out, info.Size(), time.Since(start).Round(time.Millisecond))
}
