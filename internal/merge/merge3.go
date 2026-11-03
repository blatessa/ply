package merge

import (
	"slices"
	"strings"
)

// Conflict markers. A file still contains a conflict while any line starts
// with MarkerStart or MarkerEnd.
const (
	MarkerStart = "<<<<<<< "
	MarkerMid   = "=======\n"
	MarkerEnd   = ">>>>>>> "
)

// Lines3 merges two descendants of base line by line. Changes to different
// parts are combined; overlapping changes that differ become conflicts,
// written with markers labeled oursLabel and theirsLabel. It returns the
// merged lines and the number of conflicts.
func Lines3(base, ours, theirs []string, oursLabel, theirsLabel string) ([]string, int) {
	ho, ht := Diff(base, ours), Diff(base, theirs)
	var out []string
	conflicts := 0
	pos := 0               // next base line not yet emitted
	dOurs, dTheirs := 0, 0 // offset from base to side coordinates before pos
	i, j := 0, 0
	for i < len(ho) || j < len(ht) {
		// Start a region at the earliest next hunk.
		var lo, hi int
		var inO, inT []Hunk
		if j >= len(ht) || (i < len(ho) && ho[i].A0 <= ht[j].A0) {
			lo, hi = ho[i].A0, ho[i].A1
			inO = append(inO, ho[i])
			i++
		} else {
			lo, hi = ht[j].A0, ht[j].A1
			inT = append(inT, ht[j])
			j++
		}
		// Absorb overlapping hunks from both sides until the region is stable.
		for {
			grew := false
			if i < len(ho) && overlaps(ho[i], lo, hi) {
				hi = max(hi, ho[i].A1)
				inO = append(inO, ho[i])
				i++
				grew = true
			}
			if j < len(ht) && overlaps(ht[j], lo, hi) {
				hi = max(hi, ht[j].A1)
				inT = append(inT, ht[j])
				j++
				grew = true
			}
			if !grew {
				break
			}
		}
		out = append(out, base[pos:lo]...)
		o0, o1 := sideRange(inO, lo, hi, dOurs)
		t0, t1 := sideRange(inT, lo, hi, dTheirs)
		switch {
		case len(inT) == 0:
			out = append(out, ours[o0:o1]...)
		case len(inO) == 0:
			out = append(out, theirs[t0:t1]...)
		case slices.Equal(ours[o0:o1], theirs[t0:t1]):
			out = append(out, ours[o0:o1]...)
		default:
			conflicts++
			out = append(out, MarkerStart+oursLabel+"\n")
			out = appendEnsuringNewline(out, ours[o0:o1])
			out = append(out, MarkerMid)
			out = appendEnsuringNewline(out, theirs[t0:t1])
			out = append(out, MarkerEnd+theirsLabel+"\n")
		}
		dOurs = o1 - hi
		dTheirs = t1 - hi
		pos = hi
	}
	out = append(out, base[pos:]...)
	return out, conflicts
}

// overlaps reports whether hunk h touches region [lo, hi). Hunks that merely
// sit next to each other don't overlap, except insertions at the same point.
func overlaps(h Hunk, lo, hi int) bool {
	if h.A0 < hi {
		return true
	}
	return h.A0 == hi && (h.A0 == h.A1 || lo == hi)
}

// sideRange maps base region [lo, hi) to one side's line range, given that
// side's hunks inside the region and its offset before the region.
func sideRange(hs []Hunk, lo, hi, d int) (int, int) {
	if len(hs) == 0 {
		return lo + d, hi + d
	}
	first, last := hs[0], hs[len(hs)-1]
	return first.B0 - (first.A0 - lo), last.B1 + (hi - last.A1)
}

// appendEnsuringNewline appends lines so the last one ends in a newline,
// keeping conflict markers on their own lines.
func appendEnsuringNewline(out, ls []string) []string {
	out = append(out, ls...)
	if n := len(out); n > 0 && len(ls) > 0 && !strings.HasSuffix(out[n-1], "\n") {
		out[n-1] += "\n"
	}
	return out
}

// HasMarkers reports whether content still contains conflict markers.
func HasMarkers(b []byte) bool {
	for _, l := range Lines(b) {
		if strings.HasPrefix(l, MarkerStart) || strings.HasPrefix(l, MarkerEnd) {
			return true
		}
	}
	return false
}
