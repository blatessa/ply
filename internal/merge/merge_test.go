package merge

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

func apply(a, b []string, hs []Hunk) []string {
	var out []string
	pos := 0
	for _, h := range hs {
		out = append(out, a[pos:h.A0]...)
		out = append(out, b[h.B0:h.B1]...)
		pos = h.A1
	}
	return append(out, a[pos:]...)
}

// lcsLen is a reference O(NM) LCS to check Diff is minimal.
func lcsLen(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	return dp[0][0]
}

func randLines(r *rand.Rand, n, alphabet int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = string(rune('a'+r.IntN(alphabet))) + "\n"
	}
	return out
}

func TestDiffRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for iter := 0; iter < 3000; iter++ {
		a := randLines(r, r.IntN(30), 1+r.IntN(5))
		b := randLines(r, r.IntN(30), 1+r.IntN(5))
		hs := Diff(a, b)
		if got := apply(a, b, hs); !slices.Equal(got, b) {
			t.Fatalf("apply(diff(%q, %q)) = %q", a, b, got)
		}
		edits := 0
		for _, h := range hs {
			edits += (h.A1 - h.A0) + (h.B1 - h.B0)
		}
		if want := len(a) + len(b) - 2*lcsLen(a, b); edits != want {
			t.Fatalf("diff(%q, %q): %d edits, minimal is %d", a, b, edits, want)
		}
	}
}

func TestDiffLarge(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	a := randLines(r, 50000, 26)
	b := slices.Clone(a)
	for i := 0; i < 500; i++ {
		b[r.IntN(len(b))] = "changed\n"
	}
	if got := apply(a, b, Diff(a, b)); !slices.Equal(got, b) {
		t.Fatal("large diff doesn't reproduce b")
	}
}

func L(s string) []string  { return Lines([]byte(s)) }
func J(ls []string) string { return strings.Join(ls, "") }

func TestLines3(t *testing.T) {
	base := "a\nb\nc\nd\ne\n"
	cases := []struct {
		name, ours, theirs, want string
		conflicts                int
	}{
		{"only ours", "a\nB\nc\nd\ne\n", base, "a\nB\nc\nd\ne\n", 0},
		{"only theirs", base, "a\nb\nc\nD\ne\n", "a\nb\nc\nD\ne\n", 0},
		{"disjoint", "A\nb\nc\nd\ne\n", "a\nb\nc\nd\nE\n", "A\nb\nc\nd\nE\n", 0},
		{"adjacent", "a\nB\nc\nd\ne\n", "a\nb\nC\nd\ne\n", "a\nB\nC\nd\ne\n", 0},
		{"same change", "a\nX\nc\nd\ne\n", "a\nX\nc\nd\ne\n", "a\nX\nc\nd\ne\n", 0},
		{"insert both ends", "top\n" + base, base + "bottom\n", "top\n" + base + "bottom\n", 0},
		{"conflict", "a\nOURS\nc\nd\ne\n", "a\nTHEIRS\nc\nd\ne\n",
			"a\n<<<<<<< yours\nOURS\n=======\nTHEIRS\n>>>>>>> project #3\nc\nd\ne\n", 1},
		{"insert same point", "a\nb\nX\nc\nd\ne\n", "a\nb\nY\nc\nd\ne\n",
			"a\nb\n<<<<<<< yours\nX\n=======\nY\n>>>>>>> project #3\nc\nd\ne\n", 1},
		{"no trailing newline", "a\nb\nc\nd\nOURS", "a\nb\nc\nd\nTHEIRS",
			"a\nb\nc\nd\n<<<<<<< yours\nOURS\n=======\nTHEIRS\n>>>>>>> project #3\n", 1},
		{"delete vs edit", "", "a\nb\nC\nd\ne\n",
			"<<<<<<< yours\n=======\na\nb\nC\nd\ne\n>>>>>>> project #3\n", 1},
	}
	for _, c := range cases {
		got, n := Lines3(L(base), L(c.ours), L(c.theirs), "yours", "project #3")
		if J(got) != c.want || n != c.conflicts {
			t.Errorf("%s: got %d conflicts\n%s\nwant %d\n%s", c.name, n, J(got), c.conflicts, c.want)
		}
	}
}

// Merging with an unchanged side gives the other side, for random inputs;
// and disjoint random edits always merge cleanly into both edits.
func TestLines3Random(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for iter := 0; iter < 2000; iter++ {
		base := randLines(r, r.IntN(40), 4)
		x := randLines(r, r.IntN(40), 4)
		if got, n := Lines3(base, x, base, "o", "t"); n != 0 || !slices.Equal(got, x) {
			t.Fatalf("merge(base, x, base) != x\nbase=%q\nx=%q\ngot=%q", base, x, got)
		}
		if got, n := Lines3(base, base, x, "o", "t"); n != 0 || !slices.Equal(got, x) {
			t.Fatalf("merge(base, base, x) != x\nbase=%q\nx=%q\ngot=%q", base, x, got)
		}
		if got, n := Lines3(base, x, x, "o", "t"); n != 0 || !slices.Equal(got, x) {
			t.Fatalf("merge(base, x, x) != x")
		}
		// Disjoint: edit the first half on one side, second half on the other.
		if len(base) >= 6 {
			mid := len(base) / 2
			ours := slices.Concat([]string{"O\n"}, base[1:])
			theirs := slices.Concat(base[:len(base)-1], []string{"T\n"})
			want := slices.Concat([]string{"O\n"}, base[1:len(base)-1], []string{"T\n"})
			_ = mid
			if got, n := Lines3(base, ours, theirs, "o", "t"); n != 0 || !slices.Equal(got, want) {
				t.Fatalf("disjoint merge\nbase=%q\ngot=%q\nwant=%q", base, got, want)
			}
		}
	}
}

func TestUnified(t *testing.T) {
	a := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	b := "one\nTWO\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven"
	got := Unified("a/f", "b/f", []byte(a), []byte(b), 3)
	want := `--- a/f
+++ b/f
@@ -1,5 +1,5 @@
 one
-two
+TWO
 three
 four
 five
@@ -8,3 +8,4 @@
 eight
 nine
 ten
+eleven
\ No newline at end of file
`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if Unified("a", "b", []byte(a), []byte(a), 3) != "" {
		t.Fatal("equal inputs should give empty diff")
	}
}

func TestSidePath(t *testing.T) {
	for in, want := range map[string]string{
		"img/logo.png": "img/logo (yours).png",
		"Makefile.bin": "Makefile (yours).bin",
		"data":         "data (yours)",
		".hidden":      ".hidden (yours)",
	} {
		if got := SidePath(in, "yours"); got != want {
			t.Errorf("SidePath(%q) = %q, want %q", in, got, want)
		}
	}
}
