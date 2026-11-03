package merge

import (
	"fmt"
	"strings"
)

// Unified writes a unified diff of a and b with the given context lines.
// It returns "" when they're equal.
func Unified(aName, bName string, a, b []byte, context int) string {
	al, bl := Lines(a), Lines(b)
	hunks := Diff(al, bl)
	if len(hunks) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", aName, bName)
	for i := 0; i < len(hunks); {
		// Group hunks whose context overlaps.
		j := i
		for j+1 < len(hunks) && hunks[j+1].A0-hunks[j].A1 <= 2*context {
			j++
		}
		a0 := max(0, hunks[i].A0-context)
		a1 := min(len(al), hunks[j].A1+context)
		b0 := hunks[i].B0 - (hunks[i].A0 - a0)
		b1 := hunks[j].B1 + (a1 - hunks[j].A1)
		fmt.Fprintf(&sb, "@@ -%s +%s @@\n", rangeStr(a0, a1), rangeStr(b0, b1))
		pos := a0
		for k := i; k <= j; k++ {
			h := hunks[k]
			for _, l := range al[pos:h.A0] {
				writeLine(&sb, ' ', l)
			}
			for _, l := range al[h.A0:h.A1] {
				writeLine(&sb, '-', l)
			}
			for _, l := range bl[h.B0:h.B1] {
				writeLine(&sb, '+', l)
			}
			pos = h.A1
		}
		for _, l := range al[pos:a1] {
			writeLine(&sb, ' ', l)
		}
		i = j + 1
	}
	return sb.String()
}

func rangeStr(lo, hi int) string {
	n := hi - lo
	start := lo + 1
	if n == 0 {
		start = lo
	}
	if n == 1 {
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, n)
}

func writeLine(sb *strings.Builder, prefix byte, l string) {
	sb.WriteByte(prefix)
	sb.WriteString(l)
	if !strings.HasSuffix(l, "\n") {
		sb.WriteString("\n\\ No newline at end of file\n")
	}
}
