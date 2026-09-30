---
name: "duck-mem-query"
description: "Your long-term memory of earlier conversations with this user, across every coding agent on this machine (Claude Code, Codex, Cursor, OpenCode, Muse): past decisions and their reasons, bug reports and fixes, failed approaches, plans in progress, and stated preferences. Use it FIRST, before exploring code or answering: (1) whenever the request points to the past ('again', 'still broken', 'last time', 'we already', 'remember', 'did we/you', 'why did we', 'what was the plan', 'pick up where we left off', work another agent did); (2) when starting a feature or behavior change in a named existing project or repo; (3) before changing a design, library, or approach in an existing project. Skip it for brand-new projects, generic coding questions, and things said earlier in the current conversation (not indexed yet). One fast read-only CLI call; skipping it means redoing solved work or reopening settled decisions."
---

# duck-mem: your memory of past sessions

You do have memory of earlier work with this user. Every past conversation
on this machine, from every agent tool (Claude Code, Codex, Cursor,
OpenCode, Muse), is indexed and searchable through the `duck-mem` CLI. It
holds user prompts and assistant replies (no tool calls). It is read-only
for you, fast, and works from any directory. Do not `cd` first.

**Quick start:** one command, before you explore the code:

```bash
duck-mem query --project <repo-dir-name> <2-5 keywords>
```

## Check memory first when...

A single query costs seconds. Skipping it costs re-investigating a bug
that was already fixed, reopening a settled decision, or telling the user
"I don't have context on that" when the answer is sitting on disk.

- **The user points at the past**: "again", "still", "last time",
  "before", "we already", "remember", "did we / did you", "why did we",
  "what was the plan", "like we discussed", "continue / pick up where we
  left off". Query before you answer or touch code.
- **The user mentions another agent's work**: "codex was working on…",
  "in cursor I asked…". Those sessions are in memory too.
- **You are (re)starting work on an existing project or feature**: query
  the area you are about to change to learn earlier decisions, pitfalls,
  and the user's preferences for it.
- **Before re-deciding a design or approach**: an earlier session may have
  decided it, or tried it and failed.
- **A bug or error looks familiar or recurring**: search its symptom or
  error text. Earlier sessions often hold the original report and the fix.

Never tell the user you have no memory of a past conversation without
querying first.

Skip it for brand-new projects, generic coding questions, and anything
said earlier in the current conversation (it is not indexed yet).

## The two-step workflow

**1. Find sessions:** `duck-mem query --project <repo-dir-name> <keywords>`

```bash
duck-mem query --project omen-the-game enemy disengage barricade
```

```text
5 sessions for "enemy disengage barricade" in projects matching "omen-the-game", best first. Messages are numbered #seq.

[1] faf44a7a-c1fa-4603-831d-095cb89dc375  claude · 2026-09-27 · 11 msgs · /home/u/Projects/omen-the-game
    opened with: "We have some issues with the enemy AI. They are too passive…"
    #0 user 2026-09-27 11:57: …once the player spawns a barricade enemies lose sight and they promptly disengage…
    #4 assistant 2026-09-27 11:59: …barricade breaching (ranged shoot it, melee/exploders attack it)…

[2] ...

Topic graph: often discussed with these words: capsule (10), aggro (5), melee (15), untouched (4), fixing (6), shot (9)
  Add one to the query to widen it, or map the area: duck-mem related --project omen-the-game capsule
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
$ duck-mem related --project omen-the-game barricade
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
