package ingest

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jmarceno/duck-mem/internal/store"

	_ "github.com/mattn/go-sqlite3"
)

// OpenCodeSession is one conversation from OpenCode's multi-session SQLite store.
type OpenCodeSession struct {
	Session  store.Session
	Messages []store.Message
}

// IngestOpenCode reads a consistent snapshot of OpenCode's live database.
// Only visible text fields are decoded; tool and reasoning parts are excluded.
func IngestOpenCode(path string) ([]OpenCodeSession, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_query_only", "1")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("open OpenCode snapshot: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT s.id, COALESCE(NULLIF(s.directory, ''), p.worktree, ''), s.time_created
		FROM session_v2 s LEFT JOIN project p ON p.id = s.project_id ORDER BY s.time_created, s.id`)
	if err != nil {
		return nil, fmt.Errorf("read OpenCode sessions: %w", err)
	}
	var sessions []OpenCodeSession
	for rows.Next() {
		var id, project string
		var created int64
		if err := rows.Scan(&id, &project, &created); err != nil {
			rows.Close()
			return nil, err
		}
		sessions = append(sessions, OpenCodeSession{Session: store.Session{
			ID: "opencode:" + id, Source: "opencode", Project: project,
			Path: path, StartedAt: sqliteTime(created),
		}})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	for i := range sessions {
		s := &sessions[i]
		id := strings.TrimPrefix(s.Session.ID, "opencode:")
		rows, err := tx.Query(`SELECT type, data, time_created FROM session_message
			WHERE session_id = ? ORDER BY seq, id`, id)
		if err != nil {
			return nil, fmt.Errorf("read OpenCode messages: %w", err)
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
				return nil, fmt.Errorf("decode OpenCode message in %s: %w", s.Session.ID, err)
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
