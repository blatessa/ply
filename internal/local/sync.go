package local

import (
	"context"
	"fmt"

	"github.com/blatessa/ply/internal/merge"
	"github.com/blatessa/ply/internal/plyerr"
	"github.com/blatessa/ply/internal/project"
	"github.com/blatessa/ply/internal/store"
	"github.com/blatessa/ply/internal/workspace"
)

// ShareResult says what was shared.
type ShareResult struct {
	Task      project.TaskInfo `json:"task"`
	Shared    int              `json:"changes_shared"`
	Unlabeled int              `json:"unlabeled_files"`
}

// Share sends the task's labeled changes to the server. It refuses if
// someone else shared newer changes since this workspace last synced.
func (w *WS) Share(ctx context.Context) (*ShareResult, error) {
	cur, _, err := w.CatchUp(ctx)
	if err != nil {
		return nil, err
	}
	var req project.ShareRequest
	var unlabeled, unshared, total int
	err = w.DB.Read(ctx, func(tx *store.Tx) error {
		t, err := tx.Task(w.Task)
		if err != nil {
			return err
		}
		changes, err := tx.Changes(t.ID)
		if err != nil {
			return err
		}
		latest, err := tx.TaskSnapshot(t)
		if err != nil {
			return err
		}
		ls, err := tx.Snapshot(latest)
		if err != nil {
			return err
		}
		unlabeled = len(store.Diff(ls, cur))
		total = len(changes)
		for _, c := range changes {
			if !c.Shared {
				unshared++
			}
		}
		conflicts, err := conflictsIn(tx, t.Conflicts, ls)
		if err != nil {
			return err
		}
		req = project.ShareRequest{Task: t.ID, ShareSeq: t.ShareSeq, BaseTask: t.BaseTask,
			BaseVersion: t.BaseVersion, BaseSnapshot: t.BaseSnapshot, Changes: changes, Conflicts: conflicts}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if unshared == 0 {
		if unlabeled > 0 {
			return nil, plyerr.New(plyerr.Invalid, `ply label "What changed"`, "nothing to share: %d files have changes but none are labeled", unlabeled)
		}
		if total == 0 {
			return nil, plyerr.New(plyerr.Invalid, `ply label "What changed"`, "nothing to share yet: edit files, then label them")
		}
		return nil, plyerr.New(plyerr.Invalid, "", "nothing to share: every labeled change is already shared")
	}
	info, err := w.Client.Share(ctx, w.DB, req)
	if err != nil {
		return nil, err
	}
	err = w.DB.Write(ctx, func(tx *store.Tx) error {
		t, err := tx.Task(w.Task)
		if err != nil {
			return err
		}
		t.ShareSeq, t.SharedBy, t.Status, t.SyncedSnapshot = info.ShareSeq, info.SharedBy, info.Status, info.Latest
		if err := tx.PutTask(&t); err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE change SET shared = 1 WHERE task = ?", w.Task)
		return err
	})
	return &ShareResult{Task: *info, Shared: unshared, Unlabeled: unlabeled}, err
}

// conflictsIn returns the tracked conflicts still present in a snapshot.
func conflictsIn(tx *store.Tx, cs []store.Conflict, s *store.Snapshot) ([]store.Conflict, error) {
	var out []store.Conflict
	for _, c := range cs {
		if c.Kind == merge.BinaryConflict {
			_, o := s.Get(c.Ours)
			_, t := s.Get(c.Theirs)
			if o && t {
				out = append(out, c)
			}
			continue
		}
		e, ok := s.Get(c.Path)
		if !ok {
			continue
		}
		b, err := tx.ReadBlob(e.Hash)
		if err != nil {
			return nil, err
		}
		if merge.HasMarkers(b) {
			out = append(out, c)
		}
	}
	return out, nil
}

// RefreshResult says what a refresh did.
type RefreshResult struct {
	UpToDate  bool             `json:"up_to_date"`
	Steps     []string         `json:"steps"`
	BuiltOn   string           `json:"built_on"`
	Conflicts []store.Conflict `json:"conflicts"`
	Notices   []string         `json:"notices,omitempty"`
}

// Refresh brings the workspace up to date: first with changes others shared
// to this task, then with what the task is built on. Labeled and unlabeled
// work both come along; overlapping edits become conflicts.
func (w *WS) Refresh(ctx context.Context) (*RefreshResult, error) {
	if _, _, err := w.CatchUp(ctx); err != nil {
		return nil, err
	}
	remote, err := w.Client.Task(ctx, w.Task)
	if err != nil {
		return nil, err
	}
	up, err := w.Client.Upstream(ctx, w.Task)
	if err != nil {
		return nil, err
	}
	if err := w.Client.Fetch(ctx, w.DB, []store.Hash{remote.Latest, remote.BaseSnapshot, up.Snapshot}, w.haveSnapshots(ctx)); err != nil {
		return nil, err
	}
	res := &RefreshResult{}
	err = w.DB.Write(ctx, func(tx *store.Tx) error {
		curSnap, cur, err := w.catchUp(tx)
		if err != nil {
			return err
		}
		t, err := tx.Task(w.Task)
		if err != nil {
			return err
		}
		before, err := captureState(tx, t)
		if err != nil {
			return err
		}
		before = withFiles(before, cur)
		files := cur
		var conflicts []store.Conflict

		// 1. Changes someone else shared to this task.
		if remote.ShareSeq > t.ShareSeq {
			labeled, err := tx.TaskSnapshot(t)
			if err != nil {
				return err
			}
			local, err := tx.Changes(t.ID)
			if err != nil {
				return err
			}
			var mine []store.Change
			for _, c := range local {
				if !c.Shared {
					mine = append(mine, c)
				}
			}
			// Adopt the server's changes and base, then put ours on top.
			if _, err := tx.Exec("DELETE FROM change WHERE task = ?", t.ID); err != nil {
				return err
			}
			seq := 0
			for _, c := range remote.Changes {
				seq++
				c.Seq, c.Shared = seq, true
				if err := tx.PutChange(&c); err != nil {
					return err
				}
			}
			newLabeled := remote.Latest
			if len(mine) > 0 {
				m, err := w.merge3(tx, t.SyncedSnapshot, labeled, remote.Latest, "yours", "shared by "+remote.SharedBy)
				if err != nil {
					return err
				}
				conflicts = append(conflicts, m.Conflicts...)
				res.Notices = append(res.Notices, m.Notices...)
				if newLabeled, err = tx.PutSnapshot(m.Snapshot); err != nil {
					return err
				}
				for _, c := range mine {
					seq++
					c.Seq = seq
					if err := tx.PutChange(&c); err != nil {
						return err
					}
				}
				seq++
				rc := store.Change{ID: project.NewChangeID(), Task: t.ID, Seq: seq, Snapshot: newLabeled, Author: w.User,
					Label: fmt.Sprintf("Refreshed with changes shared by %s", remote.SharedBy)}
				if err := tx.PutChange(&rc); err != nil {
					return err
				}
			}
			if files, err = w.carryUnlabeled(tx, labeled, files, newLabeled, &conflicts, res); err != nil {
				return err
			}
			t.BaseTask, t.BaseVersion, t.BaseSnapshot = remote.BaseTask, remote.BaseVersion, remote.BaseSnapshot
			t.ShareSeq, t.SharedBy, t.SyncedSnapshot, t.Status = remote.ShareSeq, remote.SharedBy, remote.Latest, remote.Status
			res.Steps = append(res.Steps, fmt.Sprintf("brought in changes %s shared to this task", remote.SharedBy))
		}

		// 2. What the task is built on.
		if up.Snapshot != t.BaseSnapshot {
			labeled, err := tx.TaskSnapshot(t)
			if err != nil {
				return err
			}
			newLabeled := up.Snapshot
			hasChanges := labeled != t.BaseSnapshot
			if hasChanges {
				m, err := w.merge3(tx, t.BaseSnapshot, labeled, up.Snapshot, fmt.Sprintf("task #%d", t.ID), up.Label)
				if err != nil {
					return err
				}
				conflicts = append(conflicts, m.Conflicts...)
				res.Notices = append(res.Notices, m.Notices...)
				if newLabeled, err = tx.PutSnapshot(m.Snapshot); err != nil {
					return err
				}
				last, _, err := tx.LatestChange(t.ID)
				if err != nil {
					return err
				}
				rc := store.Change{ID: project.NewChangeID(), Task: t.ID, Seq: last.Seq + 1, Snapshot: newLabeled,
					Author: w.User, Label: "Refreshed to " + up.Label}
				if err := tx.PutChange(&rc); err != nil {
					return err
				}
			}
			if files, err = w.carryUnlabeled(tx, labeled, files, newLabeled, &conflicts, res); err != nil {
				return err
			}
			t.BaseTask, t.BaseVersion, t.BaseSnapshot = up.Task, up.Version, up.Snapshot
			res.Steps = append(res.Steps, "refreshed to "+up.Label)
		}
		res.BuiltOn = builtOn(t)
		if len(res.Steps) == 0 {
			res.UpToDate = true
			return nil
		}
		t.Conflicts = append(t.Conflicts, conflicts...)
		res.Conflicts = t.Conflicts
		if err := tx.PutTask(&t); err != nil {
			return err
		}
		if files != cur {
			target, err := tx.Snapshot(files)
			if err != nil {
				return err
			}
			if err := applyFiles(tx, w, curSnap, target); err != nil {
				return err
			}
		}
		after, err := captureState(tx, t)
		if err != nil {
			return err
		}
		if err := recordAction(tx, w.ID, "refresh", "refresh: "+joinSteps(res.Steps), before, withFiles(after, files)); err != nil {
			return err
		}
		_, _, err = w.catchUp(tx)
		return err
	})
	return res, err
}

func joinSteps(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ", then "
		}
		out += x
	}
	return out
}

func (w *WS) merge3(tx *store.Tx, base, ours, theirs store.Hash, ol, tl string) (*merge.Result, error) {
	b, err := tx.Snapshot(base)
	if err != nil {
		return nil, err
	}
	o, err := tx.Snapshot(ours)
	if err != nil {
		return nil, err
	}
	t, err := tx.Snapshot(theirs)
	if err != nil {
		return nil, err
	}
	return merge.Trees(tx, b, o, t, merge.Labels{Ours: ol, Theirs: tl})
}

// carryUnlabeled re-applies the workspace's unlabeled work (files vs the old
// labeled state) on top of the new labeled state.
func (w *WS) carryUnlabeled(tx *store.Tx, oldLabeled, files, newLabeled store.Hash, conflicts *[]store.Conflict, res *RefreshResult) (store.Hash, error) {
	if files == oldLabeled {
		return newLabeled, nil
	}
	m, err := w.merge3(tx, oldLabeled, files, newLabeled, "your unlabeled work", "refreshed")
	if err != nil {
		return files, err
	}
	*conflicts = append(*conflicts, m.Conflicts...)
	res.Notices = append(res.Notices, m.Notices...)
	return tx.PutSnapshot(m.Snapshot)
}

// ConflictList returns the workspace's unresolved conflicts.
func (w *WS) ConflictList(ctx context.Context) ([]store.Conflict, error) {
	if _, _, err := w.CatchUp(ctx); err != nil {
		return nil, err
	}
	var cs []store.Conflict
	err := w.DB.Read(ctx, func(tx *store.Tx) error {
		t, err := tx.Task(w.Task)
		cs = t.Conflicts
		return err
	})
	return cs, err
}

// Approve applies a task. Run in that task's workspace, it first checks that
// all labeled work has been shared.
func (e *Env) Approve(ctx context.Context, ref, projectName string) ([]store.Version, error) {
	id, err := project.ParseTask(ref)
	if err != nil {
		return nil, err
	}
	if w, err := e.Workspace(); err == nil {
		defer w.Close()
		if w.Task == id {
			if n, _ := w.unsharedCount(ctx); n > 0 {
				return nil, plyerr.New(plyerr.State, "ply share", "task #%d has %d labeled changes that aren't shared yet", id, n)
			}
		}
		return w.Client.Approve(ctx, id)
	}
	p, err := e.Project(projectName)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	return p.Client.Approve(ctx, id)
}

func applyFiles(tx *store.Tx, w *WS, cur, target *store.Snapshot) error {
	return workspace.Apply(tx, w.ID, w.Root, cur, target, nil)
}
