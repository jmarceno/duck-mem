package ingest

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jmarceno/duck-mem/internal/project"
	"github.com/jmarceno/duck-mem/internal/store"
)

// IngestZCode reads a consistent snapshot of ZCode's live database
// (~/.zcode/cli/db/db.sqlite). Conversations live in the session, message and
// part tables; only visible text parts are decoded, tool and reasoning parts
// are excluded. Subagent sessions (parent_id set) are kept: their reports
// reach the main conversation only as tool output, which is dropped.
func IngestZCode(path string) ([]Snapshot, error) {
	db, tx, err := snapshotTx(path, "ZCode")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	defer tx.Rollback()

	sessions, rawIDs, err := zCodeSessions(tx, path)
	if err != nil {
		return nil, err
	}

	for i := range sessions {
		s := &sessions[i]
		zCodeModel(tx, rawIDs[i], s)
		rows, err := tx.Query(`SELECT m.id, m.time_created, m.data, p.data
			FROM message m JOIN part p ON p.message_id = m.id
			WHERE m.session_id = ? ORDER BY m.time_created, m.id, p.time_created, p.id`, rawIDs[i])
		if err != nil {
			return nil, openCodeReadError("read ZCode messages", err)
		}
		var (
			cur   string
			role  string
			at    time.Time
			texts []string
			// lastModelID is the model the newest assistant message ran
			// on; used only when the store keeps no model selection.
			lastModelID string
		)
		flush := func() {
			text := strings.TrimSpace(strings.Join(texts, "\n"))
			texts = nil
			switch role {
			case "user", "system", "assistant":
			default:
				return
			}
			if text == "" {
				return
			}
			s.Messages = append(s.Messages, store.Message{
				SessionID: s.Session.ID, Seq: len(s.Messages), Source: "zcode",
				Project: s.Session.Project, Role: role, Text: text, CreatedAt: at,
			})
		}
		for rows.Next() {
			var msgID, msgData, partData string
			var created int64
			if err := rows.Scan(&msgID, &created, &msgData, &partData); err != nil {
				rows.Close()
				return nil, err
			}
			if msgID != cur {
				flush()
				cur, texts = msgID, nil
				var msg struct {
					Role    string `json:"role"`
					ModelID string `json:"modelID"`
				}
				if err := json.Unmarshal([]byte(msgData), &msg); err != nil {
					rows.Close()
					return nil, fmt.Errorf("%w: decode ZCode message in %s", ErrUnexpectedFormat, s.Session.ID)
				}
				role, at = msg.Role, sqliteTime(created)
				if msg.ModelID != "" {
					lastModelID = msg.ModelID
				}
			}
			var part struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(partData), &part); err != nil {
				rows.Close()
				return nil, fmt.Errorf("%w: decode ZCode part in %s", ErrUnexpectedFormat, s.Session.ID)
			}
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				texts = append(texts, strings.TrimSpace(part.Text))
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		flush()
		if s.Session.Model == store.UnknownModel || s.Session.Model == "" {
			s.Session.Model = lastModelID
		}
	}
	return sessions, nil
}

// zCodeSessions reads the session table with its update time when the
// live schema carries it, falling back to the columns older stores have.
// Raw IDs are returned alongside because stored IDs carry a source prefix.
func zCodeSessions(tx *sql.Tx, storePath string) ([]Snapshot, []string, error) {
	rows, err := tx.Query(`SELECT id, COALESCE(NULLIF(directory, ''), ''), time_created, time_updated
		FROM session ORDER BY time_created, id`)
	if err != nil {
		if !missingSchema(err) {
			return nil, nil, openCodeReadError("read ZCode sessions", err)
		}
		sessions, rawIDs, err := zCodeSessionsLegacy(tx, storePath)
		return sessions, rawIDs, err
	}
	var sessions []Snapshot
	var rawIDs []string
	for rows.Next() {
		var id, dir string
		var created int64
		var updated sql.NullInt64
		if err := rows.Scan(&id, &dir, &created, &updated); err != nil {
			rows.Close()
			return nil, nil, err
		}
		sessions = append(sessions, Snapshot{Session: finalize(store.Session{
			ID: "zcode:" + strings.TrimPrefix(id, "sess_"), Source: "zcode",
			Project: project.Clean(dir), Path: storePath, StartedAt: sqliteTime(created),
			UpdatedAt: sqliteTime(updated.Int64),
		}, time.Time{})})
		rawIDs = append(rawIDs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	return sessions, rawIDs, nil
}

func zCodeSessionsLegacy(tx *sql.Tx, storePath string) ([]Snapshot, []string, error) {
	rows, err := tx.Query(`SELECT id, COALESCE(NULLIF(directory, ''), ''), time_created
		FROM session ORDER BY time_created, id`)
	if err != nil {
		return nil, nil, openCodeReadError("read ZCode sessions", err)
	}
	defer rows.Close()
	var sessions []Snapshot
	var rawIDs []string
	for rows.Next() {
		var id, dir string
		var created int64
		if err := rows.Scan(&id, &dir, &created); err != nil {
			return nil, nil, err
		}
		sessions = append(sessions, Snapshot{Session: finalize(store.Session{
			ID: "zcode:" + strings.TrimPrefix(id, "sess_"), Source: "zcode",
			Project: project.Clean(dir), Path: storePath, StartedAt: sqliteTime(created),
		}, time.Time{})})
		rawIDs = append(rawIDs, id)
	}
	return sessions, rawIDs, rows.Err()
}

// zCodeModel fills a session's model and effort from the store's model
// selection entry when one exists. Errors are ignored: a store without
// the entry table just runs as Unknown, and the message-level modelID
// fallback still applies afterwards.
func zCodeModel(tx *sql.Tx, rawID string, s *Snapshot) {
	var data string
	if err := tx.QueryRow(`SELECT data FROM session_entry
		WHERE session_id = ? AND type = 'runtime/model_selection'
		ORDER BY time_created DESC LIMIT 1`, rawID).Scan(&data); err != nil {
		return
	}
	var entry struct {
		ModelSelection struct {
			ModelID string `json:"modelId"`
			Options struct {
				ReasoningLevel string `json:"reasoningLevel"`
			} `json:"options"`
		} `json:"modelSelection"`
	}
	if json.Unmarshal([]byte(data), &entry) != nil {
		return
	}
	if entry.ModelSelection.ModelID != "" {
		s.Session.Model = entry.ModelSelection.ModelID
	}
	if entry.ModelSelection.Options.ReasoningLevel != "" {
		s.Session.Effort = entry.ModelSelection.Options.ReasoningLevel
	}
}
