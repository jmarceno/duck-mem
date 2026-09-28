# Contributing to duck-mem

Thanks for helping out. duck-mem reads other people's session logs, so most
changes touch parsing or ranking — the two places where a quiet regression is
easiest to ship.

## Getting set up

```bash
git clone https://github.com/jmarceno/duck-mem.git
cd duck-mem
go build ./...
go test ./...
```

You do not need to run `duck-mem --install` to work on the code. The test suite
creates its own temporary DuckDB files; the installed services and your real
memory database are only touched by the CLI you run yourself.

## Where things live

| Path | What lives there |
|---|---|
| `cmd/duck-mem` | CLI commands, the systemd units, the D-Bus tray |
| `internal/ingest` | parsers for each agent's session format |
| `internal/store` | schema, hybrid search, the topic graph |
| `internal/topics` | tokenizer, stemming, term extraction, typed rules |
| `internal/embed` | the hashed term and phrase vectors |
| `assets` | the logo, embedded into the binary for the tray icon |

## Tests

The suite is deliberately small and behavioural: no end-to-end tests, and no
test that only checks that a function or constant exists. A test earns its place
by failing for a reason someone would care about.

```bash
go test ./...          # everything
go test ./internal/store -run Recall -v
```

Parser changes need a fixture. Put a trimmed session log under the relevant
`internal/ingest` test and assert what should be kept and what should be
dropped — the whole point of the project is that tool calls never enter the
database, so that assertion is the one to write first.

Ranking changes should come with a way to tell they helped: a real query and
the session it should return first.

## Commit and PR conventions

- Run `gofmt` and `go vet ./...` before committing.
- One concern per commit; the commit subject says what changed and why.
- Note schema changes and parser-version bumps explicitly — existing databases
  are migrated in place, so a silent change to either is a compatibility break.

## Reporting bugs

Open an issue with the agent and version you are on, the command you ran, what
you expected and what happened. If the output points at your own session
directory, the database or the log path, please redact the path before posting
it — the trace is more useful than the name, and the paths are yours.

## Security

Please report vulnerabilities privately rather than in a public issue; see
[SECURITY.md](SECURITY.md).

## License

Contributions are accepted under the [MIT license](LICENSE).
