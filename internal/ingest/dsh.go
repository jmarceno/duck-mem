package ingest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jmarceno/duck-mem/internal/store"
	"github.com/klauspost/compress/zstd"
)

// dshSessionVersion is the only DeepSeek Harness event-log format this parser
// understands. A different version fails as an unexpected format, which the
// sync log reports, rather than guessing at records that may have changed
// meaning.
const dshSessionVersion = 4

// dshSource labels DeepSeek Harness conversations in memory.
const dshSource = "dsh"

// parseDSH reads one DeepSeek Harness session. The harness stores its event
// log as a zstd stream that grows one frame per recorded event, so the file is
// read whole and decompressed on the fly.
//
// Kept: real user prompts and assistant text. The harness also writes its own
// context as user-role records (workspace instructions, runtime context, the
// skill catalog, approval notices); those carry a source kind other than
// "user" and are dropped, as are the harness system prompt, reasoning, tool
// calls and tool results.
func parseDSH(path string) (store.Session, []store.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return store.Session{}, nil, err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return store.Session{}, nil, fmt.Errorf("%w: open DSH session: %v", ErrUnexpectedFormat, err)
	}
	defer zr.Close()

	c := newCollector(newSession("", dshSource, "", path, time.Time{}), nil)
	err = readDSHRecords(zr, c)
	if errors.Is(err, io.ErrUnexpectedEOF) && c.sess.ID != "" {
		// The harness appends one frame per event, so a torn tail only means
		// the session grew while it was read. The records already decoded are
		// kept; the checkpoint stays behind and the next pass reconciles the
		// whole file.
		err = nil
	}
	if err != nil {
		return store.Session{}, nil, err
	}
	if c.sess.ID == "" {
		return store.Session{}, nil, fmt.Errorf("%w: DSH session header missing", ErrUnexpectedFormat)
	}
	for i := range c.msgs {
		c.msgs[i].SessionID = c.sess.ID
		c.msgs[i].Project = c.sess.Project
	}
	return c.sess, c.msgs, nil
}

// readDSHRecords decodes the event log and folds each record into the
// conversation. A final line without a newline is a record still being
// written and is ignored.
func readDSHRecords(r io.Reader, c *collector) error {
	br := bufio.NewReader(r)
	for {
		data, readErr := br.ReadBytes('\n')
		if line := bytes.TrimSpace(data); len(line) > 0 && bytes.HasSuffix(data, []byte("\n")) {
			if err := dshRecord(line, c); err != nil {
				return err
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

// dshRecord folds one event into the conversation. Lifecycle, model, tool and
// title records carry no conversation text and are ignored.
func dshRecord(line []byte, c *collector) error {
	var o map[string]any
	if err := json.Unmarshal(line, &o); err != nil {
		return fmt.Errorf("%w: invalid DSH record", ErrUnexpectedFormat)
	}
	at := millisToTime(o["time"])
	switch typ := str(o, "type"); typ {
	case "session":
		if v, _ := o["version"].(float64); int(v) != dshSessionVersion {
			return fmt.Errorf("%w: DSH session version %v", ErrUnexpectedFormat, o["version"])
		}
		id := str(o, "id")
		if id == "" {
			return fmt.Errorf("%w: DSH session header has no id", ErrUnexpectedFormat)
		}
		c.sess = newSession(dshSource+":"+id, dshSource, str(o, "cwd"), c.sess.Path, millisToTime(o["createdAt"]))
	case "user/message":
		d := dshMap(o["data"])
		if str(d, "source", "kind") != "user" {
			return nil
		}
		c.add("user", dshText(d["content"]), at)
	case "assistant/message":
		c.add("assistant", dshText(dshMap(dshMap(o["data"])["message"])["content"]), at)
	}
	return nil
}

// dshText joins the visible text parts of a message. Reasoning and tool-call
// parts carry no conversation text and are dropped.
func dshText(parts any) string {
	arr, _ := parts.([]any)
	var sb strings.Builder
	for _, p := range arr {
		pm, _ := p.(map[string]any)
		if pm == nil {
			continue
		}
		if t, _ := pm["type"].(string); t != "text" {
			continue
		}
		if tx, _ := pm["text"].(string); tx != "" {
			sb.WriteString(tx)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func dshMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// millisToTime reads an epoch-milliseconds field, the unit DSH uses.
func millisToTime(v any) time.Time {
	f, ok := v.(float64)
	if !ok || f <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(f)).UTC()
}
