package cli

// Skill is what `ply skill` prints: a short guide for agents using Ply.
const Skill = `# Using Ply

Ply is source control built on tasks. The project changes only when a task is
approved. A project folder is a normal folder that shows one task at a time;
each task keeps its own saves and unlabeled work, so switching loses nothing.

## Workflow

1. ply new "Short title" --description "what and why"   # creates a task and shows it in this folder
2. Edit files normally. Every save is recorded automatically.
3. ply label "What changed"        # mark a good point as a change (ply label --edit folds more work into the last one)
4. ply share                       # send labeled changes to the server for review
5. ply ready                       # mark the task ready for review when it's done

Repeat 2-4 as needed. Read feedback with ply review; reply with ply note "...".
For a small fix, ply ship "Fix typo" [files...] wraps unlabeled work into its own task and applies it.

## Useful commands

- ply status        what's unlabeled, conflicts, whether a refresh is needed
- ply diff          unlabeled work; ply diff <task> shows a task's changes
- ply refresh       bring in project changes (or changes others shared to this task)
- ply conflicts     list conflicts; resolve by editing out the markers (or deleting one of the two
                    side-by-side binary copies), then label and share
- ply undo          undo the last action; ply history + ply undo <save> to go back to any save
- ply tasks         list tasks; ply switch <task> shows one in this folder (ply switch project: the project)
- ply new "..." --on               build on the task this folder shows (--on <task>: any task)
- ply new "..." --separate         an extra folder (ignored files like node_modules are copied over);
                                   ply remove deletes it

## Agents

Run in the project's main folder, ply new and ply switch give you your own folder
(so a person's files don't change under them) and print its path: cd there and work.
This happens when PLY_AGENT or CLAUDECODE is set; set PLY_AGENT=1 if your agent sets
neither. Pass --here only if you're told to work in the main folder.

## Rules

- Never edit an exported folder expecting Ply to notice; work only in project folders.
- Add --json to any command for machine-readable output. Commands never prompt.
- Errors name the command that fixes them, e.g. "run ` + "`ply refresh`" + `". Run it and retry.
- Project-specific instructions live in the project's AGENTS.md.
`
