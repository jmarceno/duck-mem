---
name: duck-mem-query
description: Recall long-term memory from past agent sessions via the local duck-mem database.
---

# Query duck-mem Memory

`duck-mem` is this machine's long-term agent memory: past Codex, Claude,
Cursor, and Muse sessions, minus tool calls, in a DuckDB file refreshed by
a background daemon every few minutes. Use it to recover prior decisions,
gotchas, and context instead of re-deriving them.

## Contract

- **Read-only.** Use `query` and `related` only. Never run `ingest`,
  `index`, or `daemon`, and never open the database file for writing.
  A daemon owns all writes.
- Default database: `~/.local/share/duck-mem/memory.duckdb`. Pass `--db`
  only if the user points you at another file.

## query: find past utterances

```bash
duck-mem query [--project P] [--source S] [--limit N] <words...>
```

- Words AND-match against message text (case-insensitive substring).
- `--project` filters by project path substring (e.g. `omen-the-game`).
- `--source` is one of `codex`, `claude`, `cursor`, `muse`.
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

- Lists topics discussed together with the term, most relevant first.
- Lines shaped `sentry (replaces, 3)` are typed relations (`owns`,
  `replaces`) and outrank bare co-mentions like `turret (3)`.
- Typed edges are undirected: the pair names the relationship; run a
  `query` on both words to recover direction from the source message.
- `--depth 2` adds neighbors-of-neighbors as `topic (weight, via hub)`.

Example: `related --project omen-the-game bastion` surfaces the turret
cluster; then `query --project omen-the-game bastion turret` pulls the
decision text.

## When to use it

- Session start on an existing project: one `query --project <name>` for
  the current task area plus one `related` hop to find linked topics.
- Before re-deciding something: search first; prior sessions often decided
  it already (design calls, failed approaches, error fixes).
- Mid-task when stuck: recall how a past session solved the same error.

## Limits

- Memory lags the daemon by minutes; it never contains the current
  conversation.
- The CLI retries through daemon write cycles automatically; if a command
  ever reports a lock conflict, wait a few seconds and retry once.
- Keyword match only: synonyms and paraphrases can miss. If a query comes
  back empty, retry with different words from the same topic.
- Snippets are memory, not truth: files may have changed since. Confirm
  before acting.
