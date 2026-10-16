package owner

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/jmarceno/duck-mem/internal/store"
)

const (
	opQuery   = "query"
	opResolve = "resolve"
	opWindow  = "window"
	opRelated = "related"
	opIngest  = "ingest"
	opIndex   = "index"
	opRename  = "rename"
	opRepack  = "repack"
)

type request struct {
	Op          string   `json:"op"`
	Query       string   `json:"query,omitempty"`
	Project     string   `json:"project,omitempty"`
	Source      string   `json:"source,omitempty"`
	Limit       int      `json:"limit,omitempty"`
	Hits        int      `json:"hits,omitempty"`
	Ref         string   `json:"ref,omitempty"`
	From        int      `json:"from,omitempty"`
	To          int      `json:"to,omitempty"`
	Keep        int      `json:"keep,omitempty"`
	WithContext bool     `json:"with_context,omitempty"`
	Term        string   `json:"term,omitempty"`
	Depth       int      `json:"depth,omitempty"`
	Roots       []string `json:"roots,omitempty"`
	MinDF       int      `json:"min_df,omitempty"`
	Full        bool     `json:"full,omitempty"`
	OldProject  string   `json:"old_project,omitempty"`
	NewProject  string   `json:"new_project,omitempty"`
	Out         string   `json:"out,omitempty"`
}

type response struct {
	Error   string          `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type queryPayload struct {
	Results []store.SessionResult `json:"results"`
	Topics  []store.Topic         `json:"topics"`
}

// Client talks to the daemon that owns a database.
type Client struct {
	conn net.Conn
}

// Dial connects to the owner socket for dbPath.
func Dial(dbPath string) (*Client, error) {
	conn, err := net.DialTimeout("unix", SocketPath(dbPath), time.Second)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) roundTrip(req request, dest any) error {
	if err := writeRequest(c.conn, req); err != nil {
		return err
	}
	resp, err := readResponse(c.conn)
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	if dest == nil {
		return nil
	}
	if len(resp.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(resp.Payload, dest)
}

// Recall runs a query on the owner.
func (c *Client) Recall(query, project, source string, limit, hits int) ([]store.SessionResult, []store.Topic, error) {
	var out queryPayload
	err := c.roundTrip(request{Op: opQuery, Query: query, Project: project, Source: source, Limit: limit, Hits: hits}, &out)
	return out.Results, out.Topics, err
}

// ResolveSession finds a session by id or unique prefix.
func (c *Client) ResolveSession(ref string) (store.SessionResult, error) {
	var out store.SessionResult
	err := c.roundTrip(request{Op: opResolve, Ref: ref}, &out)
	return out, err
}

// Window reads messages in a sequence range.
func (c *Client) Window(sessionID string, from, to, keep int, withContext bool) ([]store.Message, error) {
	var out []store.Message
	err := c.roundTrip(request{Op: opWindow, Ref: sessionID, From: from, To: to, Keep: keep, WithContext: withContext}, &out)
	return out, err
}

// Related walks the topic graph.
func (c *Client) Related(term, project string, depth, limit int) ([]store.RelatedHit, error) {
	var out []store.RelatedHit
	err := c.roundTrip(request{Op: opRelated, Term: term, Project: project, Depth: depth, Limit: limit}, &out)
	return out, err
}

// Ingest asks the owner to sync the given roots.
func (c *Client) Ingest(roots []string) (IngestResult, error) {
	var out IngestResult
	err := c.roundTrip(request{Op: opIngest, Roots: roots}, &out)
	return out, err
}

// Index asks the owner to rebuild or extend the topic graph.
func (c *Client) Index(minDF int, full bool) (IndexResult, error) {
	var out IndexResult
	err := c.roundTrip(request{Op: opIndex, MinDF: minDF, Full: full}, &out)
	return out, err
}

// RenameProject asks the owner to rename a project. It runs as a write job,
// so it is serialized with ingest cycles.
func (c *Client) RenameProject(old, new string) (int, error) {
	var out int
	err := c.roundTrip(request{Op: opRename, OldProject: old, NewProject: new}, &out)
	return out, err
}

// Repack asks the owner to rebuild the database file. The owner closes its
// handle for the duration, so the command works while the indexer is up.
// Out writes a verified copy there instead of replacing the live file.
func (c *Client) Repack(out string) (store.RepackResult, error) {
	var res store.RepackResult
	err := c.roundTrip(request{Op: opRepack, Out: out}, &res)
	return res, err
}
