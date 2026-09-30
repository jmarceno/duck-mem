<p align="center">
  <img src="assets/logo.png" alt="duck-mem" width="200">
</p>

<h1 align="center">duck-mem</h1>

<p align="center">
  <b>Give your coding agents a memory they can actually search.</b><br>
  Every Codex, Claude, Cursor, Muse and OpenCode session lands in one DuckDB
  file — conversations only, no tool noise — and becomes a search index your
  agent can query from the terminal.
</p>

<p align="center">
  <i>Open a pull request for your favorite agent if it is not here.</i><br>
</p>

<p align="center">
  <img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-yellow.svg">
  <img alt="Go 1.24" src="https://img.shields.io/badge/go-1.24-00ADD8?logo=go&logoColor=white">
  <img alt="Platform: Linux" src="https://img.shields.io/badge/platform-Linux-FBCA04?logo=linux&logoColor=white">
  <img alt="Database: DuckDB" src="https://img.shields.io/badge/database-DuckDB-black?logo=duckdb&logoColor=white">
</p>

<p align="center">
  <a href="#what-it-does">What it does</a> ·
  <a href="#install">Install</a> ·
  <a href="#usage">Usage</a> ·
  <a href="#how-search-works">How search works</a> ·
  <a href="#tray">Tray</a> ·
  <a href="#agent-skill">Agent skill</a> ·
  <a href="#contributing">Contributing</a>
</p>

---

## What it does

Your agents forget everything between sessions, and the raw logs they forget
with are full of tool calls, environment dumps and half-finished reasoning.
duck-mem turns those logs into something searchable:

- **One store, five agents.** Codex, Claude, Cursor, Muse and OpenCode sessions
  are parsed into a single DuckDB file. Nothing is copied out of the original
  agent directories, and the stores are read-only.
- **Conversations, not transcripts.** Tool calls, tool output, reasoning blocks
  and metadata are dropped. You keep the prompts, the answers and the text that
  actually explained the work.
- **Search that returns the conversation.** `query` prints whole sessions —
  the prompt that opened them, the matching messages, the project — not a wall
  of message IDs for the agent to go and read.
- **A topic graph, not just keywords.** A `duckpgq` property graph over the
  vocabulary finds what a project talks about *together*, so a query phrased
  differently still reaches the right session.
- **It keeps itself current.** A systemd user service re-indexes every few
  minutes and skips unchanged files; a tray icon shows status and runs a full
  re-index with one click.
- **No LLM anywhere.** Ingestion, indexing and search are deterministic. No
  message text ever leaves your machine, and no API key is needed.

## Install

Requires Linux with Go 1.24+ and `systemd --user` (any GNOME, KDE, COSMIC or
Hyprland session). Extensions are installed on first run.

```bash
git clone https://github.com/jmarceno/duck-mem.git
cd duck-mem
go build -o duck-mem ./cmd/duck-mem
./duck-mem --install
```

`--install` copies the binary to `~/.local/bin`, adds that directory to your
`PATH` if needed, and enables and starts `duck-mem.service` (the indexer) and
`duck-mem-tray.service` (the tray icon). Open a new login shell to use
`duck-mem` by name.

To remove it again:

```bash
duck-mem --uninstall                 # asks whether to keep the database
duck-mem --uninstall --keep-data     # noninteractive, keeps the database
duck-mem --uninstall --purge-data    # also deletes duck-mem data and config
```

Uninstalling never touches your Codex, Claude, Cursor, Muse or OpenCode
session stores.

## Usage

```
duck-mem ingest [--db PATH] [ROOT...]   index new messages from session logs
duck-mem query [--db PATH] [--project P] [--source S] [--limit N] [--hits N] <text...>
                                        best-matching sessions, with their hits
duck-mem show [--db PATH] [--context N] [--from A] [--to B] [--full] <session>[#seq]
                                        read the messages around a hit
duck-mem related [--db PATH] [--project P] [--depth 1|2] [--limit N] <term>
                                        what a project discusses together with <term>
duck-mem index [--db PATH] [--min-df N] [--full]
                                        incremental (or full) topic-graph index
duck-mem daemon [--db PATH] [--interval 5m] [ROOT...]
                                        the ingest loop the service runs
duck-mem repack [--db PATH] --out PATH  verified ZSTD copy; source is read-only
```

Flags may come before or after the text. The database defaults to
`~/.local/share/duck-mem/memory.duckdb`; because it is a plain DuckDB file you
can also open it with the DuckDB CLI or any DuckDB client.

### Search

```console
$ duck-mem query "flaky test on darwin arm64" --project duck-mem --limit 2
2 sessions for "flaky test on darwin arm64" in projects matching "duck-mem", best first. Messages are numbered #seq.

[1] 0199f1c4-2b7a-7c31-9f0e-5d2a8b41c7de  claude · 2026-08-14 → 2026-08-14 · 412 msgs · /home/you/Projects/duck-mem
    opened with: "the arm64 build times out on the test runner, can you look at it?"
    #12 user 14:02:11: the arm64 runner times out on ingest_test.go:184, flaky?
    #41 assistant 14:19:03: TestRetryExponentialBackoff failed 3 of 5 runs on darwin-arm64
    #58 assistant 14:41:57: the timeout is the -race flag, not the backoff; fixed in helper_test.go:22

[2] 0199f2d8-77aa-70c2-9d13-6b2f0aa9e410  codex · 2026-08-15 · 96 msgs · /home/you/Projects/duck-mem
    (no query words here; found through the topic-graph neighbours below)
    opened with: "re-run the CI matrix and report what broke"
    #7 assistant 09:11:20: darwin-arm64 job failed again: flaky, retrying

Topic graph: often discussed with these words: race (4), timeout (3), retry (2), matrix (1)
  Add one to the query to widen it, or map the area: duck-mem related --project duck-mem runner
Read a hit with the messages around it: duck-mem show 0199f1c4-2b7a-7c31-9f0e-5d2a8b41c7de#41
```

Each result is a session: what it was, when, which project, how it started and
the messages that matched, placed where the query terms are closest together.
Sessions that match only through the topic graph are marked as such and appear
after the direct hits, so the graph widens a search without reordering it.

`show` is the second step — it prints the messages around a hit so you can read
the reasoning in context:

```console
$ duck-mem show 0199f1c4#41 --context 2
session 0199f1c4-2b7a-7c31-9f0e-5d2a8b41c7de · claude · /home/you/Projects/duck-mem
2026-08-14 · 412 messages (#0–#411) · log /home/you/.claude/projects/<project>/<session>.jsonl
showing #39–#43; earlier: --from 0 --to 38; later: --from 44 --to 64; whole messages: --full
```

Unique ID prefixes are enough, and `--project`/`--source` narrow either command.

### Topic relationships

```console
$ duck-mem related runner --depth 2
matrix (14, via timeout)
race (11, via flaky)
flaky (9)
backoff (replaces: retry -> delay, 3)
```

`related` answers "what is discussed together with X" by traversing the topic
graph. Nodes are Porter stems, so `enemy` and `enemies` are the same node and
your query words find their node directly. Co-mention edges are undirected and
weighted by how many messages mention both terms; typed relations recovered
from possessive and replacement phrasing (`bastion --owns--> turret`) keep
their direction and quote the sentence they came from. `--depth 2` follows
neighbours-of-neighbours, expanding only the strongest direct links.

## How search works

Two rankings are fused with reciprocal rank fusion, and the topic graph takes
part in every query.

- **BM25** over a posting list of Porter-stemmed terms, which gives recall:
  word forms match, and a long message that mentions the terms still ranks.
  Postings are written in the same transaction as their messages.
- **Vector similarity** over hashed term and phrase vectors (384 dimensions,
  exact cosine scan in DuckDB). It rewards messages that reuse the query's adjacent
  word pairs, and it only reorders the BM25 candidates, so a hash collision
  between two unrelated short messages cannot promote one of them.
- **The topic graph** contributes one hop of co-mention neighbours to the
  query, at a fraction of the weight of a direct term. If your words match
  fewer sessions than you asked for, those neighbours are searched again to
  fill the empty slots, and the results say so.

Ingestion and indexing are incremental by default: per-file checkpoints skip
logs that have not changed, and appended messages are read from the byte offset
where the previous version left off. Rewritten or truncated sessions are
reconciled in place instead of rebuilt. Because incremental threshold decisions
use counts as of each run, a periodic full rebuild heals the small drift:

```bash
0 4 * * 0 duck-mem index --full   # weekly rebuild; daily ingest stays incremental
```

DuckDB allows a single writer process at a time, so the daemon opens the
database per cycle and closes it again, and CLI commands retry through the
brief lock a cycle can hold. Nothing is left locked between runs.

Vector search deliberately uses no approximate index. The vss HNSW index that
earlier versions persisted kept every checkpoint's index blocks, so the file
grew by roughly 100 MB per changed cycle with almost no new data. An exact scan
of ~60k embeddings costs ~40 ms warm and finds every true neighbor (the HNSW
index returned about 60–75% of them). Opening an older database drops that
index; its blocks become reusable but the file only shrinks after a repack.

`duck-mem repack --db SOURCE --out NEW_FILE` writes a separate compressed copy,
verifies every table's row count and complete-row hash, preserves sequence
positions, and rebuilds indexes. The destination must not exist; the command
never replaces the source or stops services. Stop the services, repack, then
swap the new file in place of the old one.

### What gets indexed

| Source | Read from | Project from |
|---|---|---|
| Codex | `~/.codex/sessions/**/*.jsonl`, `~/.codex/archived_sessions/*.jsonl` | session metadata `cwd` |
| Claude | `~/.claude/projects/*/*.jsonl` | the record's `cwd` |
| Cursor | `~/.cursor/projects/*/agent-transcripts/*/*.jsonl`, `~/.cursor/plans/*.plan.md` | workspace storage `workspace.json` |
| Muse | `~/.local/share/muse/sessions/**/session.jsonl` | route facts / workspace root |
| OpenCode | `~/.local/share/opencode/opencode.db` (read-only, including WAL) | `session_v2.directory`, else the worktree |

Kept: user prompts, assistant text, system and developer text. Dropped
everywhere: tool calls, tool outputs, reasoning and thinking blocks,
environment snapshots, queue and metadata records. A Cursor plan file is
imported whole as a single note, and OpenCode child sessions are imported
alongside their parents. Cursor transcripts whose project cannot be resolved
stay under `cursor:<slug>` rather than being given an invented path.

### Schema

```sql
sessions(session_id TEXT PRIMARY KEY, source, project, started_at, path)
messages(session_id, seq, source, project, role, text, created_at,
         indexed, embedding FLOAT[384], PRIMARY KEY(session_id, seq))
msg_terms(session_id, seq, term, tf)     -- BM25 postings, Porter-stemmed
term_df(project, term, msgs)             -- per-project document frequencies
topics(topic_id, project, term, label)   -- graph nodes
topic_edges(src_id, dst_id, term_a, term_b, weight, kind,
            from_term, to_term, evidence)
msg_edges(session_id, seq, term_a, term_b, ...) -- per-message edge log
file_checkpoints(path, size, mtime_ns, tail_hash, ends_line, session_id, …)
```

`project` is a value, not a column: DuckDB columns are schema, so one column
per project would need DDL for every new project and break every query. It is
an indexed `TEXT` on both tables, filtered with `WHERE project ILIKE '%name%'`.

## Tray

`duck-mem-tray.service` registers a StatusNotifierItem (and DBusMenu) item using
the project logo. The menu shows the indexer's state and the last completed
sync, and can start or stop the indexer or force a full re-index. A dot appears
over the icon while an action is running or when the indexer is down.

Sync history is retained as monthly JSONL files in
`~/.local/state/duck-mem/logs/`. Each cycle records the harness, time, session
and extracted message counts (including zero when unchanged), and success or
failure. Failures include `Unexpected format`, `Database error`, or `Other`
and available error details; cycle-wide errors use harness `all`. No session
text is stored in these logs. Logs are retained without automatic deletion.

**View logs** opens this directory in the default file manager and clears the
indicator for existing failures. Failures include the affected file path and
show a red dot drawn directly into the tray icon, retained across successful
syncs and tray restarts; a later failure sets it again. Busy and stopped states
use amber and grey dots respectively. The tray refreshes every ten seconds.

The tray is optional — `duck-mem ingest`, `query` and the rest work without it.

## Agent skill

A read-only agent skill is included at
[`.agents/skills/duck-mem-query`](.agents/skills/duck-mem-query/SKILL.md). Point
an agent at it and it will search your history before guessing, so it can answer
"we did this last month, here's the session" instead of reinventing it.

## How it differs

- **From `grep` over the logs:** search returns the session that matters, with
  the surrounding conversation, instead of matching lines you have to reassemble.
- **From vector-only tools:** BM25 keeps recall for names, error text and word
  forms; the graph supplies the vocabulary a plain embedding misses.
- **From hosted memory services:** everything stays in one local file. No
  account, no API key, no text leaving the machine.

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for
the build, test and commit conventions, and please include a fixture with any
parser change — the tests are the fastest way to agree on what a session log
should produce.

## License

[MIT](LICENSE) © 2026 Jardel G Marceno
