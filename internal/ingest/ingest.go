package ingest

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"duck-mem/internal/store"
)

// toolBlock excises inline tool-call dumps embedded in newer Codex message
// text: [external_agent_tool_call: Name] ... [/external_agent_tool_call].
var toolBlock = regexp.MustCompile(`(?s)\[external_agent_tool_call:[^\]]*\].*?\[/external_agent_tool_call\]`)

// Kind identifies a session file format by path.
type Kind int

const (
	KindUnknown Kind = iota
	KindCodex
	KindClaude
	KindMuse
	KindCursorTranscript
	KindCursorPlan
)

// Classify picks the parser from the file path.
func Classify(path string) Kind {
	switch {
	case strings.HasSuffix(path, ".plan.md"):
		return KindCursorPlan
	case strings.Contains(path, "/agent-transcripts/") && strings.HasSuffix(path, ".jsonl"):
		return KindCursorTranscript
	case filepath.Base(path) == "session.jsonl" && strings.Contains(path, "/muse/sessions/"):
		return KindMuse
	case strings.Contains(path, "/.codex/") && strings.HasSuffix(path, ".jsonl"):
		return KindCodex
	case strings.Contains(path, "/.claude/projects/") && strings.HasSuffix(path, ".jsonl"):
		return KindClaude
	}
	return KindUnknown
}

// DefaultRoots lists the session stores scanned when the user passes none.
func DefaultRoots(home string) []string {
	return []string{
		filepath.Join(home, ".codex", "sessions"),
		filepath.Join(home, ".codex", "archived_sessions"),
		filepath.Join(home, ".claude", "projects"),
		filepath.Join(home, ".local", "share", "muse", "sessions"),
		filepath.Join(home, ".cursor", "projects"),
		filepath.Join(home, ".cursor", "plans"),
	}
}

// Discover returns every ingestible file under roots.
func Discover(roots []string) []string {
	var out []string
	for _, r := range roots {
		_ = filepath.Walk(r, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if Classify(p) != KindUnknown {
				out = append(out, p)
			}
			return nil
		})
	}
	return out
}

// IngestFile parses one session file into a session plus kept messages.
func IngestFile(path string) (store.Session, []store.Message, error) {
	switch Classify(path) {
	case KindCodex:
		return parseCodex(path)
	case KindClaude:
		return parseClaude(path)
	case KindMuse:
		return parseMuse(path)
	case KindCursorTranscript:
		return parseCursorTranscript(path)
	case KindCursorPlan:
		return parseCursorPlan(path)
	}
	return store.Session{}, nil, nil
}

func newSession(id, source, project, path string, started time.Time) store.Session {
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return store.Session{ID: id, Source: source, Project: project, Path: path, StartedAt: started}
}

// collector assigns seq numbers and drops empty / duplicate texts.
type collector struct {
	sess store.Session
	msgs []store.Message
	seen map[string]bool
}

func (c *collector) add(role, text string, at time.Time) {
	text = strings.TrimSpace(text)
	if text == "" || c.seen[text] {
		return
	}
	c.seen[text] = true
	c.msgs = append(c.msgs, store.Message{
		SessionID: c.sess.ID,
		Seq:       len(c.msgs),
		Source:    c.sess.Source,
		Project:   c.sess.Project,
		Role:      role,
		Text:      text,
		CreatedAt: at,
	})
}

func readLines(path string, fn func(map[string]any)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var o map[string]any
		if err := json.Unmarshal([]byte(line), &o); err != nil {
			continue
		}
		fn(o)
	}
	return sc.Err()
}

func str(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

func parseTime(v any) time.Time {
	s, _ := v.(string)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

func microsToTime(v any) time.Time {
	if f, ok := v.(float64); ok && f > 0 {
		return time.UnixMicro(int64(f)).UTC()
	}
	return time.Time{}
}

// ---- Codex rollout jsonl ----

func parseCodex(path string) (store.Session, []store.Message, error) {
	sess := newSession("", "codex", "", path, time.Time{})
	c := &collector{sess: sess, seen: map[string]bool{}}
	var cwd, sid string
	var started time.Time
	err := readLines(path, func(o map[string]any) {
		at := parseTime(o["timestamp"])
		switch s, _ := o["type"].(string); s {
		case "session_meta", "turn_context":
			if p := o["payload"].(map[string]any); p != nil {
				if v, _ := p["cwd"].(string); v != "" {
					cwd = v
				}
				if v, _ := p["session_id"].(string); v != "" {
					sid = v
				} else if v, _ := p["id"].(string); v != "" && s == "session_meta" {
					sid = v
				}
			}
			if s == "session_meta" {
				started = at
			}
		case "response_item":
			p, _ := o["payload"].(map[string]any)
			if p == nil || p["type"] != "message" {
				return // custom_tool_call(_output), reasoning: dropped
			}
			role, _ := p["role"].(string)
			if role == "developer" {
				role = "system"
			}
			if role != "user" && role != "assistant" && role != "system" {
				return
			}
			c.add(role, toolBlock.ReplaceAllString(codexText(p), ""), at)
		case "event_msg":
			// Newer format: item_completed envelopes carrying AgentMessage /
			// UserMessage items; Reasoning / CommandExecution / FileChange /
			// McpToolCall / ... are reasoning or tool calls: dropped.
			p, _ := o["payload"].(map[string]any)
			if p == nil || p["type"] != "item_completed" {
				return
			}
			item, _ := p["item"].(map[string]any)
			if item == nil {
				return
			}
			var role string
			switch item["type"] {
			case "AgentMessage":
				role = "assistant"
			case "UserMessage":
				role = "user"
			default:
				return
			}
			var sb strings.Builder
			if arr, _ := item["content"].([]any); arr != nil {
				for _, b := range arr {
					bm, _ := b.(map[string]any)
					if bm == nil {
						continue
					}
					if t, _ := bm["type"].(string); t == "Text" || t == "text" {
						if tx, _ := bm["text"].(string); tx != "" {
							sb.WriteString(tx)
							sb.WriteString("\n")
						}
					}
				}
			}
			c.add(role, toolBlock.ReplaceAllString(sb.String(), ""), at)
		}
	})
	if err != nil {
		return sess, nil, err
	}
	sess = newSession(sid, "codex", cwd, path, started)
	c.sess = sess
	for i := range c.msgs {
		c.msgs[i].SessionID = sess.ID
		c.msgs[i].Project = sess.Project
	}
	return sess, c.msgs, nil
}

// codexText extracts input_text/output_text from a response_item message.
func codexText(p map[string]any) string {
	var sb strings.Builder
	if arr, _ := p["content"].([]any); arr != nil {
		for _, b := range arr {
			bm, _ := b.(map[string]any)
			if bm == nil {
				continue
			}
			if t, _ := bm["type"].(string); t == "input_text" || t == "output_text" {
				if tx, _ := bm["text"].(string); tx != "" {
					sb.WriteString(tx)
					sb.WriteString("\n")
				}
			}
		}
	}
	return sb.String()
}

// ---- Claude Code jsonl ----

func claudeText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, b := range v {
			bm, _ := b.(map[string]any)
			if bm == nil {
				continue
			}
			if t, _ := bm["type"].(string); t == "text" {
				if tx, _ := bm["text"].(string); tx != "" {
					sb.WriteString(tx)
					sb.WriteString("\n")
				}
			}
			// thinking, tool_use, tool_result: dropped (reasoning / tool calls)
		}
		return sb.String()
	}
	return ""
}

func parseClaude(path string) (store.Session, []store.Message, error) {
	sess := newSession("", "claude", "", path, time.Time{})
	c := &collector{sess: sess, seen: map[string]bool{}}
	var sid, cwd string
	var started time.Time
	err := readLines(path, func(o map[string]any) {
		if v, _ := o["sessionId"].(string); v != "" {
			sid = v
		}
		if v, _ := o["cwd"].(string); v != "" {
			cwd = v
		}
		at := parseTime(o["timestamp"])
		if started.IsZero() && !at.IsZero() {
			started = at
		}
		typ, _ := o["type"].(string)
		switch typ {
		case "user", "assistant":
			msg, _ := o["message"].(map[string]any)
			if msg == nil {
				return
			}
			c.add(typ, claudeText(msg["content"]), at)
		case "system", "summary":
			msg, _ := o["message"].(map[string]any)
			var content any
			if msg != nil {
				content = msg["content"]
			} else {
				content = o["content"]
			}
			c.add("system", claudeText(content), at)
		}
	})
	if err != nil {
		return sess, nil, err
	}
	sess = newSession(sid, "claude", cwd, path, started)
	c.sess = sess
	for i := range c.msgs {
		c.msgs[i].SessionID = sess.ID
		c.msgs[i].Project = sess.Project
	}
	return sess, c.msgs, nil
}

// ---- Muse session.jsonl ----

func museRecords(o map[string]any, yield func(map[string]any)) {
	if rf, _ := o["retained_frame"].(string); rf != "" {
		if kids, _ := o["children"].([]any); kids != nil {
			for _, k := range kids {
				km, _ := k.(map[string]any)
				if km == nil {
					continue
				}
				if rj, _ := km["record_json"].(string); rj != "" {
					var inner map[string]any
					if err := json.Unmarshal([]byte(rj), &inner); err == nil {
						yield(inner)
					}
				}
			}
		}
		return
	}
	if _, ok := o["payload"]; ok {
		yield(o)
	}
}

func museUserTexts(payload map[string]any, out *[]string) {
	for _, key := range []string{"model_messages"} {
		if arr, _ := payload[key].([]any); arr != nil {
			for _, m := range arr {
				mm, _ := m.(map[string]any)
				if mm == nil {
					continue
				}
				if carr, _ := mm["content"].([]any); carr != nil {
					for _, b := range carr {
						bm, _ := b.(map[string]any)
						if bm != nil && bm["kind"] == "text" {
							if tx, _ := bm["text"].(string); tx != "" {
								*out = append(*out, tx)
							}
						}
					}
				}
			}
		}
	}
	if arr, _ := payload["refill_blocks"].([]any); arr != nil {
		for _, b := range arr {
			if bm, _ := b.(map[string]any); bm != nil && bm["kind"] == "text" {
				if tx, _ := bm["text"].(string); tx != "" {
					*out = append(*out, tx)
				}
			}
		}
	}
}

func parseMuse(path string) (store.Session, []store.Message, error) {
	sess := newSession("", "muse", "", path, time.Time{})
	c := &collector{sess: sess, seen: map[string]bool{}}
	var sid, cwd string
	var started time.Time
	err := readLines(path, func(o map[string]any) {
		museRecords(o, func(r map[string]any) {
			if v := str(r, "stream", "id"); v != "" {
				sid = v
			}
			at := microsToTime(r["recorded_at"])
			if started.IsZero() && !at.IsZero() {
				started = at
			}
			pt, _ := r["payload_type"].(string)
			pl, _ := r["payload"].(map[string]any)
			if pl == nil {
				return
			}
			switch {
			case pt == "runtime.session.route_facts":
				if v := str(pl, "record", "cwd"); v != "" {
					cwd = v
				}
			case pt == "runtime.session.metadata":
				if v := str(pl, "record", "workspace_root"); v != "" && cwd == "" {
					cwd = v
				}
			case pt == "runtime.user_intent.accepted" || pt == "runtime.user_intent.materialized":
				var texts []string
				museUserTexts(pl, &texts)
				for _, t := range texts {
					c.add("user", t, at)
				}
			case pt == "runtime.session":
				ev, _ := pl["event"].(map[string]any)
				if ev == nil {
					return
				}
				switch ev["kind"] {
				case "started":
					if tx, _ := ev["prompt"].(string); tx != "" {
						c.add("user", tx, at)
					}
				case "assistant_message_committed":
					if tx, _ := ev["text"].(string); tx != "" {
						c.add("assistant", tx, at)
					}
				case "inbox_item_queued":
					if src, _ := ev["source"].(map[string]any); src != nil && src["source"] == "user_steer" {
						if p, _ := ev["payload"].(map[string]any); p != nil {
							if tx, _ := p["prompt"].(string); tx != "" {
								c.add("user", tx, at)
							}
						} else if tx, _ := ev["body"].(string); tx != "" {
							c.add("user", tx, at)
						}
					}
				}
				// assistant_tool_calls_committed, tool_result_batch_committed,
				// output, reasoning_committed: dropped (tool calls / outputs)
			}
		})
	})
	if err != nil {
		return sess, nil, err
	}
	sess = newSession(sid, "muse", cwd, path, started)
	c.sess = sess
	for i := range c.msgs {
		c.msgs[i].SessionID = sess.ID
		c.msgs[i].Project = sess.Project
	}
	return sess, c.msgs, nil
}

// ---- Cursor agent transcripts ----

func parseCursorTranscript(path string) (store.Session, []store.Message, error) {
	sess := newSession("", "cursor", decodeCursorProject(path), path, time.Time{})
	c := &collector{sess: sess, seen: map[string]bool{}}
	err := readLines(path, func(o map[string]any) {
		role, _ := o["role"].(string)
		if role != "user" && role != "assistant" {
			return
		}
		msg, _ := o["message"].(map[string]any)
		if msg == nil {
			return
		}
		var sb strings.Builder
		if arr, _ := msg["content"].([]any); arr != nil {
			for _, b := range arr {
				bm, _ := b.(map[string]any)
				if bm == nil {
					continue
				}
				if t, _ := bm["type"].(string); t == "text" {
					if tx, _ := bm["text"].(string); tx != "" {
						sb.WriteString(tx)
						sb.WriteString("\n")
					}
				}
				// tool_use and friends: dropped
			}
		}
		c.add(role, sb.String(), time.Time{})
	})
	if err != nil {
		return sess, nil, err
	}
	sess.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	c.sess = sess
	for i := range c.msgs {
		c.msgs[i].SessionID = sess.ID
	}
	return sess, c.msgs, nil
}

// decodeCursorProject maps ~/.cursor/projects/<slug>/... back to a path.
// Slugs join path segments with '-': home-jmarceno-Projects-x -> /home/.../x.
// Names that originally contained dashes are ambiguous; best effort only.
func decodeCursorProject(path string) string {
	idx := strings.Index(path, "/projects/")
	if idx < 0 {
		return ""
	}
	rest := path[idx+len("/projects/"):]
	slug := rest
	if i := strings.Index(slug, "/"); i >= 0 {
		slug = slug[:i]
	}
	return "/" + strings.ReplaceAll(slug, "-", "/")
}

// ---- Cursor plan files ----

func parseCursorPlan(path string) (store.Session, []store.Message, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return store.Session{}, nil, err
	}
	id := strings.TrimSuffix(filepath.Base(path), ".plan.md")
	sess := newSession(id, "cursor", "", path, time.Time{})
	msg := store.Message{
		SessionID: id, Seq: 0, Source: "cursor",
		Role: "note", Text: strings.TrimSpace(string(raw)),
	}
	if msg.Text == "" {
		return sess, nil, nil
	}
	return sess, []store.Message{msg}, nil
}
