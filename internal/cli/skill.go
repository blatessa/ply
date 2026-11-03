package cli

// Skill is what `ply skill` prints: a short guide for agents using Ply.
const Skill = `# Using Ply

Ply is source control built on tasks. The project changes only when a task is
approved. You do all work inside a task's workspace: a normal folder.

## Workflow

1. ply new "Short title" --description "what and why"   # creates a task + workspace folder; cd into it
2. Edit files normally. Every save is recorded automatically.
3. ply label "What changed"        # mark a good point as a change (ply label --edit folds more work into the last one)
4. ply share                       # send labeled changes to the server for review
5. ply ready                       # mark the task ready for review when it's done

Repeat 2-4 as needed. Read feedback with ply review; reply with ply note "...".

## Useful commands

- ply status        what's unlabeled, conflicts, whether a refresh is needed
- ply diff          unlabeled work; ply diff <task> shows a task's changes
- ply refresh       bring in project changes (or changes others shared to this task)
- ply conflicts     list conflicts; resolve by editing out the markers (or deleting one of the two
                    side-by-side binary copies), then label and share
- ply undo          undo the last action; ply history + ply undo <save> to go back to any save
- ply tasks         list tasks; ply get <task> to make a workspace for an existing task
- ply new "..." --copy-ignored   reuse node_modules etc. from the current workspace

## Rules

- Never edit an exported folder expecting Ply to notice; work only in workspaces.
- Add --json to any command for machine-readable output. Commands never prompt.
- Errors name the command that fixes them, e.g. "run ` + "`ply refresh`" + `". Run it and retry.
- Project-specific instructions live in the project's AGENTS.md.
`
