# Ply

Source control for a small team and its AI agents. All work happens through tasks; a project is
just the result of its approved tasks, applied one after another, layer by layer.

- Central server, one SQLite file per project, no git underneath.
- Every save is recorded; anything in a workspace can be undone.
- Agents use it the way people do.

## Try it

```sh
go build -o ply ./cmd/ply
./ply server start &                      # http://localhost:7070, no setup
./ply server new demo
./ply join http://localhost:7070/demo     # makes ./demo for workspaces
cd demo && ply new "Initial import"       # makes ./initial-import
cd initial-import && echo hi > README.md
ply label "Add readme" && ply share && ply approve
ply log
```

Run `ply help` for every command, and `ply skill` for the short guide agents load.

**Status:** early development. See [docs/spec.md](docs/spec.md) for the design and the
[issues](https://github.com/blatessa/ply/issues) for progress. Contributors and agents: read
[AGENTS.md](AGENTS.md).
