# Design decisions

Choices made where the spec left room. Change them with a note here explaining why.

- **SQLite driver:** pure Go (`modernc.org/sqlite`), so the binary cross-builds for macOS without cgo.
- **Local storage:** one SQLite file per project per machine, holding shared content plus every workspace's saves and undo log.
- **Content:** FastCDC chunking, ~32 KB average; each unique chunk stored once, keyed by hash.
- **Transactions:** every operation (save, share, approve) is one transaction.
- **Workspaces:** a normal folder with a `.ply` marker naming its project and task.
- **Recording saves:** a background watcher records saves; every command scans first to catch anything missed.
