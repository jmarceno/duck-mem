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
duck-mem daemon [--db PATH] [--interval 5m] [ROOT...]
```

To foreground-test: go build -o ~/.local/bin/duck-mem ./cmd/duck-mem && duck-mem daemon.


## Daemon

`daemon` runs ingest + incremental index on a loop (default every 5
minutes), logging one line per cycle to stdout. Foreground for now; the
service unit comes later. Each cycle opens the database and closes it
again: DuckDB allows one process at a time, so holding the file would
block queries — CLI commands additionally retry through the brief
mid-cycle lock. Stop with SIGINT/SIGTERM; the daemon finishes its line
and exits (`daemon stop after N cycles`). No LLM is involved anywhere
in this path.

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
the 12 most distinctive qualifying terms per message, at most 66 pairs — so
weak edges never materialize instead of being pruned after the fact.

Threshold decisions use counts as of each run, so a term crossing `minDF`
later slightly undercounts its early pairs. `index --full` rebuilds from
scratch with exact thresholds and heals that drift; run it weekly:

```bash
0 4 * * 0 duck-mem index --full   # weekly heal; daily ingest stays incremental
```

Flags may come before or after the query text.

Default db: `~/.local/share/duck-mem/memory.duckdb`. Re-running `ingest` is
idempotent. Appended messages index incrementally; if a session's earlier
messages change or disappear, ingest reconciles its rows and the next index
pass rebuilds the graph to remove stale relationships.

## Roadmap (suggested, not yet implemented)

- **LLM typed extraction.** Rules misfire (`long (owns)` from "X's long …")
  and only know `owns`/`replaces`. A Graphiti-style per-message LLM pass
  would raise precision and add kinds (`fixes`, `blocks`, …). The `kind`
  column and typed-first ranking are ready for it.
- **Directed typed edges.** `replaces` is stored undirected; direction needs
  a column plus arrow display (`turret —replaces→ super`).
- **Top-K neighbors per topic (union rule).** If the graph outgrows comfort,
  keep an edge when *either* endpoint ranks it in its top K: bounds size at
  ~2K×vocabulary while keeping everything reachable.
- **Weight-1 pruning.** ~83% of edges have weight 1. A flag to drop them at
  index or query time would cut the graph ~6x — at the cost of rare links
  like `bastion–turret`, so it stays opt-in.
- **duckpgq path queries.** Multi-hop "how is Bastion connected to Cataclysm"
  over `topic_edges` once the extension story is solid.
- **VSS semantic search.** Keyword match misses synonyms/paraphrases; an
  embedding column on `messages` plus `related`-style fusion is the fix.
- **Cursor slug decoding.** Dashes in project names decode wrong
  (`omen-the-game` → `omen/the/game`); needs a workspace-mapping lookup.

## Tests

Per AGENTS.md methodology (no E2E, no existence assertions, smallest set):

- `internal/ingest/ingest_test.go` — one conversation fixture per source:
  user/assistant text kept, tool calls/outputs/reasoning dropped.
- `internal/store/store_test.go` — keyword AND-match, project filter,
  re-ingest dedupe, related ranking (typed-first, depth-2 via cap),
  incremental index idempotency, full-rebuild drift healing.
- `internal/topics/topics_test.go` — term extraction, glue/stopword drops,
  narrow pair selection, typed owns/replaces rules.
