// Package local implements Ply's commands on this machine: workspaces,
// labels, undo, and syncing tasks with the server. The CLI is a thin layer
// over it.
package local

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/blatessa/ply/internal/client"
	"github.com/blatessa/ply/internal/merge"
	"github.com/blatessa/ply/internal/plyerr"
	"github.com/blatessa/ply/internal/store"
	"github.com/blatessa/ply/internal/workspace"
)

// Env is the machine context for commands.
type Env struct {
	Cfg *workspace.Config
	Dir string // current directory
}

// NewEnv loads this machine's config.
func NewEnv(dir string) (*Env, error) {
	cfg, err := workspace.LoadConfig()
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &Env{Cfg: cfg, Dir: abs}, nil
}

// Proj is an open joined project.
type Proj struct {
	*workspace.Project
	DB     *store.DB
	Client *client.Client
	User   string
}

func (p *Proj) Close() error { return p.DB.Close() }

func (e *Env) open(p *workspace.Project) (*Proj, error) {
	db, err := store.Open(p.DB)
	if err != nil {
		return nil, err
	}
	return &Proj{Project: p, DB: db, Client: client.New(p.URL, e.Cfg.User, p.Token), User: e.Cfg.User}, nil
}

// Project opens the project the current directory belongs to: the
// workspace's project, the project whose folder we're in, the named one, or
// the only one joined.
func (e *Env) Project(name string) (*Proj, error) {
	if name != "" {
		p := e.Cfg.Find(name)
		if p == nil {
			return nil, plyerr.New(plyerr.NotFound, "ply join <url>", "project %q hasn't been joined on this machine", name)
		}
		return e.open(p)
	}
	if w, err := workspace.FindWorkspace(e.Dir); err == nil {
		if p := e.Cfg.Find(w.Project); p != nil {
			return e.open(p)
		}
	}
	if p := e.Cfg.ProjectForDir(e.Dir); p != nil {
		return e.open(p)
	}
	switch len(e.Cfg.Projects) {
	case 0:
		return nil, plyerr.New(plyerr.NotFound, "ply join <url>", "no project joined on this machine")
	case 1:
		return e.open(e.Cfg.Projects[0])
	}
	var names []string
	for _, p := range e.Cfg.Projects {
		names = append(names, p.Name)
	}
	return nil, plyerr.New(plyerr.Invalid, "", "more than one project is joined (%s); run this inside a workspace or pass --project", strings.Join(names, ", "))
}

// WS is an open workspace.
type WS struct {
	*Proj
	Root string
	ID   int64
	Task int64
}

// Workspace opens the workspace containing the current directory.
func (e *Env) Workspace() (*WS, error) {
	w, err := workspace.FindWorkspace(e.Dir)
	if errors.Is(err, workspace.ErrNotInWorkspace) {
		return nil, plyerr.New(plyerr.Invalid, "ply new \"...\"` or `ply get <task>", "not in a workspace")
	}
	if err != nil {
		return nil, err
	}
	p := e.Cfg.Find(w.Project)
	if p == nil {
		return nil, plyerr.New(plyerr.NotFound, "ply join "+w.Project, "this workspace's project (%s) isn't joined on this machine", w.Project)
	}
	proj, err := e.open(p)
	if err != nil {
		return nil, err
	}
	ws := &WS{Proj: proj, Root: w.Root, Task: w.Task}
	err = proj.DB.Write(context.Background(), func(tx *store.Tx) error {
		err := tx.QueryRow("SELECT id FROM workspace WHERE path = ?", w.Root).Scan(&ws.ID)
		if errors.Is(err, sql.ErrNoRows) {
			// The folder moved: find it by task.
			err = tx.QueryRow("SELECT id FROM workspace WHERE task = ?", w.Task).Scan(&ws.ID)
			if err == nil {
				_, err = tx.Exec("UPDATE workspace SET path = ? WHERE id = ?", w.Root, ws.ID)
			}
		}
		if errors.Is(err, sql.ErrNoRows) {
			return plyerr.New(plyerr.NotFound, fmt.Sprintf("ply get %d", w.Task), "this folder's workspace was removed")
		}
		return err
	})
	if err != nil {
		proj.Close()
		return nil, err
	}
	return ws, nil
}

// Save is a recorded snapshot of a workspace.
type Save struct {
	ID       int64      `json:"id"`
	Snapshot store.Hash `json:"snapshot"`
	At       time.Time  `json:"at"`
}

// CatchUp scans the workspace and records a save if anything changed since
// the last one. Every command runs it first. It also clears conflicts that
// have been resolved.
func (w *WS) CatchUp(ctx context.Context) (*store.Snapshot, store.Hash, error) {
	var snap *store.Snapshot
	var h store.Hash
	err := w.DB.Write(ctx, func(tx *store.Tx) error {
		var err error
		snap, h, err = w.catchUp(tx)
		return err
	})
	return snap, h, err
}

func (w *WS) catchUp(tx *store.Tx) (*store.Snapshot, store.Hash, error) {
	snap, err := workspace.Scan(tx, w.ID, w.Root)
	if err != nil {
		return nil, store.Hash{}, err
	}
	t, err := tx.Task(w.Task)
	if err != nil {
		return nil, store.Hash{}, err
	}
	if len(t.Conflicts) > 0 {
		if changed, err := w.resolveConflicts(tx, &t, snap); err != nil {
			return nil, store.Hash{}, err
		} else if changed {
			if snap, err = workspace.Scan(tx, w.ID, w.Root); err != nil {
				return nil, store.Hash{}, err
			}
		}
	}
	h, err := tx.PutSnapshot(snap)
	if err != nil {
		return nil, h, err
	}
	var last []byte
	err = tx.QueryRow("SELECT snapshot FROM save WHERE workspace = ? ORDER BY id DESC LIMIT 1", w.ID).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && store.Hash(last) != h) {
		_, err = tx.Exec("INSERT INTO save (workspace, snapshot, at) VALUES (?, ?, ?)", w.ID, h[:], time.Now().UnixMilli())
	}
	return snap, h, err
}

// resolveConflicts drops conflicts that are resolved in the workspace: text
// files without markers, and binary conflicts where one version was removed
// (the other is moved to the original name). It reports whether files moved.
func (w *WS) resolveConflicts(tx *store.Tx, t *store.Task, snap *store.Snapshot) (bool, error) {
	var still []store.Conflict
	moved := false
	for _, c := range t.Conflicts {
		switch c.Kind {
		case merge.BinaryConflict:
			_, hasO := snap.Get(c.Ours)
			_, hasT := snap.Get(c.Theirs)
			if hasO && hasT {
				still = append(still, c)
				continue
			}
			keep := c.Ours
			if hasT {
				keep = c.Theirs
			}
			if hasO || hasT {
				if err := os.Rename(filepath.Join(w.Root, filepath.FromSlash(keep)), filepath.Join(w.Root, filepath.FromSlash(c.Path))); err != nil {
					return false, err
				}
				moved = true
			}
		default:
			e, ok := snap.Get(c.Path)
			if !ok {
				continue
			}
			b, err := tx.ReadBlob(e.Hash)
			if err != nil {
				return false, err
			}
			if merge.HasMarkers(b) {
				still = append(still, c)
			}
		}
	}
	if len(still) != len(t.Conflicts) {
		t.Conflicts = still
		if err := tx.PutTask(t); err != nil {
			return false, err
		}
	}
	return moved, nil
}

// Slug turns a title into a folder name.
func Slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		} else {
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	if b.Len() == 0 {
		return "task"
	}
	return b.String()
}

func snapshotOf(tx *store.Tx, h store.Hash) (*store.Snapshot, error) { return tx.Snapshot(h) }
