## Overview

Ply is a source control tool for a small team and its AI agents. All work happens through tasks; a project is just the result of its approved tasks, applied one after another, layer by layer (hence the name). It has a central server, records every save, stores everything in SQLite, does not build on git, and runs on Mac and Linux.

Design principles:

- **The project is the result of its tasks.** Nobody edits the project. Work happens in a task, and the project changes only when a task is approved.
- **Only commands change things.** On your machine, nothing syncs, merges or changes your files on its own. The one automatic thing is recording saves, which never changes your files.
- **Nothing is lost while you work.** Every save is recorded, and anything you do in a workspace can be undone. Applied work can be reverted with a new task.
- **Plain words.** Project, task, workspace, label, share, refresh, approve. Users never need to know how it works inside.
- **One server, one file.** One server everyone works against, local or hosted, installed in one command. Each project's whole history is one SQLite file.
- **Agents use it the way people do.** An agent works as you, or plugs in as an integration through the same commands and API.

**How to read this spec.** It describes what Ply does and how it should feel to use. The concepts, behavior and principles are requirements. Anything about how to build it (schemas, numbers, settings, command spellings) is a starting point, not a rule: whoever builds Ply should make those calls and measure. Ply is written in **Go**.

## Core concepts

Three things matter: the project, tasks, and workspaces. The rest are the actions on them.

| Term | What it is |
| --- | --- |
| **Project** | The current state of the code: the result of every approved task, applied in order. Read-only. Nobody works in it directly; it only changes when a task is approved. Its history is the list of applied tasks; each approval makes a new numbered version (#1, #2, ...). |
| **Task** | A piece of work: a title and a short description of what and why, plus its labeled changes, its notes, the person responsible for it, and who's working on it. What gets shared and reviewed. |
| **Workspace** | A normal folder on your machine showing one task's files at a time. Where work actually happens. Usually one folder per project; switching it to another task keeps each task's work separate. |
| **Save** | A snapshot recorded every time a file in a workspace is saved. Builds the task's timeline, so anything can be undone. Local only; never shared. |
| **Change** | A labeled point in a workspace's timeline ("Handle + in emails"). A task's changes are what reviewers see and what gets applied. |
| **Share** | Share a task's latest labeled changes with the server, so others can see and review them. |
| **Refresh** | Bring a workspace up to date with what its task is built on: the current project, or the task underneath. Only happens when you run it. |
| **Approve** | Apply a task (and any tasks it's built on) to the project. The task is done. |
| **Note** | A comment on a task, from anyone. Feedback lives here; the task and its workspace just carry on. |
| **Export** | Write out the project's files (now, or as of a task) as a plain, untracked folder or zip, for building, deploying or just looking. |
| **Undo** | Reverse any action in a workspace, or go back to any earlier save. |
| **Revert** | Undo an applied task by creating a new task that reverses it. It goes through the same steps as any task. |

## Lifecycle of a task

A task goes from created to applied in five steps, repeating steps 2–4 until it's approved.

1. **Create a task.** The folder you're in switches to it, starting from the current project.
2. **Work.** A person or agent edits files. Every save is recorded.
3. **Label changes** at good spots. Until the task is approved, a change can still be edited (fold a typo fix into it instead of adding a new change).
4. **Share the task.** Its labeled changes go to the server, where others can see and review them. Share as often as you like. If someone else shared newer changes to the same task, Ply refuses until you refresh. When it's finished, mark it ready (`ply ready`) so people know it's not a draft.
5. **Review.**
   - Approve → the task is applied to the project. Who can approve is a project setting (default: anyone, people or agents; can optionally require a passing check). If other tasks were applied in the meantime, approve combines with them itself. It refuses only on a real conflict, which needs a refresh.
   - Not ready → leave notes on the task. Whoever's on it keeps working in the same workspace and shares again.

Saves belong to the task and are kept until it's applied or closed. The labeled changes stay with the task, in the project's history.

Statuses: **open**, **ready**, **applied**, **closed**. `ply close` closes a task without applying it; it can be reopened. Ready is a signal, not a gate: sharing more keeps a task ready, and approving doesn't require it unless the project's settings say so. If applied work turns out to be wrong, `ply revert <task>` creates a new task that undoes it.

**Building on a task that isn't approved yet**

You shouldn't have to wait for approval to keep going. A task can be based on another task instead of the project.

- `ply new "Add password reset" --on`, run in a folder showing "Fix login bug", starts the new task on top of that task's latest change. Without `--on`, `ply new` starts from the project; `--on <task>` builds on any task. Ply always says what the new task is built on.
- If the earlier task gets more changes, `ply refresh` brings them in, same as catching up with the project.
- If a task underneath gets new or edited changes, Ply marks the tasks on top as needing a refresh. When the earlier task is applied, the later one is simply based on the project again.
- Tasks can stack more than one deep. Each task shows only its own changes. Approving a task that's built on others applies them first, in order, then this one.
- Closing a task that others are built on marks them as needing a refresh. Refreshing moves them onto what the closed task was built on, without its changes.

## Commands

Every command does one thing, and only commands change files. Commands run inside a workspace act on that workspace's task. This is the intended set; exact names and flags are up to the builders.

| Command | What it does |
| --- | --- |
| `ply join <url>` | Connect to a project on a server and make a folder for it. Opens the browser to sign in. |
| `ply start <url>` | Create a project from the current folder: its files become a first task, "Initial files", and the folder becomes the project's folder. |
| `ply new "Fix login bug"` | Create a task from the current project and show it in this folder. The task you leave keeps its unlabeled work. |
| `ply new "..." --on [<task>]` | Build on the task this folder shows, or on any task. |
| `ply new "..." --separate` | Make an extra folder for the task instead (copying ignored files like node\_modules over), for parallel work. |
| `ply go <task>` | Show another task in this folder: yours or anyone's. Unlabeled work stays with the task you left; switching back restores it. Ignored files aren't touched. |
| `ply ship "Fix typo"` | Wrap what you just changed into its own small task and apply it, if the project lets you approve your own tasks; otherwise share it, ready for review. |
| `ply status` | What's unlabeled, any conflicts, what the task is built on, and whether it needs a refresh. |
| `ply diff` | Show changes: unlabeled work, or a task's changes. |
| `ply tasks` | List tasks, who's on them, and their status. |
| `ply label "Handle + in emails"` | Label the work since the last label as a change. |
| `ply label --edit` | Fold new work into the latest change. Works until the task is approved. |
| `ply history` | Show this workspace's timeline: saves and labeled changes. |
| `ply undo` | Undo the last action in this workspace. Given a save from the history, go back to that save. |
| `ply share` | Share this task's labeled changes with the server. Refuses if someone else shared newer changes to it; refresh first. |
| `ply refresh` | Bring this workspace up to date with what its task is built on. Reports conflicts. |
| `ply conflicts` | List unresolved conflicts in this workspace. |
| `ply review <task>` | Show a task's changes and notes. |
| `ply ready` | Mark this task ready for review. |
| `ply close <task>` | Close a task without applying it. ply reopen brings it back. |
| `ply approve <task>` | Apply the task (and any tasks under it) to the project. Combines with newer work itself; refuses only on a conflict. |
| `ply note <task> "notes"` | Add a note to a task. |
| `ply revert <task>` | Create a new task that undoes an applied task. |
| `ply remove` | Remove an extra folder. The task's work stays. |
| `ply export <folder> [--at <task>]` | Write the project's files as a plain, untracked folder. --at \<task> exports the project as of an applied task; --task \<task> exports an open task's latest version. |
| `ply log` | The project's history: applied tasks, newest first. |
| `ply skill` | Print the instructions file an agent loads to learn Ply. |
| `ply server start` | Run a server on this machine, no setup needed. |
| `ply server` | Open the server dashboard (on the server machine). |
| `ply server new <project>` | Create a new, empty project on the server. |
| `ply server upgrade` | Upgrade the server to the latest version. |
| `ply server purge` | Admin only: remove a file or version from history for good, for leaked secrets or huge files added by mistake. |

## Storage

Everything lives in one SQLite file per project, on each machine and on the server: records and every stored version of every file. SQLite holds up well as file storage when it's configured for it.

**Requirements**

- One file per project. The server also keeps one small file for accounts and settings; server backups, restores and moves always include it along with the project files.
- File contents stored once no matter how many snapshots use them, and a save only stores what changed.
- No file size limit and nothing stored outside the database. Large files are split into pieces so an edit doesn't store the whole file again.
- Saves, shares and approvals are all-or-nothing: a crash never leaves history half-written.
- Saves and actions are local to a workspace and go away with it. Shared history (tasks, changes, notes, the project log) is never deleted, except by an admin purge for a leaked secret or a huge file added by mistake.
- Many people and agents can read while one writes, on the server and locally.

**Guidance**

- SQLite reads values under about 100 KB faster than separate files, and larger ones slower, so keep stored pieces small (content-defined chunking, as backup tools like restic use, averaging tens of KB).
- Use production settings: WAL mode, `synchronous=NORMAL`, a busy timeout, a larger page size and cache, memory-mapped reads. Group each operation into one transaction.
- Keep everything in one database file: in WAL mode, a transaction across two attached files isn't all-or-nothing if the machine crashes.
- Fossil (by SQLite's author) stores all file content in a SQLite table and is a useful reference.

**What needs storing:** file contents; snapshots (which file version sits at which path); the project log (applied tasks in order); tasks (title, description, owner, assignee, status, what it's based on: a point in the project or another task); changes (stable ID, label, snapshot, author, whether shared); notes and approvals; people and agent integrations; and, locally, workspaces, saves and an action log for undo.

## Local machine: workspaces and recording saves

Each task has its own normal folder, and Ply records every save in it. Recording saves is the only automatic thing Ply does, and it never changes your files.

- **Workspaces are normal folders** anywhere on disk, each knowing which project and task it belongs to. Users open them in any editor.
- **Creating a workspace should be near-instant,** even for big projects, and shouldn't force a full reinstall of dependencies (copy-on-write file copies are one way).
- **Every file save is recorded** without the user doing anything, including edits made while Ply wasn't watching; the next command catches up first.
- **An ignore list** is a normal file in the project, changed through tasks like any other. It keeps things like dependencies and build output out of history, with sensible defaults.

## Server

Setting up a Ply server takes one command, modeled on 37signals' ONCE: run it locally with no setup at all, or install it on a machine with a domain name for the team. The server is the same `ply` program in server mode. It holds the project, tasks and notes in its own SQLite database, and applies a task only when it's approved.

One server holds any number of projects, each in its own database file at its own address (`https://code.yourteam.com/<project>`). Accounts are shared across the server; people are added to each project. A new project starts empty, from the web app or `ply server new <project>`, and its first task brings in the files.

**Install**

- `curl https://get.ply.dev | sh` on any Linux or Mac machine: a cloud server, a home server, or a laptop.
- It downloads the `ply` program, installs it as a background service, and starts a short setup in the terminal: the domain name and the first admin account.
- For scripted installs: `PLY_INTERACTIVE=false` with the answers as settings.
- No Docker or database server needed, unlike ONCE: Ply is one program plus a SQLite file.

**Running it locally**

- `ply server start` runs the server on your own machine, in the terminal, with no install step, domain or certificate: `http://localhost:7070`.
- Join it like any server: `ply join http://localhost:7070/myproject`. Good for working solo, or trying Ply out. No sign-in is needed on your own machine.
- Teammates on the same network can join using your machine's address (`http://your-laptop.local:7070/...`). It's only reachable while your machine is on. Without HTTPS, browsers don't allow passkeys, so they join with an invite link that signs them in directly.
- `ply server start --background` keeps it running as a background service instead.
- Moving to a real server later: install there, restore a backup of the local server, and have everyone `ply join` the new address.

**Managing it**

- `ply server` opens a dashboard in the terminal: projects, people and agents, disk use, recent activity, backups, settings.
- Everything in the dashboard is also a plain command (`ply server backup`, `ply server invite`, ...), so scripts and agents can run the server too.
- HTTPS: the certificate for the domain is set up during install and renewed by the service.
- Backups: a copy of every project database and the accounts file, to a location you choose, on a schedule you set. Restore with `ply server restore <file>`.
- Upgrades: `ply server upgrade`. Automatic upgrades are a setting, off by default.
- Moving servers: install on the new machine, restore the latest backup, point the domain at it.
- Purge: `ply server purge` removes a file or version from history for good, for a leaked secret or a huge file added by mistake. Admin only. It can't reach copies people already fetched, so rotate a leaked secret anyway.

**Web app**

The server includes a full web app, served from the same address, for everything that's easier in a browser:

- A "needs your attention" view: tasks ready for you to review, notes that mention you, and new notes on your tasks.
- Browse the project's files and history (applied tasks).
- See all tasks and their status, including what each is built on.
- Review: read a task's changes as diffs, read and write notes, approve.
- Create and assign tasks, including to agent integrations.
- Admin: projects, people and invites, agent integrations, project settings (like who can approve), backups and upgrades.

The web app, the CLI and integrations all use the same server API, so anything one can do, the others can too.

**Joining**

- `ply join https://code.yourteam.com/myproject` opens the browser to sign in with a passkey (on a server with HTTPS), then stores a token locally.
- Teammates are added with an invite link.
- Agent integrations get their token when an admin adds them on the server.

**Sharing a task**

1. Client sends the task and the IDs of its labeled changes.
2. Server replies with which file contents it's missing.
3. Client uploads only those, plus the change records.
4. Server stores everything in one transaction.

**Getting the latest**

- Commands that need the server's state (`ply tasks`, `ply refresh`, `ply get`, `ply log`) fetch it when run, downloading only file contents the machine doesn't already have.
- Nothing is fetched or sent from your machine unless a command asks for it. The server can tell people (in the web app) and integrations what needs their attention, but it never changes anyone's files.

## Refreshing a task and conflicts

`ply refresh` brings a workspace up to date with what its task is built on (the current project, or the task underneath), and it only happens when you run it. Labeled and unlabeled work both come along.

**What refresh does**

- Compares three versions of each file: the project the task started from, the task's version, and the current project.
- Only one side changed a file → take that side.
- Both changed different parts of a file → combine line by line.
- Both changed the same lines, or a binary file changed on both sides → **conflict**.
- Records the result as a change labeled "Refreshed to project #N" and moves the task's base to the current project. Like everything else, it can be undone.

**Conflicts**

- The file is written with both versions clearly marked, and the conflict is listed by `ply conflicts`.
- Binary files can't be marked, so both versions are kept side by side. Whichever version you keep ends up under the original name.
- You can keep working and labeling. The task can't be approved until conflicts are cleared.
- A conflict is cleared once its markers are gone, or one of the two binary versions is removed.

**What reviewers see:** the difference between the project the task is based on and the task's latest change, so refreshes don't clutter the review.

## Exports

An export is the project's files as a plain folder, with no tracking: for building, deploying, running or just looking. Working on the project always goes through a task and a workspace.

- **Locally:** `ply export ./somewhere` writes the current project; `--at <task>` writes the project as of when that applied task went in. `--task <task>` exports an open task's latest version instead, e.g. for a preview deploy.
- **From the server:** a download address returns the same thing as a zip, e.g. `https://code.yourteam.com/myproject/export?at=<task>`. Build and deploy machines need only a token, not Ply installed.
- **Export info:** every export includes a plain text file `.plyexport` saying what it is (project, the last applied task or the open task and its version, date), so you can tell which version a deployed copy is. It's for people to read; Ply never reads it. It can be left out.
- Editing an exported folder does nothing; Ply doesn't know about it.

## Agents

Agents show up in Ply in two ways. Ply needs nothing special for the first, and only an integration point for the second.

**1. Agents working as you**

An agent on your machine codes, labels and shares tasks for you, under your name and your login. To Ply it's just you. Nothing to build beyond a CLI that's easy for an agent to drive:

- Every command takes `--json` and never prompts when given its arguments.
- Errors say what went wrong and the command that fixes it ("project has changed: run `ply refresh`").
- `ply skill` prints a short instructions file an agent loads to learn Ply. Project-specific instructions stay in the project's own AGENTS.md.
- `ply new --separate` makes an extra folder and copies ignored files like `node_modules` into it (copy-on-write where possible), so a parallel agent doesn't reinstall everything.

**2. Agents as integrations**

An agent that responds to tasks and notes inside Ply, the way GitHub's Copilot agent works on an assigned issue. Ply doesn't ship an agent; it gives any agent (Claude Code, Codex, your own script) a way to plug in:

- An admin adds an integration on the server: a name, an address to send events to, and a token. It appears as a teammate, clearly marked as an agent.
- Assign a task to it, or mention it in a note, and Ply sends it an event ("task assigned," "new note") with the task's details.
- The integration does the work through the same CLI or API with its own token: makes a workspace, labels changes, shares the task, and replies with notes.
- You steer it with notes, like a teammate. The person who assigned the task stays responsible for it.
- Integrations have the same access as anyone on the project unless an admin limits them.
- **Automated checks work the same way.** A test runner plugs in as an integration: it hears when a task is shared, runs the tests, and posts the result as a note. The approval setting can optionally require a passing check before a task can be approved.

## Non-goals

- Git compatibility, or importing git repositories.
- Branches. Tasks, including tasks built on tasks, replace them.
- Merging that understands code structure.
- Windows. Ply runs on Mac and Linux.
