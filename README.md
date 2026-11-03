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
`ply go <task>` shows another task in your folder; your unlabeled work stays with the task you left.

### Showing the task in your prompt or status bar

Each project folder has a `.ply` file at its root saying what it shows, so a shell prompt or editor
status bar can display it without running Ply:

```json
{ "project": "http://localhost:7070/my-app", "task": 12, "title": "Fix login", "folder": 1 }
```

`task` is 0 when the folder shows the project itself. For example, a [starship](https://starship.rs)
module (needs `jq`):

```toml
[custom.ply]
when = 'd=$PWD; while [ "$d" != / ] && [ ! -f "$d/.ply" ]; do d=$(dirname "$d"); done; [ -f "$d/.ply" ]'
command = 'd=$PWD; while [ ! -f "$d/.ply" ]; do d=$(dirname "$d"); done; jq -r "if .task == 0 then \"project\" else \"#\\(.task) \\(.title)\" end" "$d/.ply"'
format = "[ply $output]($style) "
```

Run `ply help` for every command, and `ply skill` for the short guide agents load.

**Status:** early development. See [docs/spec.md](docs/spec.md) for the design and the
[issues](https://github.com/blatessa/ply/issues) for progress. Contributors and agents: read
[AGENTS.md](AGENTS.md).
