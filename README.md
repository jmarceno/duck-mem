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
duck-mem index [--db PATH] [--min-df N] [--full] # incremental; --full rebuilds
duck-mem related [--db PATH] [--project P] [--depth 1|2] [--limit N] <term>
```

## Topic relationships

`related` answers "what is discussed together with X" from a co-mention
graph (`topic_edges`): two terms share an edge weighted by how many messages
mention both. Depth 2 follows neighbors-of-neighbors (`term -via-> hit`),
expanding only the 8 strongest direct links. Typed relations outrank
bare co-mentions: rule extraction finds possessive `X's Y` (owns) and
replacement verbs / "instead of" (replaces) per sentence, e.g.
`(bastion, turret, owns)`, `(super, turret, replaces)`. Typed edges are
stored undirected — the pair names the relationship, `query` shows the
source message for direction.

## Indexing: incremental by default, full weekly

`ingest` and `index` only process messages not yet indexed (`messages.indexed`):
pair weights grow by delta and per-project document frequencies accumulate in
`term_df`, so insertion cost is O(new messages). Pairs are created narrowly —
the 8 most distinctive qualifying terms per message, at most 28 pairs — so
weak edges never materialize instead of being pruned after the fact.

Threshold decisions use counts as of each run, so a term crossing `minDF`
later slightly undercounts its early pairs. `index --full` rebuilds from
scratch with exact thresholds and heals that drift; run it weekly:

```bash
0 4 * * 0 duck-mem index --full   # weekly heal; daily ingest stays incremental
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
