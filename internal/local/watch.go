package local

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"

	"github.com/blatessa/ply/internal/store"
	"github.com/blatessa/ply/internal/workspace"
)

// The watcher is one background process per user that records saves in every
// workspace as files change. It never changes files. Commands don't depend
// on it: each one catches up first, so nothing is missed if it isn't running.

const debounce = 700 * time.Millisecond

func lockPath() string { return filepath.Join(workspace.Home(), "watch.lock") }

// lockWatcher takes the single-watcher lock; it fails if one is running.
func lockWatcher() (*os.File, bool) {
	os.MkdirAll(workspace.Home(), 0o755)
	f, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	return f, true
}

// EnsureWatcher starts the background watcher if it isn't running.
// PLY_NO_WATCH=1 disables it (for tests and scripted environments).
func EnsureWatcher() {
	if os.Getenv("PLY_NO_WATCH") != "" {
		return
	}
	f, ok := lockWatcher()
	if !ok {
		return // already running
	}
	f.Close()
	exe, err := os.Executable()
	if err != nil {
		return
	}
	logf, err := os.OpenFile(filepath.Join(workspace.Home(), "watch.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer logf.Close()
	cmd := exec.Command(exe, "watch")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		cmd.Process.Release()
	}
}

// Watch runs the watcher until ctx is done. It returns immediately if
// another watcher is running.
func Watch(ctx context.Context, l *log.Logger) error {
	lock, ok := lockWatcher()
	if !ok {
		l.Printf("a watcher is already running")
		return nil
	}
	defer lock.Close()
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fw.Close()
	w := &watcher{fw: fw, log: l, roots: map[string]bool{}, dirs: map[string]string{}, timers: map[string]*time.Timer{}}
	w.refresh()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			w.refresh()
		case ev, ok := <-fw.Events:
			if !ok {
				return nil
			}
			w.event(ev)
		case err, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			l.Printf("watch: %v", err)
		}
	}
}

type watcher struct {
	fw     *fsnotify.Watcher
	log    *log.Logger
	mu     sync.Mutex
	roots  map[string]bool   // workspace roots being watched
	dirs   map[string]string // watched dir -> its workspace root
	timers map[string]*time.Timer
}

// refresh starts watching new workspaces and stops watching removed ones.
func (w *watcher) refresh() {
	cfg, err := workspace.LoadConfig()
	if err != nil {
		w.log.Printf("config: %v", err)
		return
	}
	current := map[string]bool{}
	for _, p := range cfg.Projects {
		db, err := store.Open(p.DB)
		if err != nil {
			continue
		}
		db.Read(context.Background(), func(tx *store.Tx) error {
			rows, err := tx.Query("SELECT path FROM workspace")
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var root string
				if rows.Scan(&root) == nil {
					current[root] = true
				}
			}
			return nil
		})
		db.Close()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for root := range current {
		if !w.roots[root] {
			w.roots[root] = true
			w.addTree(root, root)
		}
	}
	for root := range w.roots {
		if !current[root] {
			delete(w.roots, root)
			for d, r := range w.dirs {
				if r == root {
					w.fw.Remove(d)
					delete(w.dirs, d)
				}
			}
		}
	}
}

// addTree watches dir and its subdirectories, skipping ignored ones.
func (w *watcher) addTree(root, dir string) {
	m := workspace.Matcher(root)
	filepath.WalkDir(dir, func(p string, de os.DirEntry, err error) error {
		if err != nil || !de.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel != "." && m.Match(filepath.ToSlash(rel), true) {
			return filepath.SkipDir
		}
		if _, ok := w.dirs[p]; !ok {
			if err := w.fw.Add(p); err != nil {
				w.log.Printf("watch %s: %v", p, err)
				return filepath.SkipDir
			}
			w.dirs[p] = root
		}
		return nil
	})
}

func (w *watcher) event(ev fsnotify.Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	root, ok := w.dirs[filepath.Dir(ev.Name)]
	if !ok {
		return
	}
	base := filepath.Base(ev.Name)
	if base == workspace.MarkerName || strings.HasPrefix(base, ".ply-tmp-") {
		return
	}
	if ev.Has(fsnotify.Create) {
		if info, err := os.Lstat(ev.Name); err == nil && info.IsDir() {
			w.addTree(root, ev.Name)
		}
	}
	if t, ok := w.timers[root]; ok {
		t.Reset(debounce)
		return
	}
	w.timers[root] = time.AfterFunc(debounce, func() {
		w.mu.Lock()
		delete(w.timers, root)
		w.mu.Unlock()
		w.record(root)
	})
}

// record catches up a workspace, recording a save if anything changed.
func (w *watcher) record(root string) {
	env, err := NewEnv(root)
	if err != nil {
		return
	}
	ws, err := env.Workspace()
	if err != nil {
		return
	}
	defer ws.Close()
	if _, _, err := ws.CatchUp(context.Background()); err != nil {
		w.log.Printf("record %s: %v", root, err)
	}
}
