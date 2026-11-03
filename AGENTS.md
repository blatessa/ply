# Working on Ply

Ply is a source control tool built around tasks instead of branches. The full product spec is in
[docs/spec.md](docs/spec.md). Read it first. Its concepts, behavior and principles are requirements;
schemas, numbers and command spellings are starting points.

## Tracking work

All work is tracked in GitHub issues on `blatessa/ply`.

- Issues are grouped by milestone (M1 Storage, M2 Local workspace, M3 Server & sync, ...).
- Before starting an issue, assign yourself or comment that you're on it, so two agents don't collide.
- Reference the issue in commits (`Refs #12`, `Closes #12`).
- If you find new work, open an issue for it instead of expanding scope silently.

## Layout

```
cmd/ply/          main package: the single `ply` binary (client + server)
internal/store/   SQLite storage: chunks, blobs, snapshots, transactions
internal/chunk/   content-defined chunking (FastCDC)
internal/...      see package doc comments
docs/             spec and design notes
```

## Decisions so far

- Go, single binary. Pure-Go SQLite (`modernc.org/sqlite`) so the binary builds for macOS and Linux without cgo.
- One SQLite file per project, locally and on the server. Locally it also holds workspaces, saves and the undo log.
- File content is split by content-defined chunking (~32 KB average) and stored once per unique chunk (keyed by BLAKE3/SHA-256 hash).
- Every operation is one transaction.
- A workspace is a normal folder with a `.ply` marker pointing at its project database and task.
- Saves are recorded by a background watcher, and every command scans first to catch anything missed.

## Conventions

- `go test ./...` must pass. `go vet ./...` clean. Format with `gofmt`.
- Every command supports `--json` and never prompts when given its arguments.
- Error messages say what went wrong and the command that fixes it.
- Use plain words in user-facing text (project, task, workspace, label, share, refresh, approve).
