package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/blatessa/ply/internal/client"
	"github.com/blatessa/ply/internal/merge"
	"github.com/blatessa/ply/internal/plyerr"
	"github.com/blatessa/ply/internal/project"
	"github.com/blatessa/ply/internal/store"
	"github.com/blatessa/ply/internal/workspace"
)

// Join connects this machine to a project. Workspaces will be created in dir.
func (e *Env) Join(ctx context.Context, url, dir string) (*workspace.Project, error) {
	p, err := e.Cfg.AddProject(url, dir)
	if err != nil {
		return nil, plyerr.New(plyerr.Invalid, "", "%v", err)
	}
	c := client.New(p.URL, e.Cfg.User, p.Token)
	if _, err := c.State(ctx, false); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return p, e.Cfg.Save()
}

// NewResult describes a created workspace.
type NewResult struct {
	Task    project.TaskInfo `json:"task"`
	Path    string           `json:"path"`
	BuiltOn string           `json:"built_on"`
	Warning string           `json:"warning,omitempty"`
}

// NewOptions configure a new task.
type NewOptions struct {
	Title       string
	Description string
	// On is "project", a task reference, or "" for the default: the current
	// workspace's task if in one, else the project.
	On          string
	CopyIgnored bool
	Path        string // where to put the workspace; default under the project folder
	Project     string
}

// New creates a task and its workspace.
func (e *Env) New(ctx context.Context, o NewOptions) (*NewResult, error) {
	cur, curErr := e.Workspace()
	var p *Proj
	if curErr == nil {
		p = cur.Proj
	} else {
		var err error
		if p, err = e.Project(o.Project); err != nil {
			return nil, err
		}
	}
	defer p.Close()

	var on int64
	var warning string
	switch {
	case o.On == "project":
	case o.On != "":
		id, err := project.ParseTask(o.On)
		if err != nil {
			return nil, err
		}
		on = id
	case cur != nil:
		on = cur.Task
	}
	if cur != nil && on == cur.Task {
		if n, _ := cur.unsharedCount(ctx); n > 0 {
			warning = fmt.Sprintf("task #%d has %d labeled changes that aren't shared; the new task builds on its last shared change (run `ply share` in that workspace, then `ply refresh` here)", cur.Task, n)
		}
	}
	info, err := p.Client.CreateTask(ctx, o.Title, o.Description, on)
	if err != nil {
		return nil, err
	}
	path := o.Path
	if path == "" {
		path = filepath.Join(p.Dir, Slug(info.Title))
		if _, err := os.Stat(path); err == nil {
			path = fmt.Sprintf("%s-%d", path, info.ID)
		}
	}
	var from string
	if o.CopyIgnored {
		if cur == nil {
			return nil, plyerr.New(plyerr.Invalid, "", "--copy-ignored copies from the current workspace; run it inside one")
		}
		from = cur.Root
	}
	if err := p.makeWorkspace(ctx, info, path, from); err != nil {
		return nil, err
	}
	res := &NewResult{Task: *info, Path: path, BuiltOn: builtOn(info.Task), Warning: warning}
	return res, nil
}

func builtOn(t store.Task) string {
	if t.BaseTask != 0 {
		return fmt.Sprintf("task #%d", t.BaseTask)
	}
	return fmt.Sprintf("project #%d", t.BaseVersion)
}

// Get makes a workspace for an existing task.
func (e *Env) Get(ctx context.Context, ref, path, projectName string) (*NewResult, error) {
	id, err := project.ParseTask(ref)
	if err != nil {
		return nil, err
	}
	p, err := e.Project(projectName)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	info, err := p.Client.Task(ctx, id)
	if err != nil {
		return nil, err
	}
	if path == "" {
		path = filepath.Join(p.Dir, Slug(info.Title))
		if _, err := os.Stat(path); err == nil {
			path = fmt.Sprintf("%s-%d", path, info.ID)
		}
	}
	if err := p.makeWorkspace(ctx, info, path, ""); err != nil {
		return nil, err
	}
	return &NewResult{Task: *info, Path: path, BuiltOn: builtOn(info.Task)}, nil
}

// makeWorkspace creates the folder for a task at its latest shared state.
func (p *Proj) makeWorkspace(ctx context.Context, info *project.TaskInfo, path, copyIgnoredFrom string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if entries, err := os.ReadDir(path); err == nil && len(entries) > 0 {
		return plyerr.New(plyerr.Invalid, "", "%s already exists and isn't empty", path)
	}
	var existing string
	p.DB.Read(ctx, func(tx *store.Tx) error {
		return tx.QueryRow("SELECT path FROM workspace WHERE task = ?", info.ID).Scan(&existing)
	})
	if existing != "" {
		return plyerr.New(plyerr.Invalid, "cd "+existing, "task #%d already has a workspace on this machine at %s", info.ID, existing)
	}
	if err := p.Client.Fetch(ctx, p.DB, []store.Hash{info.Latest, info.BaseSnapshot}, p.haveSnapshots(ctx)); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	ws := &WS{Proj: p, Root: path, Task: info.ID}
	err = p.DB.Write(ctx, func(tx *store.Tx) error {
		t := info.Task
		t.SyncedSnapshot = info.Latest
		if err := tx.PutTask(&t); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM change WHERE task = ?", t.ID); err != nil {
			return err
		}
		for _, c := range info.Changes {
			c.Shared = true
			if err := tx.PutChange(&c); err != nil {
				return err
			}
		}
		res, err := tx.Exec("INSERT INTO workspace (path, task, created_at) VALUES (?, ?, ?)", path, t.ID, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		if ws.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		snap, err := tx.Snapshot(info.Latest)
		if err != nil {
			return err
		}
		donors, err := workspace.LoadDonors(tx)
		if err != nil {
			return err
		}
		if err := workspace.Materialize(tx, ws.ID, path, snap, donors); err != nil {
			return err
		}
		mk := &workspace.Workspace{Root: path, Marker: workspace.Marker{Project: p.URL, Task: t.ID}}
		if err := mk.WriteMarker(); err != nil {
			return err
		}
		if copyIgnoredFrom != "" {
			if err := workspace.CopyIgnored(copyIgnoredFrom, path); err != nil {
				return fmt.Errorf("copy ignored files: %w", err)
			}
		}
		_, _, err = ws.catchUp(tx)
		return err
	})
	if err != nil {
		os.RemoveAll(path)
	}
	return err
}

// haveSnapshots lists snapshots this machine has, so fetches skip content
// they share: the project head and each workspace's task state.
func (p *Proj) haveSnapshots(ctx context.Context) []store.Hash {
	var out []store.Hash
	p.DB.Read(ctx, func(tx *store.Tx) error {
		rows, err := tx.Query(`SELECT base_snapshot FROM task WHERE id IN (SELECT task FROM workspace)
			UNION SELECT synced_snapshot FROM task WHERE synced_snapshot IS NOT NULL AND id IN (SELECT task FROM workspace)`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b []byte
			if rows.Scan(&b) == nil && len(b) == 32 {
				out = append(out, store.Hash(b))
			}
		}
		return nil
	})
	return out
}

// Status is a workspace's state.
type Status struct {
	Task      store.Task       `json:"task"`
	Path      string           `json:"path"`
	BuiltOn   string           `json:"built_on"`
	Unlabeled []store.Delta    `json:"unlabeled"`
	Unshared  int              `json:"unshared_changes"`
	Changes   int              `json:"changes"`
	Conflicts []store.Conflict `json:"conflicts"`
	// Refresh says why the task needs a refresh; empty if it doesn't.
	Refresh string `json:"refresh,omitempty"`
	// Server is set when the server couldn't be reached.
	Server       string `json:"server_error,omitempty"`
	ServerStatus string `json:"server_status,omitempty"`
	NewerShared  bool   `json:"newer_shared,omitempty"`
}

// Status reports the workspace's unlabeled work, conflicts, and whether it
// needs a refresh.
func (w *WS) Status(ctx context.Context) (*Status, error) {
	cur, _, err := w.CatchUp(ctx)
	if err != nil {
		return nil, err
	}
	st := &Status{Path: w.Root}
	err = w.DB.Read(ctx, func(tx *store.Tx) error {
		t, err := tx.Task(w.Task)
		if err != nil {
			return err
		}
		st.Task, st.Conflicts, st.BuiltOn = t, t.Conflicts, builtOn(t)
		latest, err := tx.TaskSnapshot(t)
		if err != nil {
			return err
		}
		ls, err := tx.Snapshot(latest)
		if err != nil {
			return err
		}
		st.Unlabeled = store.Diff(ls, cur)
		changes, err := tx.Changes(t.ID)
		for _, c := range changes {
			if !c.Shared {
				st.Unshared++
			}
		}
		st.Changes = len(changes)
		return err
	})
	if err != nil {
		return nil, err
	}
	if info, err := w.Client.Task(ctx, w.Task); err != nil {
		st.Server = err.Error()
	} else {
		st.ServerStatus = info.Status
		st.NewerShared = info.ShareSeq > st.Task.ShareSeq
		st.Refresh = w.refreshReason(ctx, st.Task, info)
	}
	return st, nil
}

// refreshReason compares the local task against the server's view.
func (w *WS) refreshReason(ctx context.Context, local store.Task, remote *project.TaskInfo) string {
	if remote.ShareSeq > local.ShareSeq {
		return fmt.Sprintf("%s shared newer changes to this task", remote.SharedBy)
	}
	if remote.Status == store.StatusApplied {
		return ""
	}
	up, err := w.Client.Upstream(ctx, w.Task)
	if err != nil {
		return ""
	}
	if up.Snapshot == local.BaseSnapshot {
		return ""
	}
	if up.Task != local.BaseTask && local.BaseTask != 0 {
		return fmt.Sprintf("task #%d it's built on is no longer open", local.BaseTask)
	}
	if up.Task != 0 {
		return fmt.Sprintf("%s it's built on has new changes", up.Label)
	}
	return fmt.Sprintf("the project has moved on to #%d", up.Version)
}

func (w *WS) unsharedCount(ctx context.Context) (int, error) {
	var n int
	err := w.DB.Read(ctx, func(tx *store.Tx) error {
		return tx.QueryRow("SELECT count(*) FROM change WHERE task = ? AND shared = 0", w.Task).Scan(&n)
	})
	return n, err
}

// Label records the work since the last label as a change. With edit, the
// work is folded into the latest change instead (and label, if non-empty,
// renames it).
func (w *WS) Label(ctx context.Context, label string, edit bool) (*store.Change, error) {
	var out store.Change
	err := w.DB.Write(ctx, func(tx *store.Tx) error {
		_, cur, err := w.catchUp(tx)
		if err != nil {
			return err
		}
		t, err := tx.Task(w.Task)
		if err != nil {
			return err
		}
		if t.Status == store.StatusApplied {
			return plyerr.New(plyerr.State, `ply new "..."`, "task #%d is already applied; start a new task for more work", t.ID)
		}
		before, err := captureState(tx, t)
		if err != nil {
			return err
		}
		latest, hasLatest, err := tx.LatestChange(t.ID)
		if err != nil {
			return err
		}
		labeled := t.BaseSnapshot
		if hasLatest {
			labeled = latest.Snapshot
		}
		if edit {
			if !hasLatest {
				return plyerr.New(plyerr.Invalid, `ply label "..."`, "there's no change to edit yet")
			}
			if cur == labeled && (label == "" || label == latest.Label) {
				return plyerr.New(plyerr.Invalid, "", "nothing new to fold into %q", latest.Label)
			}
			latest.Snapshot = cur
			if label != "" {
				latest.Label = label
			}
			latest.Shared = false
			latest.UpdatedAt = store.Now()
			out = latest
		} else {
			if label == "" {
				return plyerr.New(plyerr.Invalid, `ply label "What changed"`, "a change needs a label")
			}
			if cur == labeled {
				return plyerr.New(plyerr.Invalid, "", "nothing to label: no changes since the last label")
			}
			out = store.Change{ID: project.NewChangeID(), Task: t.ID, Seq: latest.Seq + 1, Label: label,
				Snapshot: cur, Author: w.User}
		}
		if err := tx.PutChange(&out); err != nil {
			return err
		}
		after, err := captureState(tx, t)
		if err != nil {
			return err
		}
		kind, desc := "label", fmt.Sprintf("label %q", out.Label)
		if edit {
			kind, desc = "label-edit", fmt.Sprintf("fold work into %q", out.Label)
		}
		return recordAction(tx, w.ID, kind, desc, before, after)
	})
	return &out, err
}

// DiffResult is a diff between two snapshots.
type DiffResult struct {
	From  string        `json:"from"`
	To    string        `json:"to"`
	Files []store.Delta `json:"files"`
	Text  string        `json:"diff"`
}

// Diff shows the workspace's unlabeled work.
func (w *WS) Diff(ctx context.Context) (*DiffResult, error) {
	_, cur, err := w.CatchUp(ctx)
	if err != nil {
		return nil, err
	}
	res := &DiffResult{From: "last label", To: "workspace"}
	err = w.DB.Read(ctx, func(tx *store.Tx) error {
		t, err := tx.Task(w.Task)
		if err != nil {
			return err
		}
		latest, err := tx.TaskSnapshot(t)
		if err != nil {
			return err
		}
		return diffSnapshots(tx, latest, cur, res)
	})
	return res, err
}

// TaskDiff shows a task's changes as reviewers see them: from what it's
// built on to its latest change.
func (p *Proj) TaskDiff(ctx context.Context, id int64) (*DiffResult, error) {
	info, err := p.Client.Task(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := p.Client.Fetch(ctx, p.DB, []store.Hash{info.BaseSnapshot, info.Latest}, p.haveSnapshots(ctx)); err != nil {
		return nil, err
	}
	res := &DiffResult{From: builtOn(info.Task), To: fmt.Sprintf("task #%d", id)}
	err = p.DB.Read(ctx, func(tx *store.Tx) error { return diffSnapshots(tx, info.BaseSnapshot, info.Latest, res) })
	return res, err
}

func diffSnapshots(tx *store.Tx, from, to store.Hash, res *DiffResult) error {
	a, err := tx.Snapshot(from)
	if err != nil {
		return err
	}
	b, err := tx.Snapshot(to)
	if err != nil {
		return err
	}
	res.Files = store.Diff(a, b)
	text, err := unifiedDeltas(tx, res.Files)
	res.Text = text
	return err
}

func unifiedDeltas(tx *store.Tx, deltas []store.Delta) (string, error) {
	var out []byte
	for _, d := range deltas {
		var ab, bb []byte
		var err error
		if d.Kind != store.Added {
			if ab, err = tx.ReadBlob(d.Old.Hash); err != nil {
				return "", err
			}
		}
		if d.Kind != store.Removed {
			if bb, err = tx.ReadBlob(d.New.Hash); err != nil {
				return "", err
			}
		}
		aName, bName := "a/"+d.Path, "b/"+d.Path
		if d.Kind == store.Added {
			aName = "/dev/null"
		}
		if d.Kind == store.Removed {
			bName = "/dev/null"
		}
		if merge.IsBinary(ab) || merge.IsBinary(bb) {
			out = fmt.Appendf(out, "Binary file %s %s\n", d.Path, d.Kind)
			continue
		}
		if u := merge.Unified(aName, bName, ab, bb, 3); u != "" {
			out = append(out, u...)
		} else if d.Old.Mode != d.New.Mode {
			out = fmt.Appendf(out, "%s: mode changed\n", d.Path)
		}
	}
	return string(out), nil
}

// Remove deletes the workspace folder and its saves. It refuses if there is
// work that would be lost, unless force is set.
func (w *WS) Remove(ctx context.Context, force bool) error {
	if !force {
		st, err := w.Status(ctx)
		if err != nil {
			return err
		}
		if len(st.Unlabeled) > 0 {
			return plyerr.New(plyerr.State, "ply remove --force", "the workspace has unlabeled work that would be lost; label and share it first, or")
		}
		if st.Unshared > 0 && st.ServerStatus != store.StatusApplied {
			return plyerr.New(plyerr.State, "ply share", "the task has %d labeled changes that aren't shared", st.Unshared)
		}
	}
	err := w.DB.Write(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec("DELETE FROM workspace WHERE id = ?", w.ID); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM change WHERE task = ?", w.Task)
		return err
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(w.Root)
}

// ExportOptions say what to export.
type ExportOptions struct {
	Dir     string
	At      string // applied task
	Task    string // open task
	NoInfo  bool
	Project string
}

// Export writes the project's files as a plain, untracked folder.
func (e *Env) Export(ctx context.Context, o ExportOptions) (string, error) {
	p, err := e.Project(o.Project)
	if err != nil {
		return "", err
	}
	defer p.Close()
	var snap store.Hash
	var desc string
	switch {
	case o.At != "" && o.Task != "":
		return "", plyerr.New(plyerr.Invalid, "", "export takes --at or --task, not both")
	case o.At != "":
		id, err := project.ParseTask(o.At)
		if err != nil {
			return "", err
		}
		info, err := p.Client.Task(ctx, id)
		if err != nil {
			return "", err
		}
		if info.Version == 0 {
			return "", plyerr.New(plyerr.State, "ply export <folder> --task "+o.At, "task #%d hasn't been applied", id)
		}
		v, err := p.Client.Version(ctx, info.Version)
		if err != nil {
			return "", err
		}
		snap, desc = v.Snapshot, fmt.Sprintf("project #%d, after task #%d %q", v.Num, id, info.Title)
	case o.Task != "":
		id, err := project.ParseTask(o.Task)
		if err != nil {
			return "", err
		}
		info, err := p.Client.Task(ctx, id)
		if err != nil {
			return "", err
		}
		snap = info.Latest
		desc = fmt.Sprintf("task #%d %q (%s), %d changes, share %d, built on %s", id, info.Title, info.Status, len(info.Changes), info.ShareSeq, builtOn(info.Task))
	default:
		st, err := p.Client.State(ctx, false)
		if err != nil {
			return "", err
		}
		snap, desc = st.Head.Snapshot, fmt.Sprintf("project #%d", st.Head.Num)
		if st.Head.Task != 0 {
			desc += fmt.Sprintf(", last applied task #%d", st.Head.Task)
		}
	}
	if entries, err := os.ReadDir(o.Dir); err == nil && len(entries) > 0 {
		return "", plyerr.New(plyerr.Invalid, "", "%s already exists and isn't empty", o.Dir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := p.Client.Fetch(ctx, p.DB, []store.Hash{snap}, p.haveSnapshots(ctx)); err != nil {
		return "", err
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return "", err
	}
	err = p.DB.Write(ctx, func(tx *store.Tx) error {
		s, err := tx.Snapshot(snap)
		if err != nil {
			return err
		}
		donors, err := workspace.LoadDonors(tx)
		if err != nil {
			return err
		}
		return workspace.Materialize(tx, 0, o.Dir, s, donors)
	})
	if err != nil {
		return "", err
	}
	if !o.NoInfo {
		info := project.ExportInfo(p.Name, desc, time.Now())
		if err := os.WriteFile(filepath.Join(o.Dir, ".plyexport"), []byte(info), 0o644); err != nil {
			return "", err
		}
	}
	return desc, nil
}
