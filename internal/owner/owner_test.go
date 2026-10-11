package owner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmarceno/duck-mem/internal/store"
)

func TestReadWaitsUntilWriteFinishes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.duckdb")
	release := make(chan struct{})
	started := make(chan struct{})
	own, err := Start(path, Handlers{
		Ingest: func(db *store.DB, _ []string) (IngestResult, error) {
			close(started)
			if err := db.UpsertSession(store.Session{ID: "held", Source: "codex", Project: "p"}); err != nil {
				return IngestResult{}, err
			}
			<-release
			return IngestResult{Sessions: 1}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()

	writeDone := make(chan error, 1)
	go func() {
		c, err := Dial(path)
		if err != nil {
			writeDone <- err
			return
		}
		defer c.Close()
		_, err = c.Ingest(nil)
		writeDone <- err
	}()
	select {
	case <-started:
	case err := <-writeDone:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("write did not start")
	}

	readDone := make(chan struct {
		hit store.SessionResult
		err error
	}, 1)
	go func() {
		c, err := Dial(path)
		if err != nil {
			readDone <- struct {
				hit store.SessionResult
				err error
			}{err: err}
			return
		}
		defer c.Close()
		hit, err := c.ResolveSession("held")
		readDone <- struct {
			hit store.SessionResult
			err error
		}{hit, err}
	}()
	select {
	case got := <-readDone:
		t.Fatalf("read finished while the write still held the queue: %v", got.err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("write did not finish")
	}
	select {
	case got := <-readDone:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.hit.Session.ID != "held" {
			t.Fatalf("session = %q", got.hit.Session.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("read did not finish")
	}
}

func TestReadDeadlineStartsWhenTheJobRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.duckdb")
	release := make(chan struct{})
	started := make(chan struct{})
	own, err := Start(path, Handlers{
		Ingest: func(db *store.DB, _ []string) (IngestResult, error) {
			close(started)
			<-release
			return IngestResult{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	own.readTimeout = 80 * time.Millisecond

	go func() {
		_, _ = own.Do(KindWrite, func(_ context.Context, db *store.DB) (any, error) {
			return own.handlers.Ingest(db, nil)
		})
	}()
	<-started

	type stamp struct {
		at  time.Time
		err error
	}
	readDone := make(chan stamp, 1)
	go func() {
		val, err := own.Do(KindRead, func(ctx context.Context, _ *store.DB) (any, error) {
			if ctx.Err() != nil {
				return time.Time{}, ctx.Err()
			}
			startedAt := time.Now()
			<-ctx.Done()
			return startedAt, ctx.Err()
		})
		at, _ := val.(time.Time)
		readDone <- stamp{at, err}
	}()
	time.Sleep(200 * time.Millisecond)
	close(release)

	var got stamp
	select {
	case got = <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("read did not finish")
	}
	if got.err == nil || !strings.Contains(got.err.Error(), "query timed out") {
		t.Fatalf("read error = %v", got.err)
	}
	if got.at.IsZero() {
		t.Fatal("deadline had already expired when the read started")
	}
	ran := time.Since(got.at)
	if ran < 50*time.Millisecond {
		t.Fatalf("read ran %s; queue wait was counted against the deadline", ran)
	}

	next := make(chan struct{})
	if _, err := own.Do(KindWrite, func(_ context.Context, db *store.DB) (any, error) {
		if _, _, err := db.Count(); err != nil {
			return nil, err
		}
		close(next)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-next:
	default:
		t.Fatal("next job did not run")
	}
}
