package main

import (
	"flag"
	"fmt"
	"os"
	"time"

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
