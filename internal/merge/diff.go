// Package merge compares and combines file contents and snapshots: line
// diffs, three-way line merges, and three-way merges of whole snapshots.
package merge

import "bytes"

// Lines splits content into lines, each keeping its trailing newline. The
// last line has none if the content doesn't end in a newline.
func Lines(b []byte) []string {
	var out []string
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, string(b))
			break
		}
		out = append(out, string(b[:i+1]))
		b = b[i+1:]
	}
	return out
}

// IsBinary reports whether content should be treated as binary (not merged
// line by line).
func IsBinary(b []byte) bool {
	if len(b) > 8000 {
		b = b[:8000]
	}
	return bytes.IndexByte(b, 0) >= 0
}

// Hunk says a[A0:A1] is replaced by b[B0:B1].
type Hunk struct {
	A0, A1, B0, B1 int
}

// Diff returns the hunks that turn a into b, in order, using Myers' linear-
// space algorithm.
func Diff(a, b []string) []Hunk {
	// Intern lines so comparisons are integer compares.
	ids := map[string]int{}
	intern := func(ls []string) []int {
		out := make([]int, len(ls))
		for i, l := range ls {
			id, ok := ids[l]
			if !ok {
				id = len(ids)
				ids[l] = id
			}
			out[i] = id
		}
		return out
	}
	d := &differ{a: intern(a), b: intern(b)}
	d.delA = make([]bool, len(a))
	d.insB = make([]bool, len(b))
	n := len(a) + len(b)
	d.vf = make([]int, 2*n+3)
	d.vb = make([]int, 2*n+3)
	d.compare(0, len(a), 0, len(b))

	var hunks []Hunk
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		if i < len(a) && j < len(b) && !d.delA[i] && !d.insB[j] {
			i++
			j++
			continue
		}
		h := Hunk{A0: i, B0: j}
		for i < len(a) && d.delA[i] {
			i++
		}
		for j < len(b) && d.insB[j] {
			j++
		}
		h.A1, h.B1 = i, j
		hunks = append(hunks, h)
	}
	return hunks
}

type differ struct {
	a, b   []int
	delA   []bool
	insB   []bool
	vf, vb []int
}

func (d *differ) compare(a0, a1, b0, b1 int) {
	for a0 < a1 && b0 < b1 && d.a[a0] == d.b[b0] {
		a0++
		b0++
	}
	for a0 < a1 && b0 < b1 && d.a[a1-1] == d.b[b1-1] {
		a1--
		b1--
	}
	switch {
	case a0 == a1:
		for j := b0; j < b1; j++ {
			d.insB[j] = true
		}
		return
	case b0 == b1:
		for i := a0; i < a1; i++ {
			d.delA[i] = true
		}
		return
	}
	dist, x0, y0, x1, y1 := d.middleSnake(a0, a1, b0, b1)
	if dist <= 1 {
		// Can't happen once common ends are stripped; be safe anyway.
		for i := a0; i < a1; i++ {
			d.delA[i] = true
		}
		for j := b0; j < b1; j++ {
			d.insB[j] = true
		}
		return
	}
	d.compare(a0, a0+x0, b0, b0+y0)
	d.compare(a0+x1, a1, b0+y1, b1)
}

// middleSnake finds the middle snake of the shortest edit script for
// a[a0:a1] vs b[b0:b1]. It returns the edit distance and the snake's start
// and end, relative to (a0, b0).
func (d *differ) middleSnake(a0, a1, b0, b1 int) (dist, x0, y0, x1, y1 int) {
	N, M := a1-a0, b1-b0
	delta := N - M
	odd := delta&1 != 0
	off := N + M + 1
	vf, vb := d.vf, d.vb
	vf[off+1], vb[off+1] = 0, 0
	maxD := (N + M + 1) / 2
	for D := 0; D <= maxD; D++ {
		for k := -D; k <= D; k += 2 {
			var x int
			if k == -D || (k != D && vf[off+k-1] < vf[off+k+1]) {
				x = vf[off+k+1]
			} else {
				x = vf[off+k-1] + 1
			}
			y := x - k
			sx, sy := x, y
			for x < N && y < M && d.a[a0+x] == d.b[b0+y] {
				x++
				y++
			}
			vf[off+k] = x
			if kb := delta - k; odd && kb >= -(D-1) && kb <= D-1 {
				if x+vb[off+kb] >= N {
					return 2*D - 1, sx, sy, x, y
				}
			}
		}
		for k := -D; k <= D; k += 2 {
			var x int
			if k == -D || (k != D && vb[off+k-1] < vb[off+k+1]) {
				x = vb[off+k+1]
			} else {
				x = vb[off+k-1] + 1
			}
			y := x - k
			sx, sy := x, y
			for x < N && y < M && d.a[a1-1-x] == d.b[b1-1-y] {
				x++
				y++
			}
			vb[off+k] = x
			if kf := delta - k; !odd && kf >= -D && kf <= D {
				if x+vf[off+kf] >= N {
					return 2 * D, N - x, M - y, N - sx, M - sy
				}
			}
		}
	}
	panic("merge: no middle snake")
}
