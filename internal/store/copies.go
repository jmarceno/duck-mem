package store

import (
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

// Only known transport envelopes are removed. Code, internal whitespace,
// message order and failure notes remain significant; there is no fuzzy match.
var queryEnvelope = regexp.MustCompile(`(?s)^(?:<timestamp>[^<]*</timestamp>\s*)?<user_query>\s*(.*?)\s*</user_query>$`)
var timestampEnvelope = regexp.MustCompile(`^<timestamp>[^<]*</timestamp>$`)

type copyIdentity struct {
	Hashes   []string
	Eligible bool
	Imported bool
}

type identityBuilder struct {
	copyIdentity
	bytes           int
	user, assistant bool
}

func (b *identityBuilder) add(role, text string) {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "<EXTERNAL SESSION IMPORTED>" {
		b.Imported = true
		return
	}
	if role == "system" {
		return
	}
	if role == "user" {
		if m := queryEnvelope.FindStringSubmatch(text); m != nil {
			text = m[1]
		}
		if timestampEnvelope.MatchString(text) {
			return
		}
		for _, prefix := range []string{"# AGENTS.md instructions for ", "<environment_context>", "<external_codex_apps_open_page>", "<permissions instructions>", "<collaboration_mode>", "<skills_instructions>"} {
			if strings.HasPrefix(text, prefix) {
				return
			}
		}
	}
	if text == "" {
		return
	}
	hash := sha256.Sum256([]byte(role + "\x00" + text))
	b.Hashes = append(b.Hashes, hex.EncodeToString(hash[:]))
	b.bytes += len(text)
	b.user = b.user || role == "user"
	b.assistant = b.assistant || role == "assistant" && len(text) >= 100
	b.Eligible = len(b.Hashes) >= 3 && b.bytes >= 500 && b.user && b.assistant
}

func identityOf(msgs []Message) copyIdentity {
	var b identityBuilder
	for _, m := range msgs {
		b.add(m.Role, m.Text)
	}
	return b.copyIdentity
}

type copySession struct {
	Session
	copyIdentity
	DuplicateOf string
	SeenAt      time.Time
}

func coveredBy(a, b copySession) bool {
	return a.ID != b.ID && a.Source != b.Source && a.Project != "" && a.Project == b.Project &&
		a.Eligible && len(a.Hashes) <= len(b.Hashes) && slices.Equal(a.Hashes, b.Hashes[:len(a.Hashes)])
}

func identityValues(id string, c copyIdentity) []driver.Value {
	hashes, _ := json.Marshal(c.Hashes)
	return []driver.Value{id, string(hashes), c.Eligible, c.Imported, "", time.Now()}
}

// PrepareSessionCopies backfills identities from the existing database once,
// including backup-only sessions. It never needs the original logs to do so.
func (db *DB) PrepareSessionCopies() error {
	rows, err := db.sql.Query(`SELECT s.session_id, m.role, m.text FROM sessions s
  LEFT JOIN messages m USING(session_id)
  WHERE NOT EXISTS(SELECT 1 FROM session_copies c WHERE c.session_id=s.session_id)
  ORDER BY s.session_id, m.seq`)
	if err != nil {
		return err
	}
	var values [][]driver.Value
	var id string
	var b identityBuilder
	for rows.Next() {
		var sid string
		var role, text sql.NullString
		if err := rows.Scan(&sid, &role, &text); err != nil {
			rows.Close()
			return err
		}
		if sid != id {
			if id != "" {
				values = append(values, identityValues(id, b.copyIdentity))
			}
			id, b = sid, identityBuilder{}
		}
		if role.Valid {
			b.add(role.String, text.String)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if id != "" {
		values = append(values, identityValues(id, b.copyIdentity))
	}
	if len(values) == 0 {
		return nil
	}
	return db.withTx(func(conn *duckdb.Conn) error { return appendTable(conn, "session_copies", values) })
}

func (db *DB) refreshCopyIdentity(id string) error {
	rows, err := db.sql.Query(`SELECT role, text FROM messages WHERE session_id=? ORDER BY seq`, id)
	if err != nil {
		return err
	}
	var b identityBuilder
	for rows.Next() {
		var role, text string
		if err := rows.Scan(&role, &text); err != nil {
			rows.Close()
			return err
		}
		b.add(role, text)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	values := identityValues(id, b.copyIdentity)
	_, err = db.sql.Exec(`INSERT INTO session_copies(session_id, hashes, eligible, imported, duplicate_of) VALUES(?, ?, ?, ?, ?)
  ON CONFLICT(session_id) DO UPDATE SET hashes=excluded.hashes,
  eligible=excluded.eligible, imported=excluded.imported, duplicate_of=''`, values[0], values[1], values[2], values[3], values[4])
	return err
}

func (db *DB) copySessions(owner string) ([]copySession, error) {
	filter := ""
	var args []any
	if owner != "" {
		filter = " WHERE c.duplicate_of=?"
		args = append(args, owner)
	}
	rows, err := db.sql.Query(`SELECT s.session_id, s.source, s.project, s.path,
  c.hashes, c.eligible, c.imported, c.duplicate_of, c.seen_at
  FROM sessions s JOIN session_copies c USING(session_id)`+filter+` ORDER BY s.session_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []copySession
	for rows.Next() {
		var s copySession
		var hashes string
		if err := rows.Scan(&s.ID, &s.Source, &s.Project, &s.Path, &hashes, &s.Eligible, &s.Imported, &s.DuplicateOf, &s.SeenAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(hashes), &s.Hashes); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (db *DB) IsSessionCopy(id string) (bool, error) {
	var copy bool
	err := db.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM session_copies WHERE session_id=? AND duplicate_of<>'')`, id).Scan(&copy)
	return copy, err
}

// Old read-only databases may predate the additive copies table.
func (db *DB) canonicalSessionID(id string) (string, error) {
	var available bool
	if err := db.queryRow(`SELECT EXISTS(SELECT 1 FROM duckdb_tables()
		WHERE database_name=current_database() AND table_name='session_copies')`).Scan(&available); err != nil {
		return "", err
	}
	if !available {
		return id, nil
	}
	var target string
	err := db.queryRow(`SELECT duplicate_of FROM session_copies WHERE session_id=?`, id).Scan(&target)
	if err == sql.ErrNoRows || err == nil && target == "" {
		return id, nil
	}
	return target, err
}

// CopyLoader reads a suppressed session's full current snapshot. A failed
// read prevents rewriting its owner, so unavailable logs cannot lose history.
type CopyLoader func(Session) (Session, []Message, error)

// SyncConversation also protects suppressed dependents before a rewrite.
// Appends preserve their prefixes; changed copies always take the full path.
func (db *DB) SyncConversation(s Session, msgs []Message, appended bool, load CopyLoader) error {
	if !appended {
		sessions, err := db.copySessions(s.ID)
		if err != nil {
			return err
		}
		target := copySession{Session: s, copyIdentity: identityOf(msgs)}
		for _, c := range sessions {
			if c.DuplicateOf != s.ID || coveredBy(c, target) {
				continue
			}
			if load == nil {
				return fmt.Errorf("copy loader required before replacing %s", s.ID)
			}
			restored, snapshot, err := load(c.Session)
			if err != nil {
				return fmt.Errorf("restore copy %s before replacing %s: %w", c.ID, s.ID, err)
			}
			if restored.ID != c.ID {
				return fmt.Errorf("restored copy changed session ID: %s", c.ID)
			}
			// The snapshot may include bytes beyond its old checkpoint. Force
			// reconciliation instead of appending those messages a second time.
			if _, err := db.sql.Exec(`DELETE FROM file_checkpoints WHERE session_id=? OR path=?`, c.ID, c.Path); err != nil {
				return err
			}
			if err := db.UpsertSession(restored); err != nil {
				return err
			}
			if err := db.SyncMessages(c.ID, snapshot); err != nil {
				return err
			}
			if err := db.refreshCopyIdentity(c.ID); err != nil {
				return err
			}
		}
	}
	if err := db.UpsertSession(s); err != nil {
		return err
	}
	if appended && len(msgs) == 0 {
		return nil
	}
	var err error
	if appended {
		err = db.InsertMessages(msgs)
	} else {
		err = db.SyncMessages(s.ID, msgs)
	}
	if err != nil {
		return err
	}
	return db.refreshCopyIdentity(s.ID)
}

// SuppressSessionCopies keeps owners stable and removes only fully covered
// cross-harness snapshots. Identical copies prefer a source without an import
// marker. Dependencies are flat: an owner with copies is never suppressed.
func (db *DB) SuppressSessionCopies() (int, error) {
	sessions, err := db.copySessions("")
	if err != nil {
		return 0, err
	}
	owners := map[string]bool{}
	for _, s := range sessions {
		if s.DuplicateOf != "" {
			owners[s.DuplicateOf] = true
		}
	}
	slices.SortStableFunc(sessions, func(a, b copySession) int {
		if owners[a.ID] != owners[b.ID] {
			if owners[a.ID] {
				return -1
			}
			return 1
		}
		if a.Imported != b.Imported {
			if !a.Imported {
				return -1
			}
			return 1
		}
		if order := a.SeenAt.Compare(b.SeenAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	kept := map[string][]copySession{}
	count := 0
	for _, s := range sessions {
		if s.DuplicateOf != "" || !s.Eligible {
			continue
		}
		key := s.Project + "\x00" + s.Hashes[0]
		var target string
		if !owners[s.ID] {
			for _, k := range kept[key] {
				if coveredBy(s, k) {
					target = k.ID
					break
				}
			}
		}
		if target == "" {
			kept[key] = append(kept[key], s)
			continue
		}
		undo, err := db.sessionContribution(s.ID)
		if err != nil {
			return count, err
		}
		err = db.withTx(func(conn *duckdb.Conn) error {
			if err := undo.apply(conn, s.ID); err != nil {
				return err
			}
			for _, table := range []string{"msg_terms", "messages"} {
				if err := execConn(conn, `DELETE FROM `+table+` WHERE session_id=`+sqlString(s.ID)); err != nil {
					return err
				}
			}
			return execConn(conn, `UPDATE session_copies SET duplicate_of=`+sqlString(target)+` WHERE session_id=`+sqlString(s.ID))
		})
		if err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
