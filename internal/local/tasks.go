package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/blatessa/ply/internal/merge"
	"github.com/blatessa/ply/internal/plyerr"
	"github.com/blatessa/ply/internal/project"
	"github.com/blatessa/ply/internal/store"
	"github.com/blatessa/ply/internal/workspace"
)

func builtOn(t store.Task) string {
	if t.BaseTask != 0 {
		return fmt.Sprintf("task #%d", t.BaseTask)
	}
	return fmt.Sprintf("project #%d", t.BaseVersion)
}

// haveSnapshots lists snapshots this machine has, so fetches skip content
// they share: the project head and each workspace's task state.
func (p *Proj) haveSnapshots(ctx context.Context) []store.Hash {
	var out []store.Hash
	p.DB.Read(ctx, func(tx *store.Tx) error {
		rows, err := tx.Query(`WITH local(id) AS (SELECT task FROM workspace UNION SELECT DISTINCT task FROM save)
			SELECT base_snapshot FROM task WHERE id IN local
			UNION SELECT synced_snapshot FROM task WHERE synced_snapshot IS NOT NULL AND id IN local
			UNION SELECT base_snapshot FROM workspace WHERE base_snapshot IS NOT NULL`)
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
	// Task is the task the folder shows; its ID is 0 when the folder shows
	// the project itself (Project is then the version).
	Task    store.Task `json:"task"`
	Project int64      `json:"project,omitempty"`
	Path    string     `json:"path"`
	Folders []Folder   `json:"other_folders,omitempty"`
	// Stale lists files probably saved by an editor from before the last
	// switch.
	Stale     []StaleWrite     `json:"stale_writes,omitempty"`
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
	if st.Folders, err = w.otherFolders(ctx); err != nil {
		return nil, err
	}
	if st.Stale, err = w.StaleWrites(ctx); err != nil {
		return nil, err
	}
	if w.Task == 0 {
		return w.projectStatus(ctx, st, cur)
	}
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

// projectStatus is the status of a folder showing the project.
func (w *WS) projectStatus(ctx context.Context, st *Status, cur *store.Snapshot) (*Status, error) {
	st.Project, st.BuiltOn = w.BaseVersion, fmt.Sprintf("project #%d", w.BaseVersion)
	err := w.DB.Read(ctx, func(tx *store.Tx) error {
		base, err := tx.Snapshot(w.BaseSnapshot)
		st.Unlabeled = store.Diff(base, cur)
		return err
	})
	if err != nil {
		return nil, err
	}
	if s, err := w.Client.State(ctx, false); err != nil {
		st.Server = err.Error()
	} else if s.Head.Num != w.BaseVersion {
		st.Refresh = fmt.Sprintf("the project has moved on to #%d", s.Head.Num)
	}
	return st, nil
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
	if err := w.needTask(); err != nil {
		return nil, err
	}
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
		return recordAction(tx, w, kind, desc, before, after)
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
	if w.Task == 0 {
		res.From = w.what()
	}
	err = w.DB.Read(ctx, func(tx *store.Tx) error {
		latest, err := w.labeled(tx)
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
