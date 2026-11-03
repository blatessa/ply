package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/blatessa/ply/internal/client"
	"github.com/blatessa/ply/internal/plyerr"
	"github.com/blatessa/ply/internal/project"
	"github.com/blatessa/ply/internal/store"
	"github.com/blatessa/ply/internal/workspace"
)

// ShipResult says what `ply ship` did.
type ShipResult struct {
	Task  project.TaskInfo `json:"task"`
	Files []store.Delta    `json:"files"`
	// Applied is the new project version, if the task was approved.
	Applied *store.Version `json:"applied,omitempty"`
	// NotApproved says why it was left ready for review instead.
	NotApproved string `json:"not_approved,omitempty"`
	// LeftInTask is set when the folder shows a task: the shipped edits are
	// still its unlabeled work until it's refreshed.
	LeftInTask int64 `json:"left_in_task,omitempty"`
}

// Ship wraps unlabeled work (all of it, or just paths) into its own small
// task built on the project, shares it, and applies it if the project lets
// people approve their own tasks; otherwise it's left ready for review.
// Files in the folder aren't changed.
func (w *WS) Ship(ctx context.Context, title string, paths []string) (*ShipResult, error) {
	if strings.TrimSpace(title) == "" {
		return nil, plyerr.New(plyerr.Invalid, `ply ship "What changed"`, "a shipped change needs a title")
	}
	if _, _, err := w.CatchUp(ctx); err != nil {
		return nil, err
	}
	st, err := w.Client.State(ctx, false)
	if err != nil {
		return nil, err
	}
	head := st.Head
	if err := w.Client.Fetch(ctx, w.DB, []store.Hash{head.Snapshot}, w.haveSnapshots(ctx)); err != nil {
		return nil, err
	}
	res := &ShipResult{}
	var from, picked store.Hash
	err = w.DB.Write(ctx, func(tx *store.Tx) error {
		cur, _, err := w.catchUp(tx)
		if err != nil {
			return err
		}
		if from, err = w.labeled(tx); err != nil {
			return err
		}
		ls, err := tx.Snapshot(from)
		if err != nil {
			return err
		}
		res.Files = pick(store.Diff(ls, cur), paths)
		if len(res.Files) == 0 {
			if len(paths) > 0 {
				return plyerr.New(plyerr.Invalid, "ply status", "none of those files have unlabeled work")
			}
			return plyerr.New(plyerr.Invalid, "", "nothing to ship: there's no unlabeled work here")
		}
		picked, err = tx.PutSnapshot(applyDeltas(ls, res.Files))
		return err
	})
	if err != nil {
		return nil, err
	}
	// The edits, moved onto the current project.
	onto := func(base store.Hash) (store.Hash, error) {
		var out store.Hash
		err := w.DB.Write(ctx, func(tx *store.Tx) error {
			m, err := w.merge3(tx, from, picked, base, "your edits", "project")
			if err != nil {
				return err
			}
			if len(m.Conflicts) > 0 {
				e := plyerr.New(plyerr.Conflict, `ply label "..."`, "these edits overlap with this task's changes or newer project work, so they can't ship on their own; label them here instead with")
				for _, c := range m.Conflicts {
					e.Paths = append(e.Paths, c.Path)
				}
				return e
			}
			out, err = tx.PutSnapshot(m.Snapshot)
			return err
		})
		return out, err
	}
	result, err := onto(head.Snapshot)
	if err != nil {
		return nil, err
	}
	info, err := w.Client.CreateTask(ctx, title, "", 0)
	if err != nil {
		return nil, err
	}
	if info.BaseSnapshot != head.Snapshot {
		// The project moved on between the two calls.
		if err := w.Client.Fetch(ctx, w.DB, []store.Hash{info.BaseSnapshot}, w.haveSnapshots(ctx)); err == nil {
			result, err = onto(info.BaseSnapshot)
		}
		if err != nil {
			w.Client.SetStatus(ctx, info.ID, "close")
			return nil, err
		}
	}
	change := store.Change{ID: project.NewChangeID(), Task: info.ID, Seq: 1, Label: title, Snapshot: result, Author: w.User}
	err = w.DB.Write(ctx, func(tx *store.Tx) error {
		if err := adoptTask(tx, info); err != nil {
			return err
		}
		return tx.PutChange(&change)
	})
	if err != nil {
		return nil, err
	}
	shared, err := w.Client.Share(ctx, w.DB, project.ShareRequest{Task: info.ID, ShareSeq: info.ShareSeq,
		BaseVersion: info.BaseVersion, BaseSnapshot: info.BaseSnapshot, Changes: []store.Change{change}})
	if err != nil {
		return nil, err
	}
	w.DB.Write(ctx, func(tx *store.Tx) error {
		t := shared.Task
		t.SyncedSnapshot = shared.Latest
		if err := tx.PutTask(&t); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE change SET shared = 1 WHERE task = ?", info.ID)
		return err
	})
	res.Task = *shared
	vs, err := w.Client.Approve(ctx, info.ID)
	switch {
	case plyerr.Is(err, plyerr.Forbidden):
		e, _ := plyerr.As(err)
		res.NotApproved = e.Msg
		if r, err := w.Client.SetStatus(ctx, info.ID, "ready"); err == nil {
			res.Task = *r
		}
		return res, nil
	case err != nil:
		return res, fmt.Errorf("shared task #%d but couldn't approve it: %w", info.ID, err)
	}
	if len(vs) > 0 {
		res.Applied = &vs[len(vs)-1]
		res.Task.Status = store.StatusApplied
	}
	if w.Task != 0 {
		res.LeftInTask = w.Task
	} else if res.Applied != nil && w.BaseSnapshot == info.BaseSnapshot {
		// The folder was up to date, so the shipped edits are now simply
		// part of the project it shows.
		w.DB.Write(ctx, func(tx *store.Tx) error { return w.setBase(tx, res.Applied.Num, res.Applied.Snapshot) })
	}
	return res, nil
}

// pick keeps the deltas under the given paths (files or folders); all of
// them if none are given.
func pick(ds []store.Delta, paths []string) []store.Delta {
	if len(paths) == 0 {
		return ds
	}
	var out []store.Delta
	for _, d := range ds {
		for _, p := range paths {
			p = strings.Trim(filepath.ToSlash(filepath.Clean(p)), "/")
			if p == "." || d.Path == p || strings.HasPrefix(d.Path, p+"/") {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// applyDeltas returns s with the deltas applied.
func applyDeltas(s *store.Snapshot, ds []store.Delta) *store.Snapshot {
	m := s.Map()
	for _, d := range ds {
		if d.Kind == store.Removed {
			delete(m, d.Path)
		} else {
			m[d.Path] = d.New
		}
	}
	out := &store.Snapshot{}
	for _, e := range m {
		out.Entries = append(out.Entries, e)
	}
	out.Sort()
	return out
}

// StartResult says what `ply start` did.
type StartResult struct {
	Project *workspace.Project `json:"project"`
	Task    project.TaskInfo   `json:"task"`
	Files   int                `json:"files"`
}

// StartTitle is the title of the first task `ply start` makes.
const StartTitle = "Initial files"

// Start creates a project on the server from the current folder: it makes
// the project, opens a first task with every file here (respecting
// .plyignore), labels and shares it, and turns the folder into the
// project's folder, showing that task. Approving the task gives the project
// its starting files.
func (e *Env) Start(ctx context.Context, url string) (*StartResult, error) {
	if w, err := workspace.FindWorkspace(e.Dir); err == nil {
		return nil, plyerr.New(plyerr.Invalid, "", "%s is already a Ply folder (for %s)", w.Root, w.Project)
	}
	p, err := e.Cfg.AddProject(url, e.Dir)
	if err != nil {
		return nil, plyerr.New(plyerr.Invalid, "", "%v", err)
	}
	if err := client.CreateProject(ctx, p.Server(), e.Cfg.User, p.Name); err != nil {
		// Carry on with a project that exists but is still empty (a start
		// that was interrupted); refuse one that has work.
		st, serr := client.New(p.URL, e.Cfg.User, p.Token).State(ctx, true)
		if serr != nil || st.Head.Num != 0 || len(st.Tasks) != 0 {
			return nil, plyerr.New(plyerr.Invalid, "ply join "+p.URL, "couldn't create %s (%v); to work on an existing project, use", p.Name, err)
		}
	}
	proj, err := e.open(p)
	if err != nil {
		return nil, err
	}
	defer proj.Close()
	if _, ok := proj.mainFolder(ctx); ok {
		return nil, plyerr.New(plyerr.Invalid, "", "%s already has a folder on this machine", p.Name)
	}
	info, err := proj.Client.CreateTask(ctx, StartTitle, "The project's starting files.", 0)
	if err != nil {
		return nil, err
	}
	res := &StartResult{Project: p}
	var w *WS
	err = proj.DB.Write(ctx, func(tx *store.Tx) error {
		if err := adoptTask(tx, info); err != nil {
			return err
		}
		var err error
		if w, err = proj.newFolder(tx, e.Dir, true, info.ID, 0, store.EmptySnapshot); err != nil {
			return err
		}
		if err := w.writeMarker(); err != nil {
			return err
		}
		snap, _, err := w.catchUp(tx)
		res.Files = len(snap.Entries)
		return err
	})
	if err != nil {
		os.Remove(filepath.Join(e.Dir, workspace.MarkerName))
		return nil, err
	}
	if err := e.Cfg.Save(); err != nil {
		return nil, err
	}
	if res.Files > 0 {
		if _, err := w.Label(ctx, StartTitle, false); err != nil {
			return nil, err
		}
		sr, err := w.Share(ctx)
		if err != nil {
			return nil, err
		}
		res.Task = sr.Task
	} else {
		res.Task = *info
	}
	return res, nil
}
