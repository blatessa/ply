package workspace

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/blatessa/ply/internal/ignore"
	"github.com/blatessa/ply/internal/store"
)

type cached struct {
	size, mtime, ino int64
	mode             store.Mode
	hash             store.Hash
}

// racyWindow: files modified this recently aren't trusted to the stat cache,
// since another write in the same timestamp tick would look unchanged.
const racyWindow = 2 * time.Second

// Matcher returns the ignore rules for a workspace, from its .plyignore.
func Matcher(root string) *ignore.Matcher {
	b, _ := os.ReadFile(filepath.Join(root, ignore.FileName))
	return ignore.New(b)
}

// Scan reads the workspace's files into a snapshot, storing any new content.
// Files whose size, mtime, inode and mode match the last scan aren't re-read.
func Scan(tx *store.Tx, wsID int64, root string) (*store.Snapshot, error) {
	cache, err := loadCache(tx, wsID)
	if err != nil {
		return nil, err
	}
	m := Matcher(root)
	snap := &store.Snapshot{}
	seen := map[string]bool{}
	now := time.Now()

	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, de := range entries {
			p := de.Name()
			if rel != "" {
				p = rel + "/" + p
			}
			abs := filepath.Join(dir, de.Name())
			if m.Match(p, de.IsDir()) || !store.ValidPath(p) {
				continue
			}
			if de.IsDir() {
				if isFolder(abs) {
					continue // another workspace folder nested inside this one
				}
				if err := walk(abs, p); err != nil {
					return err
				}
				continue
			}
			info, err := de.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue // removed while scanning
			}
			if err != nil {
				return err
			}
			mode, ok := modeOf(info)
			if !ok {
				continue // sockets, devices, etc.
			}
			st := statOf(info, mode)
			seen[p] = true
			if c, ok := cache[p]; ok && c.size == st.size && c.mtime == st.mtime && c.ino == st.ino && c.mode == mode {
				snap.Entries = append(snap.Entries, store.Entry{Path: p, Mode: mode, Hash: c.hash, Size: c.size})
				continue
			}
			e, err := storeFile(tx, abs, p, mode)
			if errors.Is(err, fs.ErrNotExist) {
				delete(seen, p)
				continue
			}
			if err != nil {
				return fmt.Errorf("record %s: %w", p, err)
			}
			snap.Entries = append(snap.Entries, e)
			st.hash = e.Hash
			if now.Sub(info.ModTime()) < racyWindow {
				st.mtime = 0 // re-check next time
			}
			if err := putCache(tx, wsID, p, st); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	for p := range cache {
		if !seen[p] {
			if _, err := txExec(tx, "DELETE FROM scan WHERE workspace = ? AND path = ?", wsID, p); err != nil {
				return nil, err
			}
		}
	}
	snap.Sort()
	return snap, nil
}

func modeOf(info fs.FileInfo) (store.Mode, bool) {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return store.Symlink, true
	case info.Mode().IsRegular():
		if info.Mode()&0o111 != 0 {
			return store.Exec, true
		}
		return store.File, true
	}
	return 0, false
}

func statOf(info fs.FileInfo, mode store.Mode) cached {
	c := cached{size: info.Size(), mtime: info.ModTime().UnixNano(), mode: mode}
	if s, ok := info.Sys().(*syscall.Stat_t); ok {
		c.ino = int64(s.Ino)
	}
	return c
}

func storeFile(tx *store.Tx, abs, p string, mode store.Mode) (store.Entry, error) {
	e := store.Entry{Path: p, Mode: mode}
	if mode == store.Symlink {
		target, err := os.Readlink(abs)
		if err != nil {
			return e, err
		}
		e.Hash, err = tx.PutBlobBytes([]byte(target))
		e.Size = int64(len(target))
		return e, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return e, err
	}
	defer f.Close()
	info, err := tx.PutBlob(f)
	e.Hash, e.Size = info.Hash, info.Size
	return e, err
}

func loadCache(tx *store.Tx, wsID int64) (map[string]cached, error) {
	rows, err := txQuery(tx, "SELECT path, size, mtime, ino, mode, hash FROM scan WHERE workspace = ?", wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]cached{}
	for rows.Next() {
		var p string
		var c cached
		var mode int
		var h []byte
		if err := rows.Scan(&p, &c.size, &c.mtime, &c.ino, &mode, &h); err != nil {
			return nil, err
		}
		c.mode = store.Mode(mode)
		copy(c.hash[:], h)
		out[p] = c
	}
	return out, rows.Err()
}

func putCache(tx *store.Tx, wsID int64, p string, c cached) error {
	_, err := txExec(tx, `INSERT OR REPLACE INTO scan (workspace, path, size, mtime, ino, mode, hash)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, wsID, p, c.size, c.mtime, c.ino, int(c.mode), c.hash[:])
	return err
}

// cacheFile records a file Ply just wrote so the next scan needn't re-read it.
func cacheFile(tx *store.Tx, wsID int64, abs string, e store.Entry) error {
	info, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	c := statOf(info, e.Mode)
	c.hash = e.Hash
	if time.Since(info.ModTime()) < racyWindow {
		c.mtime = 0
	}
	return putCache(tx, wsID, e.Path, c)
}

// Raw SQL access for workspace-local tables.
func txExec(tx *store.Tx, q string, args ...any) (sql.Result, error) { return tx.Exec(q, args...) }
func txQuery(tx *store.Tx, q string, args ...any) (*sql.Rows, error) { return tx.Query(q, args...) }

// abs converts a slash path to an OS path under root.
func abs(root, p string) string {
	return filepath.Join(root, filepath.FromSlash(p))
}

// isFolder reports whether dir is a workspace folder (has a marker).
func isFolder(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, MarkerName))
	return err == nil
}

// IsFolder reports whether dir is a workspace folder.
func IsFolder(dir string) bool { return isFolder(dir) }
