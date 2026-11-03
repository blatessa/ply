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
cd my-app                                 # an existing folder of code
ply start http://localhost:7070/my-app    # project + first task "Initial files", shared
ply approve                               # the project now has its starting files
ply new "Add readme"                      # a task, shown in this same folder
echo hi > README.md
ply label "Add readme" && ply share && ply approve
echo typo fix >> README.md && ply ship "Fix typo"   # small fix straight to the project
ply log
```

Teammates join with `ply join http://localhost:7070/my-app`, which makes a folder for the project.
`ply switch <task>` shows another task in your folder; your unlabeled work stays with the task you left.

Run `ply help` for every command, and `ply skill` for the short guide agents load.

**Status:** early development. See [docs/spec.md](docs/spec.md) for the design and the
[issues](https://github.com/blatessa/ply/issues) for progress. Contributors and agents: read
[AGENTS.md](AGENTS.md).
