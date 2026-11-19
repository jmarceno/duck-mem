package ingest

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jmarceno/duck-mem/internal/project"
	"github.com/jmarceno/duck-mem/internal/store"
	"github.com/mattn/go-sqlite3"
)

// snapshotTx opens a live SQLite store read-only and starts a transaction so
// the whole file is read as one consistent snapshot.
func snapshotTx(path, source string) (*sql.DB, *sql.Tx, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_query_only", "1")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("open %s snapshot: %w", source, err)
	}
	return db, tx, nil
}

// IngestOpenCode reads a consistent snapshot of OpenCode's live database.
// Only visible text fields are decoded; tool and reasoning parts are excluded.
func IngestOpenCode(path string) ([]Snapshot, error) {
	return IngestOpenCodeSince(path, 0)
}

// IngestOpenCodeSince reads only sessions updated after since (Unix
// milliseconds). A zero since reads the whole store. Stores whose schema
// predates the update column fall back to a full read.
func IngestOpenCodeSince(path string, since int64) ([]Snapshot, error) {
	db, tx, err := snapshotTx(path, "OpenCode")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	defer tx.Rollback()

	sessions, err := openCodeSessions(tx, path, since)
	if err != nil {
		return nil, err
	}

	for i := range sessions {
		s := &sessions[i]
		id := strings.TrimPrefix(s.Session.ID, "opencode:")
		rows, err := tx.Query(`SELECT type, data, time_created FROM session_message
			WHERE session_id = ? ORDER BY seq, id`, id)
		if err != nil {
			return nil, openCodeReadError("read OpenCode messages", err)
		}
		for rows.Next() {
			var kind, data string
			var created int64
			if err := rows.Scan(&kind, &data, &created); err != nil {
				rows.Close()
				return nil, err
			}
			if kind != "user" && kind != "system" && kind != "assistant" {
				continue
			}
			text, err := openCodeText(kind, data)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("%w: decode OpenCode message in %s", ErrUnexpectedFormat, s.Session.ID)
			}
			if text == "" {
				continue
			}
			s.Messages = append(s.Messages, store.Message{
				SessionID: s.Session.ID, Seq: len(s.Messages), Source: "opencode",
				Project: s.Session.Project, Role: kind, Text: text, CreatedAt: sqliteTime(created),
			})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return sessions, nil
}

// openCodeSessions reads the session table with its model and update time
// when the live schema carries them, falling back to the columns older
// stores have. Missing columns are not an unexpected format: the session
// then just runs as Unknown with its creation date. A nonzero since keeps
// only sessions updated after it.
func openCodeSessions(tx *sql.Tx, storePath string, since int64) ([]Snapshot, error) {
	rows, err := tx.Query(`SELECT s.id, COALESCE(NULLIF(s.directory, ''), p.worktree, ''), s.time_created,
			s.model, s.time_updated
		FROM session_v2 s LEFT JOIN project p ON p.id = s.project_id
		WHERE (? = 0 OR COALESCE(s.time_updated, s.time_created) >= ?)
		ORDER BY s.time_created, s.id`, since, since)
	if err != nil {
		if !missingSchema(err) {
			return nil, openCodeReadError("read OpenCode sessions", err)
		}
		return openCodeSessionsLegacy(tx, storePath)
	}
	return scanOpenCodeSessions(rows, storePath, true)
}

func openCodeSessionsLegacy(tx *sql.Tx, storePath string) ([]Snapshot, error) {
	rows, err := tx.Query(`SELECT s.id, COALESCE(NULLIF(s.directory, ''), p.worktree, ''), s.time_created
		FROM session_v2 s LEFT JOIN project p ON p.id = s.project_id ORDER BY s.time_created, s.id`)
	if err != nil {
		return nil, openCodeReadError("read OpenCode sessions", err)
	}
	return scanOpenCodeSessions(rows, storePath, false)
}

func scanOpenCodeSessions(rows *sql.Rows, storePath string, enriched bool) ([]Snapshot, error) {
	var sessions []Snapshot
	for rows.Next() {
		var id, dir string
		var created int64
		var model sql.NullString
		var updated sql.NullInt64
		if enriched {
			if err := rows.Scan(&id, &dir, &created, &model, &updated); err != nil {
				rows.Close()
				return nil, err
			}
		} else if err := rows.Scan(&id, &dir, &created); err != nil {
			rows.Close()
			return nil, err
		}
		sess := store.Session{
			ID: "opencode:" + id, Source: "opencode", Project: project.Clean(dir),
			Path: storePath, StartedAt: sqliteTime(created), UpdatedAt: sqliteTime(updated.Int64),
		}
		if enriched {
			var m openCodeModel
			if model.Valid && json.Unmarshal([]byte(model.String), &m) == nil && m.ID != "" {
				sess.Model = m.ID
				// The variant names the model flavour the session ran,
				// which is as close to an effort level as OpenCode keeps.
				sess.Effort = m.Variant
			}
		}
		sessions = append(sessions, Snapshot{Session: finalize(sess, time.Time{})})
	}
	err := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

// openCodeModel decodes session_v2.model JSON.
type openCodeModel struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant"`
}

// missingSchema reports whether a live-store read failed because its
// schema predates the columns the enriched query selects. Older stores
// are read with the columns they have rather than reported as an
// unexpected format.
func missingSchema(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "no such column") || strings.Contains(err.Error(), "no such table"))
}

func sqliteTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func openCodeText(kind, data string) (string, error) {
	if kind != "assistant" {
		var msg struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			return "", err
		}
		return strings.TrimSpace(msg.Text), nil
	}
	var msg struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal([]byte(data), &msg); err != nil {
		return "", err
	}
	var parts []string
	for _, raw := range msg.Content {
		var part struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &part); err != nil {
			return "", err
		}
		if part.Type != "text" {
			continue
		}
		var visible struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &visible); err != nil {
			return "", err
		}
		if strings.TrimSpace(visible.Text) != "" {
			parts = append(parts, strings.TrimSpace(visible.Text))
		}
	}
	return strings.Join(parts, "\n"), nil
}

func openCodeReadError(operation string, err error) error {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code == sqlite3.ErrError && (strings.Contains(err.Error(), "no such table") || strings.Contains(err.Error(), "no such column")) {
		return fmt.Errorf("%w: %s: %v", ErrUnexpectedFormat, operation, err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
