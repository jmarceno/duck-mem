// Package owner is the single process allowed to open a duck-mem database
// while it is running. Reads and writes share one FIFO worker, so a query
// waits out a sync instead of losing DuckDB's file lock.
package owner

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jmarceno/duck-mem/internal/store"

	"golang.org/x/sys/unix"
)

// AgentReadTimeout caps query, show, and related once they start running.
// Time spent waiting in the queue does not count. Ingest and index do not
// use it.
const AgentReadTimeout = 30 * time.Second

// maxFrame is the largest request or response. A full session window fits;
// a runaway payload does not.
const maxFrame = 32 << 20

// ErrBusy means another duck-mem process holds the owner lock.
var ErrBusy = errors.New("duck-mem database is in use")

// Kind is a queue job. Reads are interrupted at AgentReadTimeout.
// Writes run until they finish.
type Kind int

const (
	KindWrite Kind = iota
	KindRead
)

// IngestResult is what one ingest-and-index cycle changed.
type IngestResult struct {
	Files    int `json:"files"`
	Sessions int `json:"sessions"`
	Messages int `json:"messages"`
	Skipped  int `json:"skipped"`
	NewMsgs  int `json:"new_msgs"`
	NewPairs int `json:"new_pairs"`
}

// IndexResult is what an index run changed. A full rebuild leaves the
// counts at zero.
type IndexResult struct {
	NewMsgs  int  `json:"new_msgs"`
	NewPairs int  `json:"new_pairs"`
	Full     bool `json:"full"`
}

// Handlers are the write operations the CLI can ask the daemon to run.
// Reads are served directly from the store.
type Handlers struct {
	Ingest func(db *store.DB, roots []string) (IngestResult, error)
	Index  func(db *store.DB, minDF int, full bool) (IndexResult, error)
	Rename func(db *store.DB, old, new string) (store.RenameResult, error)
}

type job struct {
	kind Kind
	fn   func(context.Context, *store.DB) (any, error)
	// A repack job is handled by the worker itself: it closes the shared
	// handle, rebuilds the file, and reopens before the next job runs.
	repack *repackJob
	// fresh reopens the handle after the job. After the graph tables are
	// dropped and recreated, the next rewrite of them in the same DuckDB
	// instance misreads its own ZSTD segments ("ZSTD Decompression
	// failed"); a fresh handle, like a separate process, does not.
	fresh bool
	out   chan result
}

// repackJob is one repack request: an empty out swaps the live file.
type repackJob struct {
	out string
}

type result struct {
	val any
	err error
}

// Owner holds the database and its socket.
type Owner struct {
	db          *store.DB
	path        string
	lock        *os.File
	ln          net.Listener
	jobs        chan job
	readTimeout time.Duration
	handlers    Handlers

	// fatalStreak counts consecutive write jobs that failed with a broken
	// transaction. The worker owns it; the mutex guards the pause below.
	fatalStreak int
	mu          sync.Mutex
	paused      bool
	pauseMsg    string

	wg         sync.WaitGroup
	workerDone chan struct{}
}

// LockPath is the flock file beside the database.
func LockPath(dbPath string) string { return dbPath + ".owner" }

// SocketPath is the daemon socket beside the database.
func SocketPath(dbPath string) string { return dbPath + ".sock" }

// TryLock takes the owner lock without waiting. The returned function
// releases it. ErrBusy means the daemon or another command holds it.
func TryLock(dbPath string) (func(), error) {
	f, err := os.OpenFile(LockPath(dbPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}

// Start opens the database, listens on its socket, and runs the queue.
// A second process that finds the lock held gets ErrBusy.
func Start(dbPath string, h Handlers) (*Owner, error) {
	if len(SocketPath(dbPath)) >= 100 {
		return nil, fmt.Errorf("database path is too long for a unix socket: %s", dbPath)
	}
	f, err := os.OpenFile(LockPath(dbPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w; the daemon is already running", ErrBusy)
		}
		return nil, err
	}
	db, err := store.Open(dbPath)
	if err != nil {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
		return nil, err
	}
	sock := SocketPath(dbPath)
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = db.Close()
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(sock)
		_ = db.Close()
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
		return nil, err
	}
	o := &Owner{
		db:          db,
		path:        dbPath,
		lock:        f,
		ln:          ln,
		jobs:        make(chan job),
		readTimeout: AgentReadTimeout,
		handlers:    h,
		workerDone:  make(chan struct{}),
	}
	go o.loop()
	o.wg.Add(1)
	go o.accept()
	return o, nil
}

// Close stops accepting, finishes queued work, and releases the database.
func (o *Owner) Close() error {
	_ = o.ln.Close()
	o.wg.Wait()
	close(o.jobs)
	<-o.workerDone
	var err error
	// A failed reopen or repack leaves no handle to close.
	if o.db != nil {
		err = o.db.Close()
	}
	_ = os.Remove(SocketPath(o.path))
	_ = unix.Flock(int(o.lock.Fd()), unix.LOCK_UN)
	_ = o.lock.Close()
	return err
}

// Do runs fn on the single worker. A read is cancelled AgentReadTimeout
// after it starts; a write is not. Queue wait is not limited and is not
// cancelled when the caller gives up.
func (o *Owner) Do(kind Kind, fn func(context.Context, *store.DB) (any, error)) (any, error) {
	return o.run(job{kind: kind, fn: fn})
}

// doFresh runs a write that rebuilds the graph tables and reopens the
// handle afterwards.
func (o *Owner) doFresh(fn func(context.Context, *store.DB) (any, error)) (any, error) {
	return o.run(job{kind: KindWrite, fn: fn, fresh: true})
}

func (o *Owner) run(j job) (any, error) {
	j.out = make(chan result, 1)
	o.jobs <- j
	r := <-j.out
	return r.val, r.err
}

// Repack rebuilds the database file as a write job. The worker closes the
// database for the duration, so no job can hold a handle to the file being
// replaced. Out writes a verified copy and leaves the live file in place.
func (o *Owner) Repack(out string) (store.RepackResult, error) {
	j := job{kind: KindWrite, repack: &repackJob{out: out}, out: make(chan result, 1)}
	o.jobs <- j
	r := <-j.out
	if r.err != nil {
		return store.RepackResult{}, r.err
	}
	res, _ := r.val.(store.RepackResult)
	return res, nil
}

// Paused reports that writes stopped after repeated transaction errors. The
// daemon uses it to stop cycling until the database is repaired.
func (o *Owner) Paused() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.paused
}

func (o *Owner) loop() {
	defer close(o.workerDone)
	for j := range o.jobs {
		if j.repack != nil {
			val, err := o.runRepack(*j.repack)
			j.out <- result{val, err}
			continue
		}
		if j.kind == KindWrite {
			if err := o.writesPausedError(); err != nil {
				j.out <- result{nil, err}
				continue
			}
		}
		if o.db == nil {
			// The last reopen failed; nothing but a repack or a restart
			// can bring the file back.
			j.out <- result{nil, o.pausedError()}
			continue
		}
		ctx := context.Background()
		var cancel context.CancelFunc
		if j.kind == KindRead {
			ctx, cancel = context.WithTimeout(context.Background(), o.readTimeout)
		}
		val, err := j.fn(ctx, o.db)
		timedOut := j.kind == KindRead && ctx.Err() != nil
		if cancel != nil {
			cancel()
		}
		if timedOut {
			err = fmt.Errorf("query timed out after %s", o.readTimeout)
		}
		if j.kind == KindWrite {
			o.observeWrite(err)
		}
		if j.fresh && o.db != nil {
			o.reopen(errors.New("graph rebuild"))
		}
		j.out <- result{val, err}
	}
}

// fatalDBError reports whether err left DuckDB's transaction manager
// unusable: "Failed to commit", "Failed to rollback", and "Conflict on
// update" all arrive with this prefix, and the handle cannot commit again.
func fatalDBError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "TransactionContext Error")
}

// observeWrite reopens the database after a broken transaction and, when it
// breaks twice without a clean write in between, pauses writes. Without the
// pause a corrupt file fails every cycle forever with the same error.
func (o *Owner) observeWrite(err error) {
	if err == nil {
		o.fatalStreak = 0
		return
	}
	if !fatalDBError(err) {
		return
	}
	o.fatalStreak++
	o.reopen(err)
	if o.fatalStreak >= 2 {
		o.pause(err)
	}
}

func (o *Owner) reopen(cause error) {
	if err := o.db.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close database after %v: %v\n", cause, err)
	}
	db, err := store.Open(o.path)
	if err != nil {
		// No usable handle: every job is refused until a repack succeeds
		// or the service restarts.
		o.db = nil
		o.pause(err)
		fmt.Fprintf(os.Stderr, "reopen database after %v: %v\n", cause, err)
		return
	}
	o.db = db
}

func (o *Owner) pause(err error) {
	o.mu.Lock()
	if !o.paused {
		o.paused = true
		o.pauseMsg = err.Error()
	}
	o.mu.Unlock()
	fmt.Fprintf(os.Stderr, "writes paused after database errors: %v\n", err)
}

func (o *Owner) resume() {
	o.mu.Lock()
	o.paused = false
	o.pauseMsg = ""
	o.mu.Unlock()
	o.fatalStreak = 0
}

func (o *Owner) writesPausedError() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.paused {
		return nil
	}
	return fmt.Errorf("writes are paused after database errors (%s); repair the database with duck-mem repack or restart duck-mem.service", o.pauseMsg)
}

func (o *Owner) pausedError() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.paused {
		return fmt.Errorf("the database is not open")
	}
	return fmt.Errorf("the database needs repair (%s); run duck-mem repack or restart duck-mem.service", o.pauseMsg)
}

// runRepack closes the database, rewrites the file, and reopens. With an
// out path it writes a verified copy and leaves the live file alone; without
// one it replaces the live file, and only that clears a pause, because only
// then is the file the daemon writes to the one that was verified.
func (o *Owner) runRepack(rj repackJob) (any, error) {
	if o.db != nil {
		closeErr := o.db.Close()
		o.db = nil
		if closeErr != nil {
			return nil, fmt.Errorf("close database for repack: %w", closeErr)
		}
	}
	var (
		res store.RepackResult
		err error
	)
	if rj.out != "" {
		res, err = store.RepackTo(o.path, rj.out)
	} else {
		res, err = store.RepackSwap(o.path)
	}
	db, reopenErr := store.Open(o.path)
	if reopenErr != nil {
		o.pause(reopenErr)
		return nil, fmt.Errorf("reopen database after repack: %w", reopenErr)
	}
	o.db = db
	if err != nil {
		if rj.out == "" && errors.Is(err, store.ErrRepacked) {
			// The rebuilt file is in place; only cleanup failed.
			o.resume()
		}
		return nil, err
	}
	if rj.out == "" {
		o.resume()
	}
	return res, nil
}

func (o *Owner) accept() {
	defer o.wg.Done()
	for {
		c, err := o.ln.Accept()
		if err != nil {
			return
		}
		o.wg.Add(1)
		go o.serve(c)
	}
}

func (o *Owner) serve(c net.Conn) {
	defer o.wg.Done()
	defer c.Close()
	for {
		req, err := readRequest(c)
		if err != nil {
			return
		}
		if err := writeFrame(c, o.dispatch(req)); err != nil {
			return
		}
	}
}

func (o *Owner) dispatch(req request) response {
	switch req.Op {
	case opQuery:
		val, err := o.Do(KindRead, func(ctx context.Context, db *store.DB) (any, error) {
			filter := store.ProjectFilter{Values: req.Projects, NoMatch: req.NoMatch}
			results, topics, err := db.RecallFiltered(ctx, req.Query, filter, req.Source, req.Limit, req.Hits)
			if err != nil {
				return nil, err
			}
			return queryPayload{Results: results, Topics: topics}, nil
		})
		return payloadResponse(val, err)
	case opResolveProject:
		val, err := o.Do(KindRead, func(ctx context.Context, db *store.DB) (any, error) {
			return db.ResolveProjectFilter(ctx, req.Project)
		})
		return payloadResponse(val, err)
	case opProjects:
		val, err := o.Do(KindRead, func(ctx context.Context, db *store.DB) (any, error) {
			return db.ProjectStats(ctx)
		})
		return payloadResponse(val, err)
	case opResolve:
		val, err := o.Do(KindRead, func(ctx context.Context, db *store.DB) (any, error) {
			return db.ResolveSession(ctx, req.Ref)
		})
		return payloadResponse(val, err)
	case opWindow:
		val, err := o.Do(KindRead, func(ctx context.Context, db *store.DB) (any, error) {
			return db.Window(ctx, req.Ref, req.From, req.To, req.Keep, req.WithContext)
		})
		return payloadResponse(val, err)
	case opRelated:
		val, err := o.Do(KindRead, func(ctx context.Context, db *store.DB) (any, error) {
			filter := store.ProjectFilter{Values: req.Projects, NoMatch: req.NoMatch}
			return db.RelatedFiltered(ctx, req.Term, filter, req.Depth, req.Limit)
		})
		return payloadResponse(val, err)
	case opIngest:
		if o.handlers.Ingest == nil {
			return response{Error: "ingest is not served by this process"}
		}
		val, err := o.Do(KindWrite, func(_ context.Context, db *store.DB) (any, error) {
			return o.handlers.Ingest(db, req.Roots)
		})
		return payloadResponse(val, err)
	case opIndex:
		if o.handlers.Index == nil {
			return response{Error: "index is not served by this process"}
		}
		index := func(_ context.Context, db *store.DB) (any, error) {
			return o.handlers.Index(db, req.MinDF, req.Full)
		}
		var val any
		var err error
		if req.Full {
			val, err = o.doFresh(index)
		} else {
			val, err = o.Do(KindWrite, index)
		}
		return payloadResponse(val, err)
	case opRename:
		if o.handlers.Rename == nil {
			return response{Error: "rename is not served by this process"}
		}
		val, err := o.doFresh(func(_ context.Context, db *store.DB) (any, error) {
			return o.handlers.Rename(db, req.OldProject, req.NewProject)
		})
		return payloadResponse(val, err)
	case opRepack:
		val, err := o.Repack(req.Out)
		return payloadResponse(val, err)
	default:
		return response{Error: "unknown operation " + req.Op}
	}
}

func payloadResponse(val any, err error) response {
	if err != nil {
		return response{Error: err.Error()}
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return response{Error: err.Error()}
	}
	return response{Payload: raw}
}

func readRequest(r io.Reader) (request, error) {
	var req request
	err := readJSON(r, &req)
	return req, err
}

func readResponse(r io.Reader) (response, error) {
	var resp response
	err := readJSON(r, &resp)
	return resp, err
}

func readJSON(r io.Reader, dest any) error {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return err
	}
	if n > maxFrame {
		return fmt.Errorf("frame is %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, dest)
}

func writeFrame(w io.Writer, v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(buf) > maxFrame {
		buf, _ = json.Marshal(response{Error: "response is too large"})
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(buf))); err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

func writeRequest(w io.Writer, req request) error { return writeFrame(w, req) }
