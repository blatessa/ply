package merge

import (
	"path"
	"strings"

	"github.com/blatessa/ply/internal/store"
)

// Content reads and stores file contents during a merge.
type Content interface {
	ReadBlob(store.Hash) ([]byte, error)
	PutBlobBytes([]byte) (store.Hash, error)
}

// Conflict is a file both sides changed incompatibly.
type Conflict = store.Conflict

// Conflict kinds.
const (
	// TextConflict: the file contains conflict markers.
	TextConflict = "text"
	// BinaryConflict: both versions are kept side by side under Ours and
	// Theirs; the original path is absent until one is removed.
	BinaryConflict = "binary"
)

// Labels name the two sides in conflict markers and side-by-side files.
type Labels struct {
	Ours, Theirs string
}

// Result is a merged snapshot.
type Result struct {
	Snapshot  *store.Snapshot
	Conflicts []Conflict
	// Notices describe resolutions people should know about, such as a file
	// one side deleted and the other changed.
	Notices []string
}

// Trees merges two descendants of base. Paths changed on only one side take
// that side; text files changed on both are merged line by line.
func Trees(c Content, base, ours, theirs *store.Snapshot, l Labels) (*Result, error) {
	bm, om, tm := base.Map(), ours.Map(), theirs.Map()
	paths := map[string]bool{}
	for _, m := range []map[string]store.Entry{bm, om, tm} {
		for p := range m {
			paths[p] = true
		}
	}
	res := &Result{Snapshot: &store.Snapshot{}}
	add := func(e store.Entry) { res.Snapshot.Entries = append(res.Snapshot.Entries, e) }
	for p := range paths {
		b, hasB := bm[p]
		o, hasO := om[p]
		t, hasT := tm[p]
		same := func(x store.Entry, hx bool, y store.Entry, hy bool) bool {
			return hx == hy && (!hx || (x.Hash == y.Hash && x.Mode == y.Mode))
		}
		switch {
		case same(o, hasO, t, hasT):
			if hasO {
				add(o)
			}
		case same(b, hasB, o, hasO):
			if hasT {
				add(t)
			}
		case same(b, hasB, t, hasT):
			if hasO {
				add(o)
			}
		case hasO && hasT && o.Hash == t.Hash:
			// Same content, different modes: prefer the side that changed it.
			if hasB && o.Mode == b.Mode {
				add(t)
			} else {
				add(o)
			}
		default:
			if err := mergeFile(c, res, p, b, hasB, o, hasO, t, hasT, l); err != nil {
				return nil, err
			}
		}
	}
	res.Snapshot.Sort()
	return res, nil
}

func mergeFile(c Content, res *Result, p string, b store.Entry, hasB bool, o store.Entry, hasO bool, t store.Entry, hasT bool, l Labels) error {
	read := func(e store.Entry, ok bool) ([]byte, error) {
		if !ok {
			return nil, nil
		}
		return c.ReadBlob(e.Hash)
	}
	bb, err := read(b, hasB)
	if err != nil {
		return err
	}
	ob, err := read(o, hasO)
	if err != nil {
		return err
	}
	tb, err := read(t, hasT)
	if err != nil {
		return err
	}
	binary := o.Mode == store.Symlink || t.Mode == store.Symlink ||
		IsBinary(bb) || IsBinary(ob) || IsBinary(tb)

	if binary {
		switch {
		case !hasO:
			res.Notices = append(res.Notices, p+": deleted by "+l.Ours+" but changed by "+l.Theirs+"; kept the changed file")
			res.Snapshot.Entries = append(res.Snapshot.Entries, t)
		case !hasT:
			res.Notices = append(res.Notices, p+": deleted by "+l.Theirs+" but changed by "+l.Ours+"; kept the changed file")
			res.Snapshot.Entries = append(res.Snapshot.Entries, o)
		default:
			op, tp := SidePath(p, l.Ours), SidePath(p, l.Theirs)
			o.Path, t.Path = op, tp
			res.Snapshot.Entries = append(res.Snapshot.Entries, o, t)
			res.Conflicts = append(res.Conflicts, Conflict{Path: p, Kind: BinaryConflict, Ours: op, Theirs: tp})
		}
		return nil
	}

	merged, n := Lines3(Lines(bb), Lines(ob), Lines(tb), l.Ours, l.Theirs)
	content := []byte(strings.Join(merged, ""))
	if n == 0 && len(content) == 0 && (!hasO || !hasT) {
		return nil // deleted on one side, and the other's changes cancel out
	}
	h, err := c.PutBlobBytes(content)
	if err != nil {
		return err
	}
	mode := o.Mode
	if !hasO || (hasB && o.Mode == b.Mode) {
		mode = t.Mode
	}
	res.Snapshot.Entries = append(res.Snapshot.Entries, store.Entry{Path: p, Mode: mode, Hash: h, Size: int64(len(content))})
	if n > 0 {
		res.Conflicts = append(res.Conflicts, Conflict{Path: p, Kind: TextConflict})
	}
	return nil
}

// SidePath names one side's copy of a binary conflict: "img/logo.png" with
// label "yours" becomes "img/logo (yours).png".
func SidePath(p, label string) string {
	dir, file := path.Split(p)
	ext := path.Ext(file)
	if ext == file {
		ext = ""
	}
	label = strings.Map(func(r rune) rune {
		if r == '/' || r == '#' {
			return -1
		}
		return r
	}, label)
	return dir + strings.TrimSuffix(file, ext) + " (" + strings.TrimSpace(label) + ")" + ext
}
