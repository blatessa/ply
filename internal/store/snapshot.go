package store

import (
	"bufio"
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Mode is a file's kind.
type Mode byte

const (
	File    Mode = 'f'
	Exec    Mode = 'x'
	Symlink Mode = 'l' // content is the link target
)

// Entry is one file in a snapshot.
type Entry struct {
	Path string `json:"path"` // slash-separated, relative to the workspace root
	Mode Mode   `json:"mode"`
	Hash Hash   `json:"hash"`
	Size int64  `json:"size"`
}

// Snapshot is the full set of files at a point in time, sorted by path.
//
// A snapshot is stored as a manifest blob, so it is chunked and deduplicated
// like file content: saving a change in a large tree stores only the few
// manifest chunks around the changed entries.
type Snapshot struct {
	Entries []Entry
}

const manifestHeader = "ply-snapshot 1\n"

// Sort orders entries by path. Snapshots must be sorted before storing.
func (s *Snapshot) Sort() {
	slices.SortFunc(s.Entries, func(a, b Entry) int { return strings.Compare(a.Path, b.Path) })
}

// Get returns the entry at path.
func (s *Snapshot) Get(path string) (Entry, bool) {
	i, ok := slices.BinarySearchFunc(s.Entries, path, func(e Entry, p string) int { return strings.Compare(e.Path, p) })
	if !ok {
		return Entry{}, false
	}
	return s.Entries[i], true
}

// Map returns the entries keyed by path.
func (s *Snapshot) Map() map[string]Entry {
	m := make(map[string]Entry, len(s.Entries))
	for _, e := range s.Entries {
		m[e.Path] = e
	}
	return m
}

// Encode serializes the snapshot as a manifest.
func (s *Snapshot) Encode() []byte {
	var b bytes.Buffer
	b.WriteString(manifestHeader)
	for _, e := range s.Entries {
		fmt.Fprintf(&b, "%c %s %d %s\n", e.Mode, e.Hash, e.Size, e.Path)
	}
	return b.Bytes()
}

// DecodeSnapshot parses a manifest.
func DecodeSnapshot(data []byte) (*Snapshot, error) {
	if !bytes.HasPrefix(data, []byte(manifestHeader)) {
		return nil, fmt.Errorf("not a snapshot")
	}
	s := &Snapshot{}
	sc := bufio.NewScanner(bytes.NewReader(data[len(manifestHeader):]))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), " ", 4)
		if len(parts) != 4 || len(parts[0]) != 1 {
			return nil, fmt.Errorf("bad snapshot line %q", sc.Text())
		}
		h, err := ParseHash(parts[1])
		if err != nil {
			return nil, err
		}
		size, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil, err
		}
		s.Entries = append(s.Entries, Entry{Mode: Mode(parts[0][0]), Hash: h, Size: size, Path: parts[3]})
	}
	return s, sc.Err()
}

// ValidPath reports whether p can be stored in a snapshot.
func ValidPath(p string) bool {
	return p != "" && !strings.ContainsAny(p, "\n\r\x00") && !strings.HasPrefix(p, "/")
}

// PutSnapshot stores a snapshot and returns its hash.
func (t *Tx) PutSnapshot(s *Snapshot) (Hash, error) {
	s.Sort()
	for _, e := range s.Entries {
		if !ValidPath(e.Path) {
			return Hash{}, fmt.Errorf("invalid path %q", e.Path)
		}
	}
	return t.PutBlobBytes(s.Encode())
}

// Snapshot loads a stored snapshot.
func (t *Tx) Snapshot(h Hash) (*Snapshot, error) {
	data, err := t.ReadBlob(h)
	if err != nil {
		return nil, err
	}
	return DecodeSnapshot(data)
}

// DeltaKind says how a path differs between two snapshots.
type DeltaKind string

const (
	Added    DeltaKind = "added"
	Removed  DeltaKind = "removed"
	Modified DeltaKind = "modified"
)

// Delta is one path's difference between two snapshots.
type Delta struct {
	Path string    `json:"path"`
	Kind DeltaKind `json:"kind"`
	Old  Entry     `json:"-"`
	New  Entry     `json:"-"`
}

// Diff lists the paths that differ from a to b, in path order.
func Diff(a, b *Snapshot) []Delta {
	var out []Delta
	i, j := 0, 0
	for i < len(a.Entries) || j < len(b.Entries) {
		switch {
		case j >= len(b.Entries) || (i < len(a.Entries) && a.Entries[i].Path < b.Entries[j].Path):
			out = append(out, Delta{Path: a.Entries[i].Path, Kind: Removed, Old: a.Entries[i]})
			i++
		case i >= len(a.Entries) || b.Entries[j].Path < a.Entries[i].Path:
			out = append(out, Delta{Path: b.Entries[j].Path, Kind: Added, New: b.Entries[j]})
			j++
		default:
			x, y := a.Entries[i], b.Entries[j]
			if x.Hash != y.Hash || x.Mode != y.Mode {
				out = append(out, Delta{Path: x.Path, Kind: Modified, Old: x, New: y})
			}
			i++
			j++
		}
	}
	return out
}
