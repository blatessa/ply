package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/blatessa/ply/internal/client"
	"github.com/blatessa/ply/internal/plyerr"
	"github.com/blatessa/ply/internal/project"
	"github.com/blatessa/ply/internal/store"
	"github.com/blatessa/ply/internal/workspace"
)

// A folder shows one task at a time. Each task's saves, unlabeled work and
// undo history live in the database, so switching a folder to another task
// and back restores the work exactly. Ignored files (node_modules, build
// output) belong to the folder and are never touched by a switch.
//
// join makes the project's main folder, showing the project itself. Extra
// folders (--separate) are for parallel work.

// Folder is one of this machine's folders for a project.
type Folder struct {
	Path string `json:"path"`
	Task int64  `json:"task"` // 0: showing the project
	Main bool   `json:"main"`
}

// Join connects this machine to a project and makes its folder at dir,
// showing the project's current files.
func (e *Env) Join(ctx context.Context, url, dir string) (*workspace.Project, error) {
	p, err := e.Cfg.AddProject(url, dir)
	if err != nil {
		return nil, plyerr.New(plyerr.Invalid, "", "%v", err)
	}
	c := client.New(p.URL, e.Cfg.User, p.Token)
	st, err := c.State(ctx, false)
	if err != nil {
		return nil, err
	}
	proj, err := e.open(p)
	if err != nil {
		return nil, err
	}
	defer proj.Close()
	if f, ok := proj.mainFolder(ctx); ok {
		if f.Path == dir {
			return p, e.Cfg.Save()
		}
		if _, err := os.Stat(f.Path); err == nil {
			return nil, plyerr.New(plyerr.Invalid, "cd "+f.Path, "%s is already joined; its folder is %s", p.Name, f.Path)
		}
		// The old main folder is gone: forget it, keeping task work.
		proj.DB.Write(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec("DELETE FROM workspace WHERE main = 1")
			return err
		})
	}
	if err := emptyDir(dir); err != nil {
		return nil, err
	}
	if err := proj.Client.Fetch(ctx, proj.DB, []store.Hash{st.Head.Snapshot}, proj.haveSnapshots(ctx)); err != nil {
		return nil, err
	}
	err = proj.DB.Write(ctx, func(tx *store.Tx) error {
		w, err := proj.newFolder(tx, dir, true, 0, st.Head.Num, st.Head.Snapshot)
		if err != nil {
			return err
		}
		return w.fill(tx, st.Head.Snapshot, "")
	})
	if err != nil {
		return nil, err
	}
	return p, e.Cfg.Save()
}

// emptyDir makes sure dir is missing or empty, and creates it.
func emptyDir(dir string) error {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return plyerr.New(plyerr.Invalid, "", "%s already exists and isn't empty", dir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

// newFolder registers a folder at path showing task (0: the project at
// version base).
func (p *Proj) newFolder(tx *store.Tx, path string, main bool, task, version int64, base store.Hash) (*WS, error) {
	var b []byte
	if !base.IsZero() {
		b = base[:]
	}
	res, err := tx.Exec("INSERT INTO workspace (path, task, main, base_version, base_snapshot, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		path, task, main, version, b, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	w := &WS{Proj: p, Root: path, Task: task, Main: main, BaseVersion: version, BaseSnapshot: base}
	w.ID, err = res.LastInsertId()
	return w, err
}

// fill writes files into a new, empty folder, copying ignored files from
// another folder if given, and records the first save.
func (w *WS) fill(tx *store.Tx, files store.Hash, copyIgnoredFrom string) error {
	snap, err := tx.Snapshot(files)
	if err != nil {
		return err
	}
	donors, err := workspace.LoadDonors(tx)
	if err != nil {
		return err
	}
	if err := workspace.Materialize(tx, w.ID, w.Root, snap, donors); err != nil {
		return err
	}
	if err := w.writeMarker(); err != nil {
		return err
	}
	if copyIgnoredFrom != "" {
		if err := workspace.CopyIgnored(copyIgnoredFrom, w.Root); err != nil {
			return fmt.Errorf("copy ignored files: %w", err)
		}
	}
	_, _, err = w.catchUp(tx)
	return err
}

// Folders lists this machine's folders for the project.
func (p *Proj) Folders(ctx context.Context) ([]Folder, error) {
	var out []Folder
	err := p.DB.Read(ctx, func(tx *store.Tx) error {
		rows, err := tx.Query("SELECT path, task, main FROM workspace ORDER BY main DESC, id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f Folder
			if err := rows.Scan(&f.Path, &f.Task, &f.Main); err != nil {
				return err
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	return out, err
}

func (p *Proj) mainFolder(ctx context.Context) (Folder, bool) {
	fs, _ := p.Folders(ctx)
	if len(fs) > 0 && fs[0].Main {
		return fs[0], true
	}
	return Folder{}, false
}

func (w *WS) otherFolders(ctx context.Context) ([]Folder, error) {
	fs, err := w.Folders(ctx)
	var out []Folder
	for _, f := range fs {
		if f.Path != w.Root {
			out = append(out, f)
		}
	}
	return out, err
}

// folderShowing returns the folder showing a task, if any.
func folderShowing(tx *store.Tx, task int64) (string, bool) {
	var path string
	if task == 0 || tx.QueryRow("SELECT path FROM workspace WHERE task = ?", task).Scan(&path) != nil {
		return "", false
	}
	return path, true
}

// adoptTask stores the server's view of a task locally: its record and its
// changes, as shared.
func adoptTask(tx *store.Tx, info *project.TaskInfo) error {
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
	return nil
}

func hasTimeline(ctx context.Context, db *store.DB, task int64) bool {
	ok := false
	db.Read(ctx, func(tx *store.Tx) error {
		_, ok, _ = lastSave(tx, task, 0)
		return nil
	})
	return ok
}

// target is what a folder is about to show, fetched from the server when
// this machine has no work of its own for it.
type target struct {
	task int64
	info *project.TaskInfo // set when adopting from the server
	head store.Version     // for the project
}

// prepare fetches what's needed to show task (0: the project).
func (p *Proj) prepare(ctx context.Context, task int64) (*target, error) {
	tg := &target{task: task}
	if task == 0 {
		st, err := p.Client.State(ctx, false)
		if err != nil {
			return nil, err
		}
		tg.head = st.Head
		return tg, p.Client.Fetch(ctx, p.DB, []store.Hash{st.Head.Snapshot}, p.haveSnapshots(ctx))
	}
	if hasTimeline(ctx, p.DB, task) {
		return tg, nil
	}
	info, err := p.Client.Task(ctx, task)
	if err != nil {
		return nil, err
	}
	tg.info = info
	return tg, p.Client.Fetch(ctx, p.DB, []store.Hash{info.Latest, info.BaseSnapshot}, p.haveSnapshots(ctx))
}

// files returns the files the folder should show for the target, setting up
// local state for it.
func (tg *target) files(tx *store.Tx, w *WS) (store.Hash, error) {
	if tg.task == 0 {
		last, ok, err := lastSave(tx, 0, w.ID)
		if err != nil {
			return last, err
		}
		if ok && last != w.BaseSnapshot {
			return last, nil // edits made in the project view
		}
		return tg.head.Snapshot, w.setBase(tx, tg.head.Num, tg.head.Snapshot)
	}
	if tg.info != nil {
		return tg.info.Latest, adoptTask(tx, tg.info)
	}
	last, _, err := lastSave(tx, tg.task, 0)
	return last, err
}

// show switches the folder to the target, writing only the files that
// differ. The folder's current work is already saved to its own timeline.
func (w *WS) show(tx *store.Tx, cur *store.Snapshot, tg *target) error {
	task := tg.task
	w.Task = task
	files, err := tg.files(tx, w)
	if err != nil {
		return err
	}
	snap, err := tx.Snapshot(files)
	if err != nil {
		return err
	}
	donors, err := workspace.LoadDonors(tx)
	if err != nil {
		return err
	}
	if err := workspace.Apply(tx, w.ID, w.Root, cur, snap, donors); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE workspace SET task = ? WHERE id = ?", task, w.ID); err != nil {
		return err
	}
	if err := w.writeMarker(); err != nil {
		return err
	}
	_, _, err = w.catchUp(tx)
	return err
}

// SwitchResult says what a folder now shows.
type SwitchResult struct {
	Task    *store.Task `json:"task,omitempty"` // nil: the project
	Project int64       `json:"project,omitempty"`
	Path    string      `json:"path"`
	BuiltOn string      `json:"built_on,omitempty"`
	// Restored is set when the folder came back to work this machine had.
	Restored bool   `json:"restored_work"`
	Warning  string `json:"warning,omitempty"`
}

// SwitchOptions configure a switch.
type SwitchOptions struct {
	Separate bool
	Path     string // with Separate: where to make the folder
	Project  string
}

// Switch makes the current folder show another task (or "project"), or with
// Separate makes an extra folder for it.
func (e *Env) Switch(ctx context.Context, ref string, o SwitchOptions) (*SwitchResult, error) {
	var task int64
	if ref != "project" {
		id, err := project.ParseTask(ref)
		if err != nil {
			return nil, err
		}
		task = id
	}
	cur, curErr := e.Workspace()
	separate := o.Separate || o.Path != ""
	var p *Proj
	switch {
	case curErr == nil:
		p = cur.Proj
	case separate:
		var err error
		if p, err = e.Project(o.Project); err != nil {
			return nil, err
		}
	default:
		return nil, e.notInFolder(o.Project)
	}
	defer p.Close()
	if !separate && cur.Task == task && task != 0 {
		return nil, plyerr.New(plyerr.Invalid, "", "this folder is already showing task #%d", task)
	}
	var shown string
	p.DB.Read(ctx, func(tx *store.Tx) error {
		shown, _ = folderShowing(tx, task)
		return nil
	})
	if shown != "" {
		return nil, plyerr.New(plyerr.Invalid, "cd "+shown, "task #%d is showing in another folder, %s", task, shown)
	}
	restored := task != 0 && hasTimeline(ctx, p.DB, task)
	tg, err := p.prepare(ctx, task)
	if err != nil {
		return nil, err
	}
	var w *WS
	if separate {
		from := ""
		if cur != nil {
			from = cur.Root
		} else if f, ok := p.mainFolder(ctx); ok {
			from = f.Path
		}
		title := "project"
		if tg.info != nil {
			title = tg.info.Title
		} else if task != 0 {
			p.DB.Read(ctx, func(tx *store.Tx) error {
				t, err := tx.Task(task)
				title = t.Title
				return err
			})
		}
		if w, err = p.separate(ctx, tg, o.Path, title, from); err != nil {
			return nil, err
		}
	} else {
		w = cur
		err = w.DB.Write(ctx, func(tx *store.Tx) error {
			curSnap, _, err := w.catchUp(tx)
			if err != nil {
				return err
			}
			return w.show(tx, curSnap, tg)
		})
		if err != nil {
			return nil, err
		}
	}
	p.prune(ctx)
	return w.switchResult(ctx, restored)
}

func (w *WS) switchResult(ctx context.Context, restored bool) (*SwitchResult, error) {
	r := &SwitchResult{Path: w.Root, Restored: restored}
	if w.Task == 0 {
		r.Project, r.BuiltOn = w.BaseVersion, fmt.Sprintf("project #%d", w.BaseVersion)
		return r, nil
	}
	err := w.DB.Read(ctx, func(tx *store.Tx) error {
		t, err := tx.Task(w.Task)
		r.Task, r.BuiltOn = &t, builtOn(t)
		return err
	})
	return r, err
}

// separate makes an extra folder showing the target.
func (p *Proj) separate(ctx context.Context, tg *target, path, title, copyIgnoredFrom string) (*WS, error) {
	if path == "" {
		main := p.Dir
		path = filepath.Join(filepath.Dir(main), filepath.Base(main)+"-"+Slug(title))
		if _, err := os.Stat(path); err == nil && tg.task != 0 {
			path = fmt.Sprintf("%s-%d", path, tg.task)
		}
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := emptyDir(path); err != nil {
		return nil, err
	}
	var w *WS
	err = p.DB.Write(ctx, func(tx *store.Tx) error {
		var err error
		if w, err = p.newFolder(tx, path, false, tg.task, tg.head.Num, tg.head.Snapshot); err != nil {
			return err
		}
		files, err := tg.files(tx, w)
		if err != nil {
			return err
		}
		return w.fill(tx, files, copyIgnoredFrom)
	})
	if err != nil {
		os.RemoveAll(path)
	}
	return w, err
}

func (e *Env) notInFolder(projectName string) error {
	if p, err := e.Project(projectName); err == nil {
		defer p.Close()
		if f, ok := p.mainFolder(context.Background()); ok {
			return plyerr.New(plyerr.Invalid, "cd "+f.Path+"` (or pass `--separate", "not in a project folder")
		}
	}
	return plyerr.New(plyerr.Invalid, "cd <project folder>` or `ply join <url>", "not in a project folder")
}

// NewResult describes a created task and the folder showing it.
type NewResult struct {
	Task    project.TaskInfo `json:"task"`
	Path    string           `json:"path"`
	BuiltOn string           `json:"built_on"`
	// Carried is set when edits made in a folder showing the project came
	// along into the task.
	Carried   int              `json:"carried_files,omitempty"`
	Conflicts []store.Conflict `json:"conflicts,omitempty"`
	Warning   string           `json:"warning,omitempty"`
}

// NewOptions configure a new task.
type NewOptions struct {
	Title       string
	Description string
	// On is "" or "project" for the project, "." for the task the folder
	// is showing, or a task reference.
	On       string
	Separate bool
	Path     string // implies Separate
	Project  string
}

// New creates a task and shows it in the current folder (or, with
// Separate, a new folder). Edits made in a folder showing the project come
// along; a task's unlabeled work stays with that task.
func (e *Env) New(ctx context.Context, o NewOptions) (*NewResult, error) {
	cur, curErr := e.Workspace()
	separate := o.Separate || o.Path != ""
	var p *Proj
	switch {
	case curErr == nil:
		p = cur.Proj
	case separate:
		var err error
		if p, err = e.Project(o.Project); err != nil {
			return nil, err
		}
	default:
		return nil, e.notInFolder(o.Project)
	}
	defer p.Close()

	var on int64
	switch o.On {
	case "", "project":
	case ".":
		if cur == nil {
			return nil, plyerr.New(plyerr.Invalid, "ply new \"...\" --on <task>", "--on with no task builds on the task this folder shows; run it in a folder, or name the task")
		}
		on = cur.Task
	default:
		id, err := project.ParseTask(o.On)
		if err != nil {
			return nil, err
		}
		on = id
	}
	var warning string
	if cur != nil && on != 0 && on == cur.Task {
		if n, _ := cur.unsharedCount(ctx); n > 0 {
			warning = fmt.Sprintf("task #%d has %d labeled changes that aren't shared; the new task builds on its last shared change (switch back and `ply share`, then `ply refresh` here)", cur.Task, n)
		}
	}
	info, err := p.Client.CreateTask(ctx, o.Title, o.Description, on)
	if err != nil {
		return nil, err
	}
	if err := p.Client.Fetch(ctx, p.DB, []store.Hash{info.Latest, info.BaseSnapshot}, p.haveSnapshots(ctx)); err != nil {
		return nil, err
	}
	res := &NewResult{Task: *info, BuiltOn: builtOn(info.Task), Warning: warning}
	tg := &target{task: info.ID, info: info}
	var w *WS
	if separate {
		from := ""
		if cur != nil {
			from = cur.Root
		} else if f, ok := p.mainFolder(ctx); ok {
			from = f.Path
		}
		if w, err = p.separate(ctx, tg, o.Path, info.Title, from); err != nil {
			return nil, err
		}
	} else {
		w = cur
		err = w.DB.Write(ctx, func(tx *store.Tx) error {
			curSnap, curH, err := w.catchUp(tx)
			if err != nil {
				return err
			}
			if w.Task != 0 || curH == w.BaseSnapshot {
				return w.show(tx, curSnap, tg)
			}
			return w.carryInto(tx, curSnap, curH, tg, res)
		})
		if err != nil {
			return nil, err
		}
	}
	res.Path = w.Root
	p.prune(ctx)
	return res, nil
}

// carryInto moves the edits made in a folder showing the project into the
// new task: they're merged onto what the task is built on and become its
// unlabeled work.
func (w *WS) carryInto(tx *store.Tx, curSnap *store.Snapshot, curH store.Hash, tg *target, res *NewResult) error {
	info := tg.info
	if err := adoptTask(tx, info); err != nil {
		return err
	}
	base, err := tx.Snapshot(w.BaseSnapshot)
	if err != nil {
		return err
	}
	res.Carried = len(store.Diff(base, curSnap))
	files := curH
	if w.BaseSnapshot != info.Latest {
		m, err := w.merge3(tx, w.BaseSnapshot, curH, info.Latest, "your edits", builtOn(info.Task))
		if err != nil {
			return err
		}
		if files, err = tx.PutSnapshot(m.Snapshot); err != nil {
			return err
		}
		if len(m.Conflicts) > 0 {
			t, err := tx.Task(info.ID)
			if err != nil {
				return err
			}
			t.Conflicts = m.Conflicts
			res.Conflicts = m.Conflicts
			if err := tx.PutTask(&t); err != nil {
				return err
			}
		}
	}
	// The project view's timeline is done with: its edits now belong to the
	// task.
	for _, q := range []string{"DELETE FROM save WHERE task = 0 AND workspace = ?", "DELETE FROM action WHERE task = 0 AND workspace = ?"} {
		if _, err := tx.Exec(q, w.ID); err != nil {
			return err
		}
	}
	target, err := tx.Snapshot(files)
	if err != nil {
		return err
	}
	if err := workspace.Apply(tx, w.ID, w.Root, curSnap, target, nil); err != nil {
		return err
	}
	w.Task = info.ID
	if _, err := tx.Exec("UPDATE workspace SET task = ? WHERE id = ?", w.Task, w.ID); err != nil {
		return err
	}
	if err := w.writeMarker(); err != nil {
		return err
	}
	_, _, err = w.catchUp(tx)
	return err
}

// Remove deletes an extra folder. The task's work stays on this machine:
// switch to the task in another folder to pick it up again.
func (w *WS) Remove(ctx context.Context, force bool) error {
	if w.Main {
		return plyerr.New(plyerr.Invalid, "ply switch <task>", "this is the project's main folder, which stays; only extra folders can be removed. Show another task in it with")
	}
	_, cur, err := w.CatchUp(ctx)
	if err != nil {
		return err
	}
	if w.Task == 0 && cur != w.BaseSnapshot && !force {
		return plyerr.New(plyerr.State, "ply remove --force", "this folder shows the project and has edits that would be lost; put them in a task with `ply new \"Title\"`, or")
	}
	err = w.DB.Write(ctx, func(tx *store.Tx) error {
		for _, q := range []string{"DELETE FROM save WHERE task = 0 AND workspace = ?", "DELETE FROM action WHERE task = 0 AND workspace = ?",
			"DELETE FROM workspace WHERE id = ?"} {
			if _, err := tx.Exec(q, w.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := os.RemoveAll(w.Root); err != nil {
		return err
	}
	w.prune(ctx)
	return nil
}

// prune drops the saves and undo history of tasks that are applied or
// closed and not showing in any folder, as long as nothing would be lost:
// work that was never labeled stays until it is.
func (p *Proj) prune(ctx context.Context) {
	var ids []int64
	p.DB.Read(ctx, func(tx *store.Tx) error {
		rows, err := tx.Query("SELECT DISTINCT task FROM save WHERE task != 0 AND task NOT IN (SELECT task FROM workspace)")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		return nil
	})
	if len(ids) == 0 {
		return
	}
	st, err := p.Client.State(ctx, true)
	if err != nil {
		return
	}
	done := map[int64]bool{}
	for _, t := range st.Tasks {
		done[t.ID] = t.Status == store.StatusApplied || t.Status == store.StatusClosed
	}
	p.DB.Write(ctx, func(tx *store.Tx) error {
		for _, id := range ids {
			if !done[id] {
				continue
			}
			t, err := tx.Task(id)
			if err != nil {
				continue
			}
			labeled, err := tx.TaskSnapshot(t)
			last, _, err2 := lastSave(tx, id, 0)
			if err != nil || err2 != nil || last != labeled {
				continue // unlabeled work: keep it
			}
			if n, _ := unshared(tx, id); n > 0 {
				continue
			}
			for _, q := range []string{"DELETE FROM save WHERE task = ? AND workspace = 0", "DELETE FROM action WHERE task = ? AND workspace = 0"} {
				if _, err := tx.Exec(q, id); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func unshared(tx *store.Tx, task int64) (int, error) {
	var n int
	err := tx.QueryRow("SELECT count(*) FROM change WHERE task = ? AND shared = 0", task).Scan(&n)
	return n, err
}
