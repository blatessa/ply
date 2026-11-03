# Design decisions

Choices made where the spec left room. Change them with a note here explaining why.

- **SQLite driver:** pure Go (`modernc.org/sqlite`), so the binary cross-builds for macOS without cgo.
- **Local storage:** one SQLite file per project per machine, holding shared content plus every workspace's saves and undo log.
- **Content:** FastCDC chunking, ~32 KB average; each unique chunk stored once, keyed by hash.
- **Transactions:** every operation (save, share, approve) is one transaction.
- **Workspaces:** a normal folder with a `.ply` marker naming its project and task.
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
- **Workspaces** default to `<project folder>/<task-slug>`, where the project folder is made by `ply join`.
- **Identity (until M5):** the server trusts the `X-Ply-User` header; the client sends `$PLY_USER` or the OS username.
