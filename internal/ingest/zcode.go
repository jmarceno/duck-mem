package ingest

import (
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

	rows, err := tx.Query(`SELECT id, COALESCE(NULLIF(directory, ''), ''), time_created
		FROM session ORDER BY time_created, id`)
	if err != nil {
		return nil, openCodeReadError("read ZCode sessions", err)
	}
	var sessions []Snapshot
	var rawIDs []string
	for rows.Next() {
		var id, dir string
		var created int64
		if err := rows.Scan(&id, &dir, &created); err != nil {
			rows.Close()
			return nil, err
		}
		sessions = append(sessions, Snapshot{Session: store.Session{
			ID: "zcode:" + strings.TrimPrefix(id, "sess_"), Source: "zcode",
			Project: project.Clean(dir), Path: path, StartedAt: sqliteTime(created),
		}})
		rawIDs = append(rawIDs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	for i := range sessions {
		s := &sessions[i]
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
					Role string `json:"role"`
				}
				if err := json.Unmarshal([]byte(msgData), &msg); err != nil {
					rows.Close()
					return nil, fmt.Errorf("%w: decode ZCode message in %s", ErrUnexpectedFormat, s.Session.ID)
				}
				role, at = msg.Role, sqliteTime(created)
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
	}
	return sessions, nil
}
