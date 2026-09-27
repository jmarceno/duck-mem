# duck-mem

Agent memory: session logs from Codex, Claude, Cursor and Muse land in one
DuckDB file, minus tool calls. Agents search it through the CLI (or by
querying the DuckDB file directly).

## Schema decision: project is a value, not a column

AGENTS.md asked whether each project should be a column. No: DuckDB columns
are schema, so per-project columns would need DDL for every new project and
break every query. Instead `project` is a plain `TEXT` value on both tables,
with an index, filtered via `WHERE project ILIKE '%<name>%'`.

```sql
sessions(session_id TEXT PRIMARY KEY, source, project, started_at, path)
messages(session_id, seq, source, project, role, text, created_at,
         PRIMARY KEY(session_id, seq))
```

`duckpgq` and `vss` extensions load best-effort at open; iteration 1 search
is `ILIKE` keyword match, so ingest/search work offline. Graph + vector
search are the natural iteration 3 on top of this schema.

## Kept vs dropped per source

Kept: user prompts, assistant text, system/developer text.
Dropped everywhere: tool calls, tool outputs, reasoning/thinking blocks,
environment snapshots, queue/metadata records.

| source | files | project from |
|---|---|---|
| Codex | `~/.codex/sessions/**/*.jsonl`, `~/.codex/archived_sessions/*.jsonl` | `session_meta`/`turn_context` cwd |
| Claude | `~/.claude/projects/*/*.jsonl` | record `cwd` |
| Muse | `…/muse/sessions/**/**/session.jsonl` (incl. `subagent/`) | `route_facts` cwd / `workspace_root` |
| Cursor | `~/.cursor/projects/*/agent-transcripts/*/*.jsonl`, `~/.cursor/plans/*.plan.md` (whole plan = one `note`) | transcript path slug / — |

Cursor's pre-migration composer blobs in `workspaceStorage/state.vscdb` are
not parsed (only migrated IDs remain there); transcripts + plans are covered.

## Usage

```bash
duck-mem ingest [--db PATH] [ROOT...]   # default roots cover all four stores above
duck-mem query [--db PATH] [--project P] [--source S] [--limit N] <text...>
```

Flags may come before or after the query text.

Default db: `~/.local/share/duck-mem/memory.duckdb`. Re-running `ingest` is
idempotent (`ON CONFLICT DO NOTHING` on `(session_id, seq)`).

## Tests

Per AGENTS.md methodology (no E2E, no existence assertions, smallest set):

- `internal/ingest/ingest_test.go` — one conversation fixture per source:
  user/assistant text kept, tool calls/outputs/reasoning dropped.
- `internal/store/store_test.go` — keyword AND-match, project filter,
  re-ingest dedupe.
