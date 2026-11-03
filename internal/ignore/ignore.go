// Package ignore decides which workspace files are kept out of history.
//
// Patterns use gitignore syntax and come from built-in defaults followed by
// the project's .plyignore file, so .plyignore can re-include a default with
// "!pattern".
package ignore

import (
	"bufio"
	"bytes"
	"path"
	"strings"
)

// FileName is the project's ignore file.
const FileName = ".plyignore"

// Defaults are always applied before .plyignore.
var Defaults = []string{
	// Ply's own files.
	"/.ply",
	".plyexport",
	// Other version control.
	".git/", ".hg/", ".svn/",
	// Dependencies and caches.
	"node_modules/",
	"__pycache__/", "*.pyc", ".venv/",
	".gradle/", ".cache/",
	// Editor and OS clutter.
	".DS_Store", "Thumbs.db",
	"*.swp", "*.swo", "*~", ".#*",
	// Local secrets.
	".env", ".env.*", "!.env.example",
}

// AlwaysIgnored can't be re-included: Ply's workspace marker.
const AlwaysIgnored = ".ply"

type rule struct {
	negate   bool
	dirOnly  bool
	anchored bool // contains a slash: match against the full path
	pattern  string
}

// Matcher matches paths against ignore rules.
type Matcher struct {
	rules []rule
}

// New builds a matcher from the defaults plus the given .plyignore content
// (which may be nil).
func New(plyignore []byte) *Matcher {
	m := &Matcher{}
	for _, p := range Defaults {
		m.add(p)
	}
	sc := bufio.NewScanner(bytes.NewReader(plyignore))
	for sc.Scan() {
		m.add(sc.Text())
	}
	return m
}

func (m *Matcher) add(line string) {
	line = strings.TrimRight(line, " \t\r")
	if line == "" || strings.HasPrefix(line, "#") {
		return
	}
	var r rule
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	} else if strings.HasPrefix(line, `\`) {
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if strings.Contains(line, "/") {
		r.anchored = true
		line = strings.TrimPrefix(line, "/")
	}
	if line == "" {
		return
	}
	r.pattern = line
	m.rules = append(m.rules, r)
}

// Match reports whether the slash-separated relative path is ignored. isDir
// says whether it names a directory. Callers walking a tree should skip
// ignored directories; files under them are not re-checked.
func (m *Matcher) Match(p string, isDir bool) bool {
	if p == AlwaysIgnored {
		return true
	}
	ignored := false
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if r.matches(p) {
			ignored = !r.negate
		}
	}
	return ignored
}

func (r rule) matches(p string) bool {
	if r.anchored {
		return globMatch(r.pattern, p)
	}
	return globMatch(r.pattern, path.Base(p))
}

// globMatch matches like path.Match, with "**" matching any number of path
// segments.
func globMatch(pattern, name string) bool {
	if !strings.Contains(pattern, "**") {
		ok, _ := path.Match(pattern, name)
		return ok
	}
	pp := strings.Split(pattern, "/")
	np := strings.Split(name, "/")
	return matchSegs(pp, np)
}

func matchSegs(pp, np []string) bool {
	for len(pp) > 0 {
		if pp[0] == "**" {
			pp = pp[1:]
			if len(pp) == 0 {
				return true
			}
			for i := range np {
				if matchSegs(pp, np[i:]) {
					return true
				}
			}
			return false
		}
		if len(np) == 0 {
			return false
		}
		if ok, _ := path.Match(pp[0], np[0]); !ok {
			return false
		}
		pp, np = pp[1:], np[1:]
	}
	return len(np) == 0
}
