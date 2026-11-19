package ingest

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmarceno/duck-mem/internal/project"
	"github.com/jmarceno/duck-mem/internal/store"
)

// Google Antigravity keeps one append-only JSONL step log per conversation at
// ~/.gemini/antigravity/brain/<conversation-uuid>/.system_generated/logs/
// transcript_full.jsonl. Each record is one step: who produced it, its type,
// when it finished, and either the visible text or a tool call.
//
// Kept: the prompts the user actually sent and the assistant's visible
// replies. The harness wraps a prompt in <USER_REQUEST> tags and appends
// metadata blocks (local time, model selection notices) that the model needs
// but memory does not, so only the tagged request is stored.
//
// Dropped everywhere: GENERIC records (tool output: command output, file
// listings, search results), the thinking field (reasoning), tool_calls, and
// any record whose source is not the user themselves or the model
// (USER_EXPLICIT and MODEL) so harness-injected context cannot leak in.
//
// transcript.jsonl next to the full log is the same conversation with long
// content fields clipped, and chunks/ holds partitions of both; only the full
// log is classified as ingestible so one conversation cannot be imported
// twice.
//
// Temporary limitations, kept until there is data to design against:
//
//   - Failed and aborted turns produce no note. Every step observed so far
//     reported DONE, so the record shape a failure or a user cancellation
//     writes is unknown. Nothing is guessed: a turn that ended without a
//     reply just looks like it stopped, unlike Codex and Cursor, which emit a
//     "turn failed" or "turn aborted" note.
//   - Subagent conversations are not linked to the conversation that spawned
//     them. conversation_summaries.db carries parent_conversation_id and
//     nesting_depth, but no subagent brain directory was observed, so it is
//     unknown whether a subagent gets one of its own. One that does is
//     ingested as an ordinary unlinked session.
//   - A conversation stored only as chunks/ would be skipped. Both the full
//     log and its chunks existed side by side in every sample, and the chunks
//     concatenated to the full log byte for byte. If a long conversation ever
//     ships chunks without the full log, its brain directory classifies as
//     unknown and nothing is ingested, rather than half-importing it or
//     duplicating the conversation.
const antigravitySource = "antigravity"

// antigravityEnvelope marks the prompt as the user typed it, before the
// harness appended its own metadata blocks.
const (
	antigravityRequestOpen  = "<USER_REQUEST>"
	antigravityRequestClose = "</USER_REQUEST>"
	antigravityMetadataOpen = "<ADDITIONAL_METADATA>"
)

// antigravityPrompt keeps only the text the user typed.
func antigravityPrompt(content string) string {
	if i := strings.Index(content, antigravityRequestOpen); i >= 0 {
		rest := content[i+len(antigravityRequestOpen):]
		if j := strings.Index(rest, antigravityRequestClose); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	// No request tags: everything after this marker is harness metadata.
	if i := strings.Index(content, antigravityMetadataOpen); i >= 0 {
		return content[:i]
	}
	return content
}

// antigravityModelEffort splits the reasoning level Antigravity encodes in the
// model id (gemini-3.8-flash-high). Only -high and -low are levels; suffixes
// like -tiered, -n and -lite are part of the model name, so they stay whole
// and the effort stays Unknown.
func antigravityModelEffort(id string) (model, effort string) {
	for _, level := range []string{"high", "low"} {
		suffix := "-" + level
		if base := strings.TrimSuffix(id, suffix); base != "" && base != id {
			return base, level
		}
	}
	return id, ""
}

// AntigravityDir returns the Antigravity state directory that holds the
// conversation summaries index for a brain transcript path.
func AntigravityDir(path string) string {
	const marker = "/.gemini/antigravity/brain/"
	if i := strings.Index(path, marker); i >= 0 {
		return path[:i+len("/.gemini/antigravity")]
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "antigravity")
}

// antigravityConversationID is the brain directory name, which is also the key
// the summaries index files the conversation under.
func antigravityConversationID(path string) string {
	if i := strings.Index(path, "/brain/"); i >= 0 {
		rest := path[i+len("/brain/"):]
		if id, _, ok := strings.Cut(rest, "/"); ok && id != "" {
			return id
		}
	}
	// <brain>/<id>/.system_generated/logs/transcript_full.jsonl
	return filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
}

// AntigravityProject returns the workspace the conversation ran in, or "" when
// the summaries index has not resolved it. Unlike Cursor's slug there is no
// synthetic fallback: a conversation outside any project stays project-less
// rather than inventing a path.
func AntigravityProject(path string, projects map[string]string) string {
	return project.Clean(projects[antigravityConversationID(path)])
}

// LoadAntigravityProjects reads the conversation-to-workspace mapping from
// Antigravity's summaries index (~/.gemini/antigravity/
// conversation_summaries.db). The index is optional and lags the transcripts:
// a missing file, a store without the table, or an unusable workspace_uris
// value leaves that conversation unresolved instead of failing the cycle.
func LoadAntigravityProjects(dir string) map[string]string {
	projects := map[string]string{}
	dbPath := filepath.Join(dir, "conversation_summaries.db")
	if _, err := os.Stat(dbPath); err != nil {
		return projects
	}
	db, tx, err := snapshotTx(dbPath, "Antigravity")
	if err != nil {
		return projects
	}
	defer db.Close()
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT conversation_id, workspace_uris FROM conversation_summaries`)
	if err != nil {
		return projects
	}
	defer rows.Close()
	for rows.Next() {
		var id, uris string
		if err := rows.Scan(&id, &uris); err != nil {
			return projects
		}
		if folder := antigravityWorkspace(uris); folder != "" {
			projects[id] = folder
		}
	}
	return projects
}

// antigravityWorkspace picks the folder from workspace_uris, a JSON array of
// file:// URIs. Anything that is not an absolute local path is ignored.
func antigravityWorkspace(uris string) string {
	var list []string
	if json.Unmarshal([]byte(uris), &list) != nil {
		return ""
	}
	for _, raw := range list {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "file" || u.Host != "" || !filepath.IsAbs(u.Path) {
			continue
		}
		return project.Clean(u.Path)
	}
	return ""
}

func parseAntigravity(path string, projects map[string]string) (store.Session, []store.Message, error) {
	sess := newSession(antigravitySource+":"+antigravityConversationID(path), antigravitySource, AntigravityProject(path, projects), path, time.Time{})
	return parseAntigravityFrom(path, 0, sess, nil, projects)
}

func parseAntigravityFrom(path string, offset int64, sess store.Session, last *store.Message, projects map[string]string) (store.Session, []store.Message, error) {
	c := newCollector(sess, last)
	started := sess.StartedAt
	model, effort := sess.Model, sess.Effort
	err := readLinesFrom(path, offset, func(o map[string]any) {
		at := parseTime(o["created_at"])
		if started.IsZero() && !at.IsZero() {
			started = at
		}
		// The model id on the step that ran is the newest selection; later
		// records win, so a model switch mid-session keeps the final value.
		if v, _ := o["model"].(string); v != "" {
			model, effort = antigravityModelEffort(v)
		}
		source, _ := o["source"].(string)
		switch typ, _ := o["type"].(string); typ {
		case "USER_INPUT":
			// Only the user's own turns: the harness also writes its context
			// as user input under a different source.
			if source != "" && source != "USER_EXPLICIT" {
				return
			}
			c.add("user", antigravityPrompt(str(o, "content")), at)
		case "PLANNER_RESPONSE":
			// A step that only called tools carries tool_calls and no
			// content, so the reply is the content of the step that has it.
			c.add("assistant", str(o, "content"), at)
		}
		// GENERIC carries tool output, thinking carries reasoning, and any
		// other record type is harness context: all dropped.
	})
	if err != nil {
		return sess, nil, err
	}
	sess = newSession(sess.ID, antigravitySource, AntigravityProject(path, projects), path, started)
	sess.Model, sess.Effort = model, effort
	c.sess = sess
	for i := range c.msgs {
		c.msgs[i].SessionID = sess.ID
		c.msgs[i].Project = sess.Project
	}
	return finalize(sess, c.lastAt), c.msgs, nil
}
