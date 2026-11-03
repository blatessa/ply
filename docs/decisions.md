# Design decisions

Choices made where the spec left room. Change them with a note here explaining why.

- **SQLite driver:** pure Go (`modernc.org/sqlite`), so the binary cross-builds for macOS without cgo.
- **Local storage:** one SQLite file per project per machine, holding shared content plus every workspace's saves and undo log.
- **Content:** FastCDC chunking, ~32 KB average; each unique chunk stored once, keyed by hash.
- **Transactions:** every operation (save, share, approve) is one transaction.
- **Workspaces are views** (#53): a folder shows one task at a time, with a `.ply` marker naming its project, task (with title, for prompts and status bars, which read the file rather than running ply) and folder ID. Showing another task is `ply go`, not "switch", to keep it from reading like a branch checkout. Saves and undo history belong to the task, so `ply go` restores a task's unlabeled work exactly; ignored files belong to the folder. Task 0 means the folder shows the project itself: edits made there come along into the next `ply new`, can be shipped, and can't be labeled.
- **`ply new`** builds on the project by default; a bare `--on` builds on the task the folder shows. Bare `--on` takes the next argument as its value only if it looks like a task (`12`, `#12`) or `project`, so `ply new --on "Title"` works.
- **Agents get their own folder:** run by an agent (`PLY_AGENT`, or `CLAUDECODE` which Claude Code sets) in the project's main folder, `ply new` and `ply go` make an extra folder rather than changing files under a person; `--here` opts out. In an extra folder they work in place.
- **Stale editor writes:** after a switch, a save whose content is exactly the previous task's version of a file the switch changed is flagged (status, label, share) until the file changes again; switching tells you to reload open files.
- **After `ply approve`** in a folder showing that task, the folder moves on to show the project, unless unlabeled work would be left behind.
- **Saves are pruned** for tasks that are applied or closed and not shown in any folder, but only when nothing would be lost (no unlabeled work, nothing unshared). A closed task can be reopened.
- **`ply ship`** takes unlabeled work (or named files), merges it onto the current project from the folder's labeled state, and refuses on overlap. It never changes files; in a task folder the shipped edits stay unlabeled until the next refresh drops them.
- **Approval setting:** `approve` is `anyone` (default) or `others` (not the task's owner), set with `ply setting`. Ship applies when allowed, otherwise leaves the task ready.
- **`ply start`** creates the project, labels and shares "Initial files", and makes the folder the project's main folder; approving is left to the person.
- **Recording saves:** a background watcher records saves; every command scans first to catch anything missed.
- **Hashes:** SHA-256 (stdlib, hardware-accelerated) for chunks, blobs and snapshots.
- **Snapshots** are sorted manifests stored as chunked blobs, so a save in a large tree stores only the manifest chunks that changed.
- **Changes are full snapshots.** A task's review diff is base → latest change. Refresh never rewrites earlier changes; it appends a "Refreshed to …" change.
- **Refresh** first merges in changes someone else shared to the task (their changes, then yours on top), then merges onto what the task is built on. Unlabeled work is carried over by a second three-way merge.
- **Conflicts** travel with the task when shared; the server refuses to approve while any remain. Binary conflicts are kept as `name (task 2).ext` / `name (project 5).ext`; removing one moves the other back to the original name (done by the next command).
- **Undo** walks back through workspace actions (label, label --edit, refresh, going back to a save); `ply redo` reverses undos. Restoring files is a three-way merge, so edits made since the undone action survive. Server actions (share, approve, notes) aren't undoable locally.
- **Approving** a stacked task applies the chain bottom-up in one transaction; tasks built on an applied task are re-based on the project automatically.
- **Stale share check:** each share increments the task's `share_seq`; a share must name the seq it builds on.
- **Sync:** fetches send a "have" snapshot list so only differing blobs are listed; chunks move in a framed binary pack, not JSON.
- **Folders:** `ply join` makes the project's main folder; `--separate` folders default to `<main>-<task-slug>` beside it. Only extra folders can be removed.
- **Identity (until M5):** the server trusts the `X-Ply-User` header; the client sends `$PLY_USER` or the OS username.
