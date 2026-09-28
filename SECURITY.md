# Security Policy

## Supported versions

duck-mem has not reached a tagged release yet. Fixes land on `master`; build
from the latest commit or pull request rather than an old tag.

## Reporting a vulnerability

Please open a [private security advisory](https://github.com/jmarceno/duck-mem/security/advisories/new)
instead of a public issue, and expect an acknowledgement within a week.

Worth reporting: anything that reads or writes outside the paths duck-mem is
documented to touch (`~/.codex`, `~/.claude`, `~/.cursor`, the Muse sessions
directory, the OpenCode SQLite store, and duck-mem's own database, config and
state directories), a command that deletes more than its own data, or session
text that leaves the machine.

## What duck-mem does not do

- It never writes to another agent's session store. Reads are read-only,
  including the OpenCode SQLite database and its WAL.
- It makes no network calls of its own. The DuckDB `vss`, `fts` and `duckpgq`
  extensions are installed from the DuckDB community repository on first run,
  which is the one outbound request the tool triggers.
- It calls no LLM or API, so no session text leaves your machine.

Because the database is a plain DuckDB file containing your conversations, it
is as sensitive as the logs it was built from: keep it out of shared locations
and out of backups you do not control. `--uninstall --purge-data` removes
duck-mem's database, config and state; it does not remove the original agent
session logs.
