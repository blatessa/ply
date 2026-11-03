package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Task statuses.
const (
	StatusOpen    = "open"
	StatusReady   = "ready"
	StatusApplied = "applied"
	StatusClosed  = "closed"
)

// Task is a piece of work.
type Task struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Owner       string `json:"owner"`
	Assignee    string `json:"assignee,omitempty"`
	Status      string `json:"status"`
	// BaseTask is the task this one is built on, or 0 for the project.
	BaseTask int64 `json:"base_task,omitempty"`
	// BaseVersion is the project version this task's work sits on, directly
	// or through BaseTask.
	BaseVersion int64 `json:"base_version"`
	// BaseSnapshot is the snapshot this task's changes start from: the
	// project at BaseVersion, or BaseTask's latest change.
	BaseSnapshot Hash  `json:"base_snapshot"`
	Reverts      int64 `json:"reverts,omitempty"`
	// ShareSeq counts shares; a share must name the ShareSeq it builds on.
	ShareSeq     int64  `json:"share_seq"`
	SharedBy     string `json:"shared_by,omitempty"`
	NeedsRefresh bool   `json:"needs_refresh,omitempty"`
	// Conflicts lists unresolved conflicts in the task's latest change; a
	// task can't be approved until it's empty. JSON-encoded merge conflicts.
	Conflicts []Conflict `json:"conflicts,omitempty"`
	// SyncedSnapshot is, locally, the task's latest snapshot as of the last
	// share or fetch. Unused on the server.
	SyncedSnapshot Hash      `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Change is a labeled point in a task's work.
type Change struct {
	ID        string    `json:"id"`
	Task      int64     `json:"task"`
	Seq       int       `json:"seq"`
	Label     string    `json:"label"`
	Snapshot  Hash      `json:"snapshot"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Shared    bool      `json:"shared"`
}

// Version is one entry in the project log.
type Version struct {
	Num       int64     `json:"num"`
	Task      int64     `json:"task,omitempty"`
	Snapshot  Hash      `json:"snapshot"`
	AppliedBy string    `json:"applied_by,omitempty"`
	AppliedAt time.Time `json:"applied_at"`
}

// Note is a comment on a task.
type Note struct {
	ID        int64     `json:"id"`
	Task      int64     `json:"task"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// EmptySnapshot is the hash of the snapshot with no files: the project before
// its first task.
var EmptySnapshot = Hash(sha256.Sum256([]byte(manifestHeader)))

func ts(t time.Time) int64      { return t.UnixMilli() }
func fromTS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// Now is the clock used for records; tests may replace it.
var Now = func() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func notFound(what string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return err
}

// ---- tasks

const taskCols = `id, title, description, owner, assignee, status, coalesce(base_task, 0),
	base_version, base_snapshot, coalesce(reverts, 0), share_seq, shared_by, needs_refresh,
	created_at, updated_at, conflicts, synced_snapshot`

// Conflict is an unresolved conflict in a task's files.
type Conflict struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`             // "text" or "binary"
	Ours   string `json:"ours,omitempty"`   // binary: path of one version
	Theirs string `json:"theirs,omitempty"` // binary: path of the other
}

func scanTask(sc interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var snap []byte
	var created, updated int64
	var conflicts string
	var synced []byte
	err := sc.Scan(&t.ID, &t.Title, &t.Description, &t.Owner, &t.Assignee, &t.Status, &t.BaseTask,
		&t.BaseVersion, &snap, &t.Reverts, &t.ShareSeq, &t.SharedBy, &t.NeedsRefresh, &created, &updated,
		&conflicts, &synced)
	if err != nil {
		return t, err
	}
	t.BaseSnapshot = hashFrom(snap)
	t.SyncedSnapshot = hashFrom(synced)
	if err := json.Unmarshal([]byte(conflicts), &t.Conflicts); err != nil {
		return t, fmt.Errorf("task %d conflicts: %w", t.ID, err)
	}
	t.CreatedAt, t.UpdatedAt = fromTS(created), fromTS(updated)
	return t, err
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// PutTask inserts or replaces a task. A zero ID assigns the next number and
// sets t.ID.
func (tx *Tx) PutTask(t *Task) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = Now()
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = t.CreatedAt
	}
	if t.Status == "" {
		t.Status = StatusOpen
	}
	conflicts, _ := json.Marshal(t.Conflicts)
	if t.Conflicts == nil {
		conflicts = []byte("[]")
	}
	var synced any
	if !t.SyncedSnapshot.IsZero() {
		synced = t.SyncedSnapshot[:]
	}
	res, err := tx.exec(`INSERT INTO task (id, title, description, owner, assignee, status,
		base_task, base_version, base_snapshot, reverts, share_seq, shared_by, needs_refresh, created_at, updated_at,
		conflicts, synced_snapshot)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET title = excluded.title, description = excluded.description,
		owner = excluded.owner, assignee = excluded.assignee, status = excluded.status,
		base_task = excluded.base_task, base_version = excluded.base_version,
		base_snapshot = excluded.base_snapshot, reverts = excluded.reverts,
		share_seq = excluded.share_seq, shared_by = excluded.shared_by,
		needs_refresh = excluded.needs_refresh, updated_at = excluded.updated_at,
		conflicts = excluded.conflicts, synced_snapshot = excluded.synced_snapshot`,
		nullID(t.ID), t.Title, t.Description, t.Owner, t.Assignee, t.Status, nullID(t.BaseTask),
		t.BaseVersion, t.BaseSnapshot[:], nullID(t.Reverts), t.ShareSeq, t.SharedBy, t.NeedsRefresh,
		ts(t.CreatedAt), ts(t.UpdatedAt), string(conflicts), synced)
	if err != nil {
		return err
	}
	if t.ID == 0 {
		t.ID, err = res.LastInsertId()
	}
	return err
}

// Task returns a task by number.
func (tx *Tx) Task(id int64) (Task, error) {
	t, err := scanTask(tx.row("SELECT "+taskCols+" FROM task WHERE id = ?", id))
	return t, notFound(fmt.Sprintf("task %d", id), err)
}

// Tasks returns tasks with any of the given statuses (all if none), by number.
func (tx *Tx) Tasks(statuses ...string) ([]Task, error) {
	q := "SELECT " + taskCols + " FROM task"
	var args []any
	if len(statuses) > 0 {
		q += " WHERE status IN (?" + strings.Repeat(", ?", len(statuses)-1) + ")"
		for _, s := range statuses {
			args = append(args, s)
		}
	}
	return tx.tasks(q+" ORDER BY id", args...)
}

// TasksOn returns the tasks built directly on the given task.
func (tx *Tx) TasksOn(id int64) ([]Task, error) {
	return tx.tasks("SELECT "+taskCols+" FROM task WHERE base_task = ? ORDER BY id", id)
}

func (tx *Tx) tasks(q string, args ...any) ([]Task, error) {
	rows, err := tx.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- changes

const changeCols = "id, task, seq, label, snapshot, author, created_at, updated_at, shared"

func scanChange(sc interface{ Scan(...any) error }) (Change, error) {
	var c Change
	var snap []byte
	var created, updated int64
	err := sc.Scan(&c.ID, &c.Task, &c.Seq, &c.Label, &snap, &c.Author, &created, &updated, &c.Shared)
	c.Snapshot = hashFrom(snap)
	c.CreatedAt, c.UpdatedAt = fromTS(created), fromTS(updated)
	return c, err
}

// PutChange inserts or replaces a change (by ID).
func (tx *Tx) PutChange(c *Change) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = Now()
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = c.CreatedAt
	}
	_, err := tx.exec(`INSERT INTO change (`+changeCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET seq = excluded.seq, label = excluded.label,
		snapshot = excluded.snapshot, author = excluded.author, updated_at = excluded.updated_at,
		shared = excluded.shared`,
		c.ID, c.Task, c.Seq, c.Label, c.Snapshot[:], c.Author, ts(c.CreatedAt), ts(c.UpdatedAt), c.Shared)
	return err
}

// DeleteChange removes a change. Only unshared local changes are deleted.
func (tx *Tx) DeleteChange(id string) error {
	_, err := tx.exec("DELETE FROM change WHERE id = ?", id)
	return err
}

// Changes returns a task's changes in order.
func (tx *Tx) Changes(task int64) ([]Change, error) {
	rows, err := tx.query("SELECT "+changeCols+" FROM change WHERE task = ? ORDER BY seq", task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LatestChange returns a task's last change, or ok=false if it has none.
func (tx *Tx) LatestChange(task int64) (Change, bool, error) {
	c, err := scanChange(tx.row("SELECT "+changeCols+" FROM change WHERE task = ? ORDER BY seq DESC LIMIT 1", task))
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// TaskSnapshot is a task's latest state: its last change, or its base if it
// has no changes yet.
func (tx *Tx) TaskSnapshot(t Task) (Hash, error) {
	c, ok, err := tx.LatestChange(t.ID)
	if err != nil || !ok {
		return t.BaseSnapshot, err
	}
	return c.Snapshot, nil
}

// ---- project log

// PutVersion records a project version.
func (tx *Tx) PutVersion(v *Version) error {
	if v.AppliedAt.IsZero() {
		v.AppliedAt = Now()
	}
	_, err := tx.exec(`INSERT OR REPLACE INTO version (num, task, snapshot, applied_by, applied_at)
		VALUES (?, ?, ?, ?, ?)`, v.Num, nullID(v.Task), v.Snapshot[:], v.AppliedBy, ts(v.AppliedAt))
	return err
}

func scanVersion(sc interface{ Scan(...any) error }) (Version, error) {
	var v Version
	var snap []byte
	var at int64
	err := sc.Scan(&v.Num, &v.Task, &snap, &v.AppliedBy, &at)
	v.Snapshot = hashFrom(snap)
	v.AppliedAt = fromTS(at)
	return v, err
}

const versionCols = "num, coalesce(task, 0), snapshot, applied_by, applied_at"

// Version returns project version n. Version 0 is the empty project.
func (tx *Tx) Version(n int64) (Version, error) {
	if n == 0 {
		return Version{Snapshot: EmptySnapshot}, nil
	}
	v, err := scanVersion(tx.row("SELECT "+versionCols+" FROM version WHERE num = ?", n))
	return v, notFound(fmt.Sprintf("project #%d", n), err)
}

// Head returns the current project version.
func (tx *Tx) Head() (Version, error) {
	v, err := scanVersion(tx.row("SELECT " + versionCols + " FROM version ORDER BY num DESC LIMIT 1"))
	if errors.Is(err, sql.ErrNoRows) {
		return Version{Snapshot: EmptySnapshot}, nil
	}
	return v, err
}

// Versions returns the project log, newest first.
func (tx *Tx) Versions() ([]Version, error) {
	rows, err := tx.query("SELECT " + versionCols + " FROM version ORDER BY num DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// VersionOfTask returns the project version a task was applied as.
func (tx *Tx) VersionOfTask(task int64) (Version, error) {
	v, err := scanVersion(tx.row("SELECT "+versionCols+" FROM version WHERE task = ?", task))
	return v, notFound(fmt.Sprintf("applied task %d", task), err)
}

// ---- notes

// PutNote inserts or replaces a note. A zero ID assigns one.
func (tx *Tx) PutNote(n *Note) error {
	if n.CreatedAt.IsZero() {
		n.CreatedAt = Now()
	}
	res, err := tx.exec("INSERT OR REPLACE INTO note (id, task, author, body, created_at) VALUES (?, ?, ?, ?, ?)",
		nullID(n.ID), n.Task, n.Author, n.Body, ts(n.CreatedAt))
	if err != nil {
		return err
	}
	if n.ID == 0 {
		n.ID, err = res.LastInsertId()
	}
	return err
}

// Notes returns a task's notes, oldest first.
func (tx *Tx) Notes(task int64) ([]Note, error) {
	rows, err := tx.query("SELECT id, task, author, body, created_at FROM note WHERE task = ? ORDER BY id", task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		var at int64
		if err := rows.Scan(&n.ID, &n.Task, &n.Author, &n.Body, &at); err != nil {
			return nil, err
		}
		n.CreatedAt = fromTS(at)
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---- settings

// Setting returns a setting's value, or def if unset.
func (tx *Tx) Setting(key, def string) (string, error) {
	var v string
	err := tx.row("SELECT value FROM setting WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	return v, err
}

// SetSetting stores a setting.
func (tx *Tx) SetSetting(key, value string) error {
	_, err := tx.exec("INSERT OR REPLACE INTO setting (key, value) VALUES (?, ?)", key, value)
	return err
}
