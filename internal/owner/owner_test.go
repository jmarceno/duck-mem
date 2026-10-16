package owner

import (
	"context"
	"errors"
	"os"
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

func TestTransactionErrorsReopenThenPauseWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.duckdb")
	own, err := Start(path, Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()

	fatalWrite := func() error {
		_, err := own.Do(KindWrite, func(_ context.Context, _ *store.DB) (any, error) {
			return nil, errors.New("TransactionContext Error: Failed to commit: Failed to rollback transaction")
		})
		return err
	}
	cleanWrite := func() error {
		_, err := own.Do(KindWrite, func(_ context.Context, _ *store.DB) (any, error) {
			return nil, nil
		})
		return err
	}
	count := func() error {
		_, err := own.Do(KindRead, func(_ context.Context, db *store.DB) (any, error) {
			_, _, err := db.Count()
			return nil, err
		})
		return err
	}
	handle := func() *store.DB {
		var got *store.DB
		if _, err := own.Do(KindRead, func(_ context.Context, db *store.DB) (any, error) {
			got = db
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}

	first := handle()
	if err := fatalWrite(); err == nil {
		t.Fatal("fatal write reported success")
	}
	if own.Paused() {
		t.Fatal("paused after a single transaction error")
	}
	// The handle was reopened, so the next job gets a fresh one.
	if second := handle(); second == first {
		t.Fatal("the database was not reopened after a transaction error")
	}
	if err := count(); err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	// A clean write clears the streak; a second fatal error then pauses.
	if err := cleanWrite(); err != nil {
		t.Fatal(err)
	}
	if err := fatalWrite(); err == nil {
		t.Fatal("fatal write reported success")
	}
	if own.Paused() {
		t.Fatal("paused before the streak reached two")
	}
	if err := fatalWrite(); err == nil {
		t.Fatal("fatal write reported success")
	}
	if !own.Paused() {
		t.Fatal("not paused after two transaction errors in a row")
	}
	ran := make(chan struct{}, 1)
	_, err = own.Do(KindWrite, func(_ context.Context, _ *store.DB) (any, error) {
		ran <- struct{}{}
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("paused write error = %v", err)
	}
	select {
	case <-ran:
		t.Fatal("a write ran while paused")
	default:
	}
	if err := count(); err != nil {
		t.Fatalf("read while paused: %v", err)
	}
}

func TestRepackClearsPauseOnlyInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.duckdb")
	own, err := Start(path, Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()

	if _, err := own.Do(KindWrite, func(_ context.Context, db *store.DB) (any, error) {
		return nil, db.UpsertSession(store.Session{ID: "kept", Source: "codex", Project: "p"})
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := own.Do(KindWrite, func(_ context.Context, _ *store.DB) (any, error) {
			return nil, errors.New("TransactionContext Error: Conflict on update!")
		}); err == nil {
			t.Fatal("fatal write reported success")
		}
	}
	if !own.Paused() {
		t.Fatal("not paused after two transaction errors")
	}

	// Copying verifies the file but leaves the live one alone: no resume.
	if _, err := own.Repack(filepath.Join(dir, "copy.duckdb")); err != nil {
		t.Fatal(err)
	}
	if !own.Paused() {
		t.Fatal("a copy-only repack cleared the pause")
	}

	res, err := own.Repack("")
	if err != nil {
		t.Fatal(err)
	}
	if res.Out != "" || res.Before == 0 || res.After == 0 {
		t.Fatalf("result = %+v", res)
	}
	if own.Paused() {
		t.Fatal("an in-place repack did not clear the pause")
	}
	ran := make(chan struct{})
	val, err := own.Do(KindRead, func(ctx context.Context, db *store.DB) (any, error) {
		hit, err := db.ResolveSession(ctx, "kept")
		close(ran)
		return hit, err
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ran:
	default:
		t.Fatal("the session did not survive the swap")
	}
	hit, _ := val.(store.SessionResult)
	if hit.Session.ID != "kept" {
		t.Fatalf("session after the swap = %q", hit.Session.ID)
	}
	if _, err := own.Do(KindWrite, func(_ context.Context, _ *store.DB) (any, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("write after an in-place repack: %v", err)
	}
}

func TestCloseAfterFailedReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.duckdb")
	own, err := Start(path, Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := own.Close(); err != nil {
			t.Errorf("close after a failed reopen: %v", err)
		}
	}()

	// A file that cannot be reopened: replace it with a directory.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := own.Do(KindWrite, func(_ context.Context, _ *store.DB) (any, error) {
		return nil, errors.New("TransactionContext Error: Conflict on update!")
	}); err == nil {
		t.Fatal("fatal write reported success")
	}
	if !own.Paused() {
		t.Fatal("not paused after a failed reopen")
	}
	if _, err := own.Do(KindRead, func(_ context.Context, db *store.DB) (any, error) {
		_, _, err := db.Count()
		return nil, err
	}); err == nil {
		t.Fatal("a read was served without a database")
	}
}
