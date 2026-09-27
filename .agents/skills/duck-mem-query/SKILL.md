---
name: duck-mem-query
description: Recall long-term memory from past agent sessions via the local duck-mem database.
---

# Query duck-mem Memory

`duck-mem` is this machine's long-term agent memory: past Codex, Claude,
Cursor, Muse, and OpenCode sessions, minus tool calls, in a DuckDB file
refreshed by a user daemon when it is running. Use it to recover prior decisions,
gotchas, and context instead of re-deriving them.

## Contract

- **Read-only.** Use `query` and `related` only. Never run `ingest`,
  `index`, or `daemon`, and never open the database file for writing.
  A daemon owns all writes.
- Default database: `~/.local/share/duck-mem/memory.duckdb`. Pass `--db`
  only if the user points you at another file.

## query: find similar utterances

```bash
duck-mem query [--project P] [--source S] [--limit N] <words...>
```

- Ranks messages by cosine similarity (DuckDB `vss` HNSW). The words are
  embedded as terms and adjacent phrases; closest messages come first.
- This is not a keyword AND filter. Unrelated messages stay out. Wording
  that shares no terms with the query can still miss.
- `--project` filters by project path substring (e.g. `omen-the-game`).
- `--source` is one of `codex`, `claude`, `cursor`, `muse`, `opencode`.
- Flags may come before or after the words.

Example:

```bash
duck-mem query --project omen-the-game "Null Sentry" --limit 5
```

Output lines look like:

```text
[muse] /home/u/Projects/omen-the-game 01a0c363…#3 (assistant): Done. Bastion's Sanctuary dome is now the **Null Sentry** …
```

`[source] project session#seq (role): snippet`. The snippet is truncated;
quote it as memory, not as current file state — verify against the checkout
before acting on it.

## related: follow topic links

```bash
duck-mem related [--project P] [--depth 1|2] [--limit N] <term>
```

- Traverses the topic graph (`duckpgq`) for topics discussed together
  with the term, most relevant first.
- Typed relations (`owns`, `replaces`) show direction and evidence, e.g.
  `sentry (replaces: sentry -> dome, 3) [source...]`. They outrank bare
  co-mentions like `turret (3)`.
- Rule-extracted relations can be wrong; run `query` on both words and
  check the source message before treating one as a fact.
- `--depth 2` adds neighbors-of-neighbors as `topic (weight, via hub)`.

Example: `related --project omen-the-game bastion` surfaces the turret
cluster; then `query --project omen-the-game bastion turret` pulls the
decision text.

## When to use it

- Session start on an existing project: query with the project name and
  distinctive terms from the current task, then use one `related` hop to
  find linked topics. `query` requires search text.
- Before re-deciding something: search first; prior sessions often decided
  it already (design calls, failed approaches, error fixes).
- Mid-task when stuck: recall how a past session solved the same error.

## Limits

- Memory lags the daemon by minutes while it runs; it never contains the
  current conversation. The tray menu shows the last completed sync.
- The CLI retries brief daemon write cycles automatically. Initial imports
  and full re-indexes can hold the database longer; on a lock conflict,
  check the tray's last-sync status and retry after indexing completes.
- Similarity uses term and phrase vectors, not a neural model. If a query
  comes back empty, retry with different words from the same topic.
- Snippets are memory, not truth: files may have changed since. Confirm
  before acting.
