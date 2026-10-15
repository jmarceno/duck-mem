package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jmarceno/duck-mem/internal/owner"
	"github.com/jmarceno/duck-mem/internal/store"
)

// memory is either a connection to the daemon that owns the database, or a
// direct open used only when that daemon is not running.
type memory struct {
	path   string
	client *owner.Client
	db     *store.DB
	close  func()
}

func (m *memory) Close() {
	if m != nil && m.close != nil {
		m.close()
	}
}

// openMemory dials the owner socket. If nothing is listening and the owner
// lock is free, it opens the file itself and holds the lock until Close.
// A held lock with no socket means the daemon is still starting: wait until
// one of those two changes.
func openMemory(path string, write bool) (*memory, error) {
	deadline := time.Now().Add(2 * time.Minute)
	var dialErr error
	for {
		if c, err := owner.Dial(path); err == nil {
			return &memory{path: path, client: c, close: func() { _ = c.Close() }}, nil
		} else {
			dialErr = err
		}
		unlock, err := owner.TryLock(path)
		if err == nil {
			db, err := openTolerant(path, !write)
			if err != nil {
				unlock()
				return nil, err
			}
			return &memory{path: path, db: db, close: func() { _ = db.Close(); unlock() }}, nil
		}
		if !errors.Is(err, owner.ErrBusy) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("database owner is not accepting connections: %w", dialErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readTimeout(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("query timed out after %s", owner.AgentReadTimeout)
	}
	return err
}

func (m *memory) Recall(query, project, source string, limit, hits int) ([]store.SessionResult, []store.Topic, error) {
	if m.client != nil {
		return m.client.Recall(query, project, source, limit, hits)
	}
	ctx, cancel := context.WithTimeout(context.Background(), owner.AgentReadTimeout)
	defer cancel()
	results, topics, err := m.db.Recall(ctx, query, project, source, limit, hits)
	return results, topics, readTimeout(ctx, err)
}

func (m *memory) ResolveSession(ref string) (store.SessionResult, error) {
	if m.client != nil {
		return m.client.ResolveSession(ref)
	}
	ctx, cancel := context.WithTimeout(context.Background(), owner.AgentReadTimeout)
	defer cancel()
	s, err := m.db.ResolveSession(ctx, ref)
	return s, readTimeout(ctx, err)
}

func (m *memory) Window(sessionID string, from, to, keep int, withContext bool) ([]store.Message, error) {
	if m.client != nil {
		return m.client.Window(sessionID, from, to, keep, withContext)
	}
	ctx, cancel := context.WithTimeout(context.Background(), owner.AgentReadTimeout)
	defer cancel()
	msgs, err := m.db.Window(ctx, sessionID, from, to, keep, withContext)
	return msgs, readTimeout(ctx, err)
}

func (m *memory) Related(term, project string, depth, limit int) ([]store.RelatedHit, error) {
	if m.client != nil {
		return m.client.Related(term, project, depth, limit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), owner.AgentReadTimeout)
	defer cancel()
	hits, err := m.db.Related(ctx, term, project, depth, limit)
	return hits, readTimeout(ctx, err)
}

func (m *memory) Ingest(roots []string) (owner.IngestResult, error) {
	if m.client != nil {
		return m.client.Ingest(roots)
	}
	return performCycle(m.db, m.path, roots)
}

func (m *memory) Index(minDF int, full bool) (owner.IndexResult, error) {
	if m.client != nil {
		return m.client.Index(minDF, full)
	}
	return performIndex(m.db, minDF, full)
}

func (m *memory) RenameProject(old, new string) (int, error) {
	if m.client != nil {
		return m.client.RenameProject(old, new)
	}
	return performRename(m.db, old, new)
}
