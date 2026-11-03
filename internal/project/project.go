// Package project holds the rules for a project on the server: creating
// tasks, sharing their changes, approving them into the project, and the
// task statuses. Each operation is one transaction.
package project

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/blatessa/ply/internal/merge"
	"github.com/blatessa/ply/internal/plyerr"
	"github.com/blatessa/ply/internal/store"
)

// Project is one project's database and the operations on it.
type Project struct {
	Name string
	DB   *store.DB
}

func New(name string, db *store.DB) *Project { return &Project{Name: name, DB: db} }

// TaskInfo is a task with what people need to see alongside it.
type TaskInfo struct {
	store.Task
	Changes []store.Change `json:"changes,omitempty"`
	Notes   []store.Note   `json:"notes,omitempty"`
	// Latest is the snapshot of the task's latest change (or its base).
	Latest store.Hash `json:"latest"`
	// Refresh says why the task needs a refresh, if it does.
	Refresh string `json:"refresh,omitempty"`
	// Version is the project version it was applied as, if applied.
	Version int64 `json:"version,omitempty"`
}

// State is the project overview.
type State struct {
	Name  string        `json:"name"`
	Head  store.Version `json:"head"`
	Tasks []TaskInfo    `json:"tasks"`
}

func notFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		msg := strings.TrimSuffix(err.Error(), ": "+store.ErrNotFound.Error())
		return plyerr.New(plyerr.NotFound, "ply tasks", "%s doesn't exist", msg)
	}
	return err
}

// State returns the head and tasks. With all false, applied and closed
// tasks are left out.
func (p *Project) State(ctx context.Context, all bool) (*State, error) {
	s := &State{Name: p.Name}
	err := p.DB.Read(ctx, func(tx *store.Tx) error {
		var err error
		if s.Head, err = tx.Head(); err != nil {
			return err
		}
		statuses := []string{store.StatusOpen, store.StatusReady}
		if all {
			statuses = nil
		}
		tasks, err := tx.Tasks(statuses...)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			info, err := taskInfo(tx, t, false)
			if err != nil {
				return err
			}
			s.Tasks = append(s.Tasks, *info)
		}
		return nil
	})
	return s, err
}

// Task returns a task with its changes and notes.
func (p *Project) Task(ctx context.Context, id int64) (*TaskInfo, error) {
	var info *TaskInfo
	err := p.DB.Read(ctx, func(tx *store.Tx) error {
		t, err := tx.Task(id)
		if err != nil {
			return notFound(err)
		}
		info, err = taskInfo(tx, t, true)
		return err
	})
	return info, err
}

func taskInfo(tx *store.Tx, t store.Task, detail bool) (*TaskInfo, error) {
	info := &TaskInfo{Task: t}
	var err error
	if info.Latest, err = tx.TaskSnapshot(t); err != nil {
		return nil, err
	}
	if detail {
		if info.Changes, err = tx.Changes(t.ID); err != nil {
			return nil, err
		}
		if info.Notes, err = tx.Notes(t.ID); err != nil {
			return nil, err
		}
	}
	if t.Status == store.StatusApplied {
		if v, err := tx.VersionOfTask(t.ID); err == nil {
			info.Version = v.Num
		}
	} else {
		if info.Refresh, err = refreshReason(tx, t); err != nil {
			return nil, err
		}
	}
	return info, nil
}

// refreshReason says why a task should be refreshed, or "" if it's up to
// date with what it's built on.
func refreshReason(tx *store.Tx, t store.Task) (string, error) {
	if t.BaseTask != 0 {
		bt, err := tx.Task(t.BaseTask)
		if err != nil {
			return "", err
		}
		switch bt.Status {
		case store.StatusClosed:
			return fmt.Sprintf("task #%d it's built on was closed", bt.ID), nil
		case store.StatusApplied:
			return fmt.Sprintf("task #%d it's built on was applied", bt.ID), nil
		}
		latest, err := tx.TaskSnapshot(bt)
		if err != nil {
			return "", err
		}
		if latest != t.BaseSnapshot {
			return fmt.Sprintf("task #%d it's built on has new changes", bt.ID), nil
		}
		return "", nil
	}
	head, err := tx.Head()
	if err != nil {
		return "", err
	}
	if head.Num > t.BaseVersion && head.Snapshot != t.BaseSnapshot {
		return fmt.Sprintf("the project has moved on to #%d", head.Num), nil
	}
	return "", nil
}

// Upstream is what a task is built on right now: the project, or the
// nearest open task underneath it (skipping closed ones).
type Upstream struct {
	Task     int64      `json:"task,omitempty"` // 0 for the project
	Version  int64      `json:"version"`
	Snapshot store.Hash `json:"snapshot"`
	Label    string     `json:"label"` // "project #5" or "task #3"
}

func upstream(tx *store.Tx, t store.Task) (Upstream, error) {
	bt := t.BaseTask
	for bt != 0 {
		b, err := tx.Task(bt)
		if err != nil {
			return Upstream{}, err
		}
		if b.Status == store.StatusOpen || b.Status == store.StatusReady {
			latest, err := tx.TaskSnapshot(b)
			return Upstream{Task: b.ID, Version: b.BaseVersion, Snapshot: latest, Label: fmt.Sprintf("task #%d", b.ID)}, err
		}
		if b.Status == store.StatusApplied {
			break
		}
		bt = b.BaseTask // closed: move onto what it was built on
	}
	head, err := tx.Head()
	return Upstream{Version: head.Num, Snapshot: head.Snapshot, Label: fmt.Sprintf("project #%d", head.Num)}, err
}

// Upstream returns what the task should be refreshed onto.
func (p *Project) Upstream(ctx context.Context, id int64) (Upstream, error) {
	var u Upstream
	err := p.DB.Read(ctx, func(tx *store.Tx) error {
		t, err := tx.Task(id)
		if err != nil {
			return notFound(err)
		}
		u, err = upstream(tx, t)
		return err
	})
	return u, err
}

// CreateTask makes a new task built on the project (on == 0) or on another
// open task.
func (p *Project) CreateTask(ctx context.Context, actor, title, description string, on int64) (*TaskInfo, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, plyerr.New(plyerr.Invalid, `ply new "Short title"`, "a task needs a title")
	}
	var info *TaskInfo
	err := p.DB.Write(ctx, func(tx *store.Tx) error {
		t := store.Task{Title: title, Description: description, Owner: actor, Assignee: actor}
		if on != 0 {
			bt, err := tx.Task(on)
			if err != nil {
				return notFound(err)
			}
			if bt.Status != store.StatusOpen && bt.Status != store.StatusReady {
				return plyerr.New(plyerr.State, "ply new \"...\" --on project", "task #%d is %s; a new task can only build on an open task", on, bt.Status)
			}
			t.BaseTask = on
			t.BaseVersion = bt.BaseVersion
			if t.BaseSnapshot, err = tx.TaskSnapshot(bt); err != nil {
				return err
			}
		} else {
			head, err := tx.Head()
			if err != nil {
				return err
			}
			t.BaseVersion, t.BaseSnapshot = head.Num, head.Snapshot
		}
		if err := tx.PutTask(&t); err != nil {
			return err
		}
		var err error
		info, err = taskInfo(tx, t, true)
		return err
	})
	return info, err
}

// ShareRequest carries a task's labeled changes from a workspace.
type ShareRequest struct {
	Task int64 `json:"task"`
	// ShareSeq is the share this builds on; it must match the server's.
	ShareSeq     int64            `json:"share_seq"`
	BaseTask     int64            `json:"base_task"`
	BaseVersion  int64            `json:"base_version"`
	BaseSnapshot store.Hash       `json:"base_snapshot"`
	Changes      []store.Change   `json:"changes"`
	Conflicts    []store.Conflict `json:"conflicts"`
}

// StaleError is returned when someone else shared newer changes.
func staleError(t store.Task) error {
	who := t.SharedBy
	if who == "" {
		who = "someone"
	}
	return plyerr.New(plyerr.Stale, "ply refresh", "%s shared newer changes to task #%d", who, t.ID)
}

// Share stores a task's labeled changes. Their content must already be
// stored (see the server's share endpoint, which stores content and calls
// this in the same transaction).
func (p *Project) Share(tx *store.Tx, actor string, req ShareRequest) (*TaskInfo, error) {
	t, err := tx.Task(req.Task)
	if err != nil {
		return nil, notFound(err)
	}
	switch t.Status {
	case store.StatusApplied:
		return nil, plyerr.New(plyerr.State, "", "task #%d is already applied; start a new task for more work", t.ID)
	case store.StatusClosed:
		return nil, plyerr.New(plyerr.State, fmt.Sprintf("ply reopen %d", t.ID), "task #%d is closed", t.ID)
	}
	if req.ShareSeq != t.ShareSeq {
		return nil, staleError(t)
	}
	// Every snapshot must be complete.
	snaps := []store.Hash{req.BaseSnapshot}
	for _, c := range req.Changes {
		snaps = append(snaps, c.Snapshot)
	}
	for _, h := range snaps {
		if err := checkSnapshot(tx, h); err != nil {
			return nil, err
		}
	}
	if req.BaseTask != 0 && req.BaseTask != t.BaseTask {
		if _, err := tx.Task(req.BaseTask); err != nil {
			return nil, notFound(err)
		}
	}
	// Replace the change list.
	old, err := tx.Changes(t.ID)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, c := range req.Changes {
		keep[c.ID] = true
	}
	for _, c := range old {
		if !keep[c.ID] {
			if err := tx.DeleteChange(c.ID); err != nil {
				return nil, err
			}
		}
	}
	// Renumber in two passes so (task, seq) stays unique while reordering.
	for i, c := range req.Changes {
		c.Seq = -(i + 1)
		c.Task = t.ID
		c.Shared = true
		if c.Author == "" {
			c.Author = actor
		}
		if err := tx.PutChange(&c); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec("UPDATE change SET seq = -seq WHERE task = ? AND seq < 0", t.ID); err != nil {
		return nil, err
	}
	t.BaseTask, t.BaseVersion, t.BaseSnapshot = req.BaseTask, req.BaseVersion, req.BaseSnapshot
	t.Conflicts = req.Conflicts
	t.ShareSeq++
	t.SharedBy = actor
	t.UpdatedAt = store.Now()
	if err := tx.PutTask(&t); err != nil {
		return nil, err
	}
	return taskInfo(tx, t, true)
}

// checkSnapshot verifies a snapshot and all its files' content are stored.
func checkSnapshot(tx *store.Tx, h store.Hash) error {
	s, err := tx.Snapshot(h)
	if err != nil {
		return plyerr.New(plyerr.Invalid, "ply share", "snapshot %s is missing: %v", h.Short(), err)
	}
	for _, e := range s.Entries {
		ok, err := tx.HasBlob(e.Hash)
		if err != nil {
			return err
		}
		if !ok {
			return plyerr.New(plyerr.Invalid, "ply share", "content for %s is missing", e.Path)
		}
	}
	return nil
}

// Approve applies a task, and any open tasks it's built on, to the project.
// Newer project work is combined automatically; a real conflict refuses.
func (p *Project) Approve(ctx context.Context, actor string, id int64) ([]store.Version, error) {
	var applied []store.Version
	err := p.DB.Write(ctx, func(tx *store.Tx) error {
		t, err := tx.Task(id)
		if err != nil {
			return notFound(err)
		}
		if who, err := tx.Setting(SettingApprove, ApproveAnyone); err != nil {
			return err
		} else if who == ApproveOthers && t.Owner == actor {
			return plyerr.New(plyerr.Forbidden, "", "in this project someone other than the task's owner approves it")
		}
		// Collect the chain from the bottom up.
		chain := []store.Task{t}
		for b := t.BaseTask; b != 0; {
			bt, err := tx.Task(b)
			if err != nil {
				return err
			}
			if bt.Status == store.StatusApplied {
				break
			}
			if bt.Status == store.StatusClosed {
				return plyerr.New(plyerr.State, "ply refresh", "task #%d is built on task #%d, which was closed", chain[0].ID, bt.ID)
			}
			chain = append(chain, bt)
			b = bt.BaseTask
		}
		slices.Reverse(chain)
		for _, t := range chain {
			v, err := apply(tx, actor, t)
			if err != nil {
				return err
			}
			applied = append(applied, v)
		}
		return nil
	})
	return applied, err
}

func apply(tx *store.Tx, actor string, t store.Task) (store.Version, error) {
	var v store.Version
	switch t.Status {
	case store.StatusApplied:
		return v, plyerr.New(plyerr.State, "", "task #%d is already applied", t.ID)
	case store.StatusClosed:
		return v, plyerr.New(plyerr.State, fmt.Sprintf("ply reopen %d", t.ID), "task #%d is closed", t.ID)
	}
	if len(t.Conflicts) > 0 {
		e := plyerr.New(plyerr.Conflict, "ply conflicts", "task #%d has unresolved conflicts; resolve them, label and share", t.ID)
		for _, c := range t.Conflicts {
			e.Paths = append(e.Paths, c.Path)
		}
		return v, e
	}
	head, err := tx.Head()
	if err != nil {
		return v, err
	}
	latest, err := tx.TaskSnapshot(t)
	if err != nil {
		return v, err
	}
	result := latest
	if head.Snapshot != t.BaseSnapshot {
		base, err := tx.Snapshot(t.BaseSnapshot)
		if err != nil {
			return v, err
		}
		ours, err := tx.Snapshot(latest)
		if err != nil {
			return v, err
		}
		theirs, err := tx.Snapshot(head.Snapshot)
		if err != nil {
			return v, err
		}
		res, err := merge.Trees(tx, base, ours, theirs, merge.Labels{Ours: fmt.Sprintf("task #%d", t.ID), Theirs: fmt.Sprintf("project #%d", head.Num)})
		if err != nil {
			return v, err
		}
		if len(res.Conflicts) > 0 {
			e := plyerr.New(plyerr.Conflict, "ply refresh", "task #%d conflicts with project #%d", t.ID, head.Num)
			for _, c := range res.Conflicts {
				e.Paths = append(e.Paths, c.Path)
			}
			return v, e
		}
		if result, err = tx.PutSnapshot(res.Snapshot); err != nil {
			return v, err
		}
	}
	v = store.Version{Num: head.Num + 1, Task: t.ID, Snapshot: result, AppliedBy: actor}
	if err := tx.PutVersion(&v); err != nil {
		return v, err
	}
	t.Status = store.StatusApplied
	t.UpdatedAt = store.Now()
	if err := tx.PutTask(&t); err != nil {
		return v, err
	}
	// Tasks built on this one are now built on the project.
	on, err := tx.TasksOn(t.ID)
	if err != nil {
		return v, err
	}
	for _, o := range on {
		if o.Status == store.StatusApplied {
			continue
		}
		o.BaseTask = 0
		o.BaseVersion = v.Num
		if err := tx.PutTask(&o); err != nil {
			return v, err
		}
	}
	return v, nil
}

// SetStatus moves a task to ready, open (reopen) or closed.
func (p *Project) SetStatus(ctx context.Context, actor string, id int64, status string) (*TaskInfo, error) {
	var info *TaskInfo
	err := p.DB.Write(ctx, func(tx *store.Tx) error {
		t, err := tx.Task(id)
		if err != nil {
			return notFound(err)
		}
		if t.Status == store.StatusApplied {
			return plyerr.New(plyerr.State, fmt.Sprintf("ply revert %d", id), "task #%d is already applied", id)
		}
		switch status {
		case store.StatusReady, store.StatusClosed:
		case store.StatusOpen:
			if t.Status != store.StatusClosed {
				return plyerr.New(plyerr.State, "", "task #%d isn't closed", id)
			}
		default:
			return plyerr.New(plyerr.Invalid, "", "unknown status %q", status)
		}
		t.Status = status
		t.UpdatedAt = store.Now()
		if err := tx.PutTask(&t); err != nil {
			return err
		}
		info, err = taskInfo(tx, t, true)
		return err
	})
	return info, err
}

// AddNote adds a note to a task.
func (p *Project) AddNote(ctx context.Context, actor string, id int64, body string) (*store.Note, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, plyerr.New(plyerr.Invalid, "", "a note can't be empty")
	}
	n := &store.Note{Task: id, Author: actor, Body: body}
	err := p.DB.Write(ctx, func(tx *store.Tx) error {
		if _, err := tx.Task(id); err != nil {
			return notFound(err)
		}
		return tx.PutNote(n)
	})
	return n, err
}

// Log returns the project's history, newest first.
func (p *Project) Log(ctx context.Context) ([]store.Version, error) {
	var vs []store.Version
	err := p.DB.Read(ctx, func(tx *store.Tx) (err error) { vs, err = tx.Versions(); return })
	return vs, err
}

// Revert creates a task that undoes an applied task, as one change against
// the current project.
func (p *Project) Revert(ctx context.Context, actor string, id int64) (*TaskInfo, error) {
	var info *TaskInfo
	err := p.DB.Write(ctx, func(tx *store.Tx) error {
		t, err := tx.Task(id)
		if err != nil {
			return notFound(err)
		}
		v, err := tx.VersionOfTask(id)
		if err != nil {
			return plyerr.New(plyerr.State, "", "task #%d hasn't been applied, so there's nothing to revert; close it instead", id)
		}
		prev, err := tx.Version(v.Num - 1)
		if err != nil {
			return err
		}
		head, err := tx.Head()
		if err != nil {
			return err
		}
		load := func(h store.Hash) *store.Snapshot {
			s, e := tx.Snapshot(h)
			if e != nil {
				err = e
			}
			return s
		}
		base, ours, theirs := load(v.Snapshot), load(head.Snapshot), load(prev.Snapshot)
		if err != nil {
			return err
		}
		res, err := merge.Trees(tx, base, ours, theirs, merge.Labels{Ours: fmt.Sprintf("project #%d", head.Num), Theirs: fmt.Sprintf("before task #%d", id)})
		if err != nil {
			return err
		}
		snap, err := tx.PutSnapshot(res.Snapshot)
		if err != nil {
			return err
		}
		r := store.Task{
			Title: fmt.Sprintf("Revert %q", t.Title), Owner: actor, Assignee: actor,
			Description: fmt.Sprintf("Undoes task #%d (project #%d).", id, v.Num),
			BaseVersion: head.Num, BaseSnapshot: head.Snapshot, Reverts: id,
			Conflicts: res.Conflicts, ShareSeq: 1, SharedBy: actor,
		}
		if err := tx.PutTask(&r); err != nil {
			return err
		}
		c := &store.Change{ID: NewChangeID(), Task: r.ID, Seq: 1, Label: fmt.Sprintf("Revert task #%d", id), Snapshot: snap, Author: actor, Shared: true}
		if err := tx.PutChange(c); err != nil {
			return err
		}
		info, err = taskInfo(tx, r, true)
		return err
	})
	return info, err
}

// Project settings.
const (
	// SettingApprove says who can approve a task: ApproveAnyone (the
	// default), or ApproveOthers (anyone but the task's owner).
	SettingApprove = "approve"
	ApproveAnyone  = "anyone"
	ApproveOthers  = "others"
)

var settings = map[string][]string{SettingApprove: {ApproveAnyone, ApproveOthers}}

// Settings returns the project's settings, with defaults filled in.
func (p *Project) Settings(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	err := p.DB.Read(ctx, func(tx *store.Tx) error {
		for k, vals := range settings {
			v, err := tx.Setting(k, vals[0])
			if err != nil {
				return err
			}
			out[k] = v
		}
		return nil
	})
	return out, err
}

// SetSetting changes a project setting.
func (p *Project) SetSetting(ctx context.Context, key, value string) (map[string]string, error) {
	vals, ok := settings[key]
	if !ok {
		return nil, plyerr.New(plyerr.Invalid, "ply setting", "there's no setting %q", key)
	}
	if !slices.Contains(vals, value) {
		return nil, plyerr.New(plyerr.Invalid, "", "%s can be %s", key, strings.Join(vals, " or "))
	}
	if err := p.DB.Write(ctx, func(tx *store.Tx) error { return tx.SetSetting(key, value) }); err != nil {
		return nil, err
	}
	return p.Settings(ctx)
}
