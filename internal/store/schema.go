package store

// migrations are applied in order; the database's user_version is the number
// applied. Never edit a released migration; append a new one.
var migrations = []string{
	// 1: content, records, local state.
	`
-- Content. A chunk is a piece of file content, stored once by hash.
-- comp: 0 = raw, 1 = zstd.
CREATE TABLE chunk (
	id    INTEGER PRIMARY KEY,
	hash  BLOB NOT NULL UNIQUE,
	size  INTEGER NOT NULL,
	comp  INTEGER NOT NULL,
	data  BLOB NOT NULL
);

-- A blob is a whole file's content: its chunks' hashes in order, packed
-- 32 bytes each. hash is the SHA-256 of the full content.
CREATE TABLE blob (
	id     INTEGER PRIMARY KEY,
	hash   BLOB NOT NULL UNIQUE,
	size   INTEGER NOT NULL,
	chunks BLOB NOT NULL
);

-- The project log: each approval makes a new numbered version.
CREATE TABLE version (
	num        INTEGER PRIMARY KEY,
	task       INTEGER REFERENCES task(id),
	snapshot   BLOB NOT NULL,
	applied_by TEXT NOT NULL,
	applied_at INTEGER NOT NULL
);

-- A task. base_task is set when it's built on another task; base_version is
-- the project version its work currently sits on (directly, or through the
-- task underneath).
CREATE TABLE task (
	id           INTEGER PRIMARY KEY,
	title        TEXT NOT NULL,
	description  TEXT NOT NULL DEFAULT '',
	owner        TEXT NOT NULL,
	assignee     TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL DEFAULT 'open',
	base_task    INTEGER REFERENCES task(id),
	base_version INTEGER NOT NULL,
	base_snapshot BLOB NOT NULL,
	reverts      INTEGER REFERENCES task(id),
	share_seq    INTEGER NOT NULL DEFAULT 0,
	shared_by    TEXT NOT NULL DEFAULT '',
	needs_refresh INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
);

-- A labeled change. id is stable across edits (label --edit replaces its
-- snapshot). seq orders a task's changes.
CREATE TABLE change (
	id         TEXT PRIMARY KEY,
	task       INTEGER NOT NULL REFERENCES task(id),
	seq        INTEGER NOT NULL,
	label      TEXT NOT NULL,
	snapshot   BLOB NOT NULL,
	author     TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	shared     INTEGER NOT NULL DEFAULT 0,
	UNIQUE (task, seq)
);

CREATE TABLE note (
	id         INTEGER PRIMARY KEY,
	task       INTEGER NOT NULL REFERENCES task(id),
	author     TEXT NOT NULL,
	body       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX note_task ON note(task);

-- Key/value settings (project settings on the server, local config locally).
CREATE TABLE setting (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

-- Local only: workspaces, their saves, and the action log for undo.
CREATE TABLE workspace (
	id         INTEGER PRIMARY KEY,
	path       TEXT NOT NULL UNIQUE,
	task       INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE save (
	id        INTEGER PRIMARY KEY,
	workspace INTEGER NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
	snapshot  BLOB NOT NULL,
	at        INTEGER NOT NULL
);
CREATE INDEX save_ws ON save(workspace, id);

-- Each action records enough to undo it. undone marks actions reversed by
-- an undo (which is itself an action).
CREATE TABLE action (
	id        INTEGER PRIMARY KEY,
	workspace INTEGER NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
	kind      TEXT NOT NULL,
	data      TEXT NOT NULL,
	at        INTEGER NOT NULL,
	undone    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX action_ws ON action(workspace, id);

-- Local only: the cached file state from the last scan of each workspace,
-- so scans only hash files whose stat changed.
CREATE TABLE scan (
	workspace INTEGER NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
	path      TEXT NOT NULL,
	size      INTEGER NOT NULL,
	mtime     INTEGER NOT NULL,
	ino       INTEGER NOT NULL,
	mode      INTEGER NOT NULL,
	hash      BLOB NOT NULL,
	PRIMARY KEY (workspace, path)
) WITHOUT ROWID;
`,
	// 2: conflicts travel with a task; locally, the last state synced with
	// the server (to merge in changes someone else shared).
	`
ALTER TABLE task ADD COLUMN conflicts TEXT NOT NULL DEFAULT '[]';
ALTER TABLE task ADD COLUMN synced_snapshot BLOB;
`,
	// 3: a folder shows one task at a time (task 0: the project itself, with
	// its own base). Saves and actions belong to the task, not the folder, so
	// switching away keeps them; a task-0 timeline belongs to its folder
	// (workspace is 0 for real tasks). main marks the project's own folder.
	`
ALTER TABLE workspace ADD COLUMN main INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workspace ADD COLUMN base_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workspace ADD COLUMN base_snapshot BLOB;

CREATE TABLE save3 (
	id        INTEGER PRIMARY KEY,
	task      INTEGER NOT NULL,
	workspace INTEGER NOT NULL DEFAULT 0,
	snapshot  BLOB NOT NULL,
	at        INTEGER NOT NULL
);
INSERT INTO save3 SELECT s.id, w.task, 0, s.snapshot, s.at FROM save s JOIN workspace w ON w.id = s.workspace;
DROP TABLE save;
ALTER TABLE save3 RENAME TO save;
CREATE INDEX save_timeline ON save(task, workspace, id);

CREATE TABLE action3 (
	id        INTEGER PRIMARY KEY,
	task      INTEGER NOT NULL,
	workspace INTEGER NOT NULL DEFAULT 0,
	kind      TEXT NOT NULL,
	data      TEXT NOT NULL,
	at        INTEGER NOT NULL,
	undone    INTEGER NOT NULL DEFAULT 0
);
INSERT INTO action3 SELECT a.id, w.task, 0, a.kind, a.data, a.at, a.undone FROM action a JOIN workspace w ON w.id = a.workspace;
DROP TABLE action;
ALTER TABLE action3 RENAME TO action;
CREATE INDEX action_timeline ON action(task, workspace, id);
`,
}
