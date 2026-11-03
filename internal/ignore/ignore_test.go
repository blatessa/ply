package ignore

import "testing"

func TestMatch(t *testing.T) {
	m := New([]byte(`
# comment
build/
*.log
!keep.log
/top-only.txt
docs/**/*.tmp
!node_modules/
`))
	cases := []struct {
		path string
		dir  bool
		want bool
	}{
		{".ply", false, true},
		{"src/.ply", false, false},
		{"node_modules", true, false}, // re-included
		{".git", true, true},
		{"a/.DS_Store", false, true},
		{"build", true, true},
		{"build", false, false}, // dir-only rule
		{"src/build", true, true},
		{"x.log", false, true},
		{"deep/keep.log", false, false},
		{"top-only.txt", false, true},
		{"sub/top-only.txt", false, false},
		{"docs/a/b/c.tmp", false, true},
		{"docs/c.tmp", false, true},
		{"other/c.tmp", false, false},
		{".env", false, true},
		{".env.local", false, true},
		{".env.example", false, false},
		{"main.go", false, false},
	}
	for _, c := range cases {
		if got := m.Match(c.path, c.dir); got != c.want {
			t.Errorf("Match(%q, dir=%v) = %v, want %v", c.path, c.dir, got, c.want)
		}
	}
}
