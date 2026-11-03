package workspace

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/blatessa/ply/internal/store"
)

// Apply changes the files in root from snapshot cur to snapshot target:
// removing, adding and rewriting only what differs. cur must reflect the
// files on disk (from a scan just before). Ignored files are never touched.
// donors supplies existing copies of content to clone instead of writing.
// wsID 0 means the folder isn't a workspace (an export): no scan cache.
func Apply(tx *store.Tx, wsID int64, root string, cur, target *store.Snapshot, donors Donors) error {
	for _, d := range store.Diff(cur, target) {
		p := abs(root, d.Path)
		switch d.Kind {
		case store.Removed:
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			removeEmptyParents(root, filepath.Dir(p))
			if wsID != 0 {
				txExec(tx, "DELETE FROM scan WHERE workspace = ? AND path = ?", wsID, d.Path)
			}
		default:
			if err := writeEntry(tx, root, d.New, donors); err != nil {
				return err
			}
			if wsID == 0 {
				continue
			}
			if err := cacheFile(tx, wsID, p, d.New); err != nil {
				return err
			}
		}
	}
	return nil
}

// Materialize writes a snapshot into an empty folder.
func Materialize(tx *store.Tx, wsID int64, root string, target *store.Snapshot, donors Donors) error {
	return Apply(tx, wsID, root, &store.Snapshot{}, target, donors)
}

func writeEntry(tx *store.Tx, root string, e store.Entry, donors Donors) error {
	p := abs(root, e.Path)
	dir := filepath.Dir(p)
	if err := mkdirAll(root, dir); err != nil {
		return err
	}
	if e.Mode == store.Symlink {
		target, err := tx.ReadBlob(e.Hash)
		if err != nil {
			return err
		}
		os.Remove(p)
		return os.Symlink(string(target), p)
	}
	perm := os.FileMode(0o644)
	if e.Mode == store.Exec {
		perm = 0o755
	}
	tmp := filepath.Join(dir, ".ply-tmp-"+filepath.Base(p))
	os.Remove(tmp)
	if src := donors.Find(e.Hash, e.Size); src != "" {
		if err := CloneFile(src, tmp); err == nil {
			os.Chmod(tmp, perm)
			return os.Rename(tmp, p)
		}
		os.Remove(tmp)
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if err := tx.WriteBlob(f, e.Hash); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	os.Chmod(tmp, perm) // umask may have narrowed it
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// mkdirAll creates dir, replacing any file in the way (a path that was a
// file in the old snapshot and a directory in the new one).
func mkdirAll(root, dir string) error {
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		return nil
	}
	// Walk down from root, removing a non-directory blocking the path.
	rel, rerr := filepath.Rel(root, dir)
	if rerr != nil {
		return err
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		if info, serr := os.Lstat(cur); serr == nil && !info.IsDir() {
			os.Remove(cur)
			break
		}
	}
	return os.MkdirAll(dir, 0o755)
}

func removeEmptyParents(root, dir string) {
	for dir != root && strings.HasPrefix(dir, root) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// Donors are files on this machine known to hold given content, so new
// workspaces can clone them (copy-on-write where the filesystem allows)
// instead of writing content out of the database.
type Donors map[store.Hash]donor

type donor struct {
	path  string
	size  int64
	mtime int64
}

// Find returns a path holding the content, if one still looks unchanged.
func (d Donors) Find(h store.Hash, size int64) string {
	c, ok := d[h]
	if !ok {
		return ""
	}
	info, err := os.Lstat(c.path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size || info.ModTime().UnixNano() != c.mtime {
		return ""
	}
	return c.path
}

// LoadDonors collects the files of this machine's workspaces from their
// scan caches.
func LoadDonors(tx *store.Tx) (Donors, error) {
	rows, err := txQuery(tx, `SELECT w.path, s.path, s.size, s.mtime, s.hash
		FROM scan s JOIN workspace w ON w.id = s.workspace WHERE s.mode != ? AND s.mtime != 0`, int(store.Symlink))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	d := Donors{}
	for rows.Next() {
		var root, p string
		var c donor
		var h []byte
		if err := rows.Scan(&root, &p, &c.size, &c.mtime, &h); err != nil {
			return nil, err
		}
		c.path = abs(root, p)
		d[store.Hash(h)] = c
	}
	return d, rows.Err()
}

// CopyIgnored copies the files ignore rules keep out of history (such as
// node_modules) from one workspace to another, cloning where possible.
func CopyIgnored(from, to string) error {
	m := Matcher(from)
	var walk func(rel string) error
	walk = func(rel string) error {
		entries, err := os.ReadDir(filepath.Join(from, rel))
		if err != nil {
			return err
		}
		for _, de := range entries {
			p := filepath.ToSlash(filepath.Join(rel, de.Name()))
			if p == MarkerName {
				continue
			}
			if m.Match(p, de.IsDir()) {
				if err := copyTree(filepath.Join(from, p), filepath.Join(to, p)); err != nil {
					return err
				}
			} else if de.IsDir() && !isFolder(filepath.Join(from, p)) {
				if err := walk(p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk("")
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		info, err := de.Info()
		if err != nil {
			return err
		}
		switch {
		case de.IsDir():
			return os.MkdirAll(out, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(t, out)
		case info.Mode().IsRegular():
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			if err := CloneFile(p, out); err != nil {
				return err
			}
			return os.Chmod(out, info.Mode().Perm())
		}
		return nil
	})
}

// copyFile is the fallback when the filesystem can't clone.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
