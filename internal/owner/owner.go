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
	Rename func(db *store.DB, old, new string) (int, error)
}

type job struct {
	kind Kind
	fn   func(context.Context, *store.DB) (any, error)
	out  chan result
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
	err := o.db.Close()
	_ = os.Remove(SocketPath(o.path))
	_ = unix.Flock(int(o.lock.Fd()), unix.LOCK_UN)
	_ = o.lock.Close()
	return err
}

// Do runs fn on the single worker. A read is cancelled AgentReadTimeout
// after it starts; a write is not. Queue wait is not limited and is not
// cancelled when the caller gives up.
func (o *Owner) Do(kind Kind, fn func(context.Context, *store.DB) (any, error)) (any, error) {
	j := job{kind: kind, fn: fn, out: make(chan result, 1)}
	o.jobs <- j
	r := <-j.out
	return r.val, r.err
}

func (o *Owner) loop() {
	defer close(o.workerDone)
	for j := range o.jobs {
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
		j.out <- result{val, err}
	}
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
			results, topics, err := db.Recall(ctx, req.Query, req.Project, req.Source, req.Limit, req.Hits)
			if err != nil {
				return nil, err
			}
			return queryPayload{Results: results, Topics: topics}, nil
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
			return db.Related(ctx, req.Term, req.Project, req.Depth, req.Limit)
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
		val, err := o.Do(KindWrite, func(_ context.Context, db *store.DB) (any, error) {
			return o.handlers.Index(db, req.MinDF, req.Full)
		})
		return payloadResponse(val, err)
	case opRename:
		if o.handlers.Rename == nil {
			return response{Error: "rename is not served by this process"}
		}
		val, err := o.Do(KindWrite, func(_ context.Context, db *store.DB) (any, error) {
			return o.handlers.Rename(db, req.OldProject, req.NewProject)
		})
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
