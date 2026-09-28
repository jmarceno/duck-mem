---
name: duck-mem-query
description: Search this machine's past agent sessions (Codex, Claude, Cursor, Muse, OpenCode) for earlier decisions, bug reports, fixes, and user requests. Use when starting work on an existing project, before re-deciding something, or when a problem looks like one solved before.
---

# duck-mem: recall past agent sessions

`duck-mem` is a CLI on `PATH`. It searches every past conversation on this
machine (user prompts and assistant replies; no tool calls). It is read-only
for you and works from any directory. Do not `cd` first.

## The two-step workflow

**1. Find sessions:** `duck-mem query --project <repo-dir-name> <keywords>`

```bash
duck-mem query --project spelunking-caves enemy disengage barricade
```

```text
5 sessions for "enemy disengage barricade" in projects matching "spelunking-caves", best first. Messages are numbered #seq.

[1] faf44a7a-c1fa-4603-831d-095cb89dc375  claude · 2026-09-27 · 11 msgs · /home/u/Projects/spelunking-caves
    opened with: "We have some issues with the enemy AI. They are too passive…"
    #0 user 2026-09-27 11:57: …once the player spawns a barricade enemies lose sight and they promptly disengage…
    #4 assistant 2026-09-27 11:59: …barricade breaching (ranged shoot it, melee/exploders attack it)…

[2] ...

Topic graph: often discussed with these words: capsule (10), aggro (5), melee (15), untouched (4), fixing (6), shot (9)
  Add one to the query to widen it, or map the area: duck-mem related --project spelunking-caves capsule
Read a hit with the messages around it: duck-mem show faf44a7a-c1fa-4603-831d-095cb89dc375#0
```

Each block is one past conversation: its ID, tool, dates, size, project,
the prompt that started it, and the matching messages (`#seq`, role, time,
snippet). Often this is enough to answer the question.

The **Topic graph** line lists words that past sessions often used together
with your query words. It is the quickest way to find the project's own
vocabulary for a problem. If your words match fewer sessions than
`--limit`, those neighbours fill the empty slots. Such sessions are marked
`(no query words here; found through the topic-graph neighbours below)`.
Treat them as leads, not matches.

**2. Read the conversation:** `duck-mem show <session-id>#<seq>`

```bash
duck-mem show faf44a7a-c1fa-4603-831d-095cb89dc375#4
```

This prints the hit plus 3 messages on each side, full text (long messages
truncated), and the exact flags to page earlier or later. Look at the
messages after a user request to see what was done and how it ended.

- `--context N`: N messages on each side (default 3)
- `--from A --to B`: an explicit `#seq` range
- `--full`: do not truncate long messages
- An ID prefix works if it is unique: `duck-mem show faf44a7a#4`

## Writing queries

- **Use 2-5 distinctive keywords, not a sentence.** Every word counts in
  the ranking, so filler ("then", "again", "walk away") dilutes it.
  Good: `enemy disengage barricade`. Bad: `melee enemy attack then disengage walk away then attack again`.
- Prefer names, identifiers, file names, and error text: `EnemyDef`,
  `enemyanim.ts`, `HNSW index missing`.
- Word forms match: `enemy` also finds `enemies`, `disengage` finds `disengaged`.
- `--project` is a substring of the project path. Use the repo directory
  name. Leave it off to search every project. If a filtered search finds
  nothing, the output says which other projects match.
- `--limit N` changes the number of sessions (default 5). `--hits N` changes the number of messages per session (default 3).
- `--source codex|claude|cursor|muse|opencode` limits results to one tool.

## When to use it

- Starting on an existing project: query the feature or area you are
  about to change.
- Before re-deciding a design or approach: an earlier session may have
  decided it or tried it and failed.
- On a bug or error: search its symptom or message text. Earlier sessions
  often have the user's original report and the fix.

## Rules

- Read-only: use only `query`, `show`, and `related`. Never run `ingest`,
  `index`, or `daemon`, and never open the database file.
- Results are memory, not current truth. The code may have changed since.
  Check the checkout before you act on a recalled detail.
- The current conversation is not in memory yet. The daemon syncs every few minutes.
- If you see "Could not set lock", the daemon is in the middle of a long
  re-index. Retry after a short wait.

## Mapping an area: related

`duck-mem related --project P <word>` walks the topic graph from one word
and lists what past sessions discussed with it, most connected first:

```text
$ duck-mem related --project spelunking-caves barricade
capsule (8)
dodge (5)
melee (5)
barrier (4)
```

The number is how many messages mention both words. `--depth 2` adds
neighbours of neighbours as `word (weight, via hub)`. Relations seen at
least twice also appear, with direction and a source message:
`sentry (replaces: sentry -> dome, 3) [session#seq: sentence]`. They are
extracted by rules and can be wrong, so confirm one with `query` before
you rely on it.
