package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blatessa/ply/internal/cli"
	"github.com/blatessa/ply/internal/server"
)

// world is a server plus people, each with their own machine (PLY_HOME).
type world struct {
	t   *testing.T
	url string
	dir string
}

func newWorld(t *testing.T) *world {
	dir := t.TempDir()
	s, err := server.New(filepath.Join(dir, "server"))
	if err != nil {
		t.Fatal(err)
	}
	s.Log.SetOutput(&bytes.Buffer{})
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() { hs.Close(); s.Close() })
	w := &world{t: t, url: hs.URL, dir: dir}
	if err := s.CreateProject("demo"); err != nil {
		t.Fatal(err)
	}
	return w
}

type person struct {
	w    *world
	name string
	cwd  string
}

func (w *world) person(name string) *person {
	p := &person{w: w, name: name, cwd: filepath.Join(w.dir, name)}
	os.MkdirAll(p.cwd, 0o755)
	p.ok("join", w.url+"/demo")
	p.cd("demo")
	return p
}

func (p *person) cd(dir string) {
	if filepath.IsAbs(dir) {
		p.cwd = dir
	} else {
		p.cwd = filepath.Join(p.cwd, dir)
	}
}

// run runs a ply command as this person, returning output and exit code.
func (p *person) run(args ...string) (string, int) {
	p.w.t.Helper()
	os.Setenv("PLY_HOME", filepath.Join(p.w.dir, "home-"+p.name))
	os.Setenv("PLY_USER", p.name)
	os.Setenv("PLY_NO_WATCH", "1")
	var out bytes.Buffer
	c := &cli.CLI{Out: &out, Err: &out, Dir: p.cwd}
	code := c.Run(context.Background(), args)
	return out.String(), code
}

func (p *person) ok(args ...string) string {
	p.w.t.Helper()
	out, code := p.run(args...)
	if code != 0 {
		p.w.t.Fatalf("%s: ply %s failed:\n%s", p.name, strings.Join(args, " "), out)
	}
	return out
}

func (p *person) fails(want string, args ...string) string {
	p.w.t.Helper()
	out, code := p.run(args...)
	if code == 0 {
		p.w.t.Fatalf("%s: ply %s should have failed:\n%s", p.name, strings.Join(args, " "), out)
	}
	if !strings.Contains(out, want) {
		p.w.t.Fatalf("%s: ply %s: want error containing %q, got:\n%s", p.name, strings.Join(args, " "), want, out)
	}
	return out
}

func (p *person) json(v any, args ...string) {
	p.w.t.Helper()
	out := p.ok(append(args, "--json")...)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		p.w.t.Fatalf("bad json from ply %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func (p *person) write(path, content string) {
	full := filepath.Join(p.cwd, path)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		p.w.t.Fatal(err)
	}
}

func (p *person) read(path string) string {
	b, err := os.ReadFile(filepath.Join(p.cwd, path))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func (p *person) exists(path string) bool {
	_, err := os.Stat(filepath.Join(p.cwd, path))
	return err == nil
}

// newTask creates a task and moves into its workspace.
func (p *person) newTask(title string, args ...string) {
	p.w.t.Helper()
	var r struct {
		Path string `json:"path"`
	}
	p.json(&r, append([]string{"new", title}, args...)...)
	p.cd(r.Path)
}

// seed applies an initial task with the given files.
func (p *person) seed(files map[string]string) {
	p.w.t.Helper()
	home := p.cwd
	p.newTask("Seed")
	for k, v := range files {
		p.write(k, v)
	}
	p.ok("label", "Seed files")
	p.ok("share")
	p.ok("approve")
	p.cd(home)
}

const lines = "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\n"

func TestApproveCombinesWithoutRefresh(t *testing.T) {
	w := newWorld(t)
	a, b := w.person("alice"), w.person("bob")
	a.seed(map[string]string{"f.txt": lines})
	bhome := b.cwd
	b.newTask("Edit top")
	b.write("f.txt", strings.Replace(lines, "one", "ONE", 1))
	b.ok("label", "top")
	b.ok("share")
	b.cd(bhome)

	a.newTask("Edit bottom")
	a.write("f.txt", strings.Replace(lines, "eight", "EIGHT", 1))
	a.ok("label", "bottom")
	a.ok("share")
	a.ok("approve")

	// Bob's task is behind but touches different lines: approve combines.
	b.ok("approve", "2")
	b.ok("export", filepath.Join(w.dir, "out"))
	got, _ := os.ReadFile(filepath.Join(w.dir, "out", "f.txt"))
	want := strings.Replace(strings.Replace(lines, "one", "ONE", 1), "eight", "EIGHT", 1)
	if string(got) != want {
		t.Fatalf("combined export:\n%s\nwant\n%s", got, want)
	}
	if info, _ := os.ReadFile(filepath.Join(w.dir, "out", ".plyexport")); !strings.Contains(string(info), "project #3") {
		t.Fatalf(".plyexport: %s", info)
	}
}

func TestConflictRefreshResolve(t *testing.T) {
	w := newWorld(t)
	a, b := w.person("alice"), w.person("bob")
	a.seed(map[string]string{"f.txt": lines})
	bhome := b.cwd
	b.newTask("Bob's edit")
	b.write("f.txt", strings.Replace(lines, "four", "BOB", 1))
	b.ok("label", "bob")
	b.ok("share")
	b.cd(bhome)

	a.newTask("Alice's edit")
	a.write("f.txt", strings.Replace(lines, "four", "ALICE", 1))
	a.ok("label", "alice")
	a.ok("share")
	a.ok("approve")

	b.cd(filepath.Join(bhome, "bob-s-edit"))
	b.fails("conflicts with project", "approve")
	out := b.ok("refresh")
	if !strings.Contains(out, "1 conflicts") {
		t.Fatalf("refresh output:\n%s", out)
	}
	f := b.read("f.txt")
	if !strings.Contains(f, "<<<<<<< task #2") || !strings.Contains(f, ">>>>>>> project #2") {
		t.Fatalf("no markers:\n%s", f)
	}
	var cs struct {
		Conflicts []struct{ Path string } `json:"conflicts"`
	}
	b.json(&cs, "conflicts")
	if len(cs.Conflicts) != 1 {
		t.Fatalf("conflicts: %+v", cs)
	}
	// Sharing with conflicts is allowed, approving isn't.
	b.ok("share")
	b.fails("unresolved conflicts", "approve")
	// Resolve, label, share, approve.
	b.write("f.txt", strings.Replace(lines, "four", "ALICE and BOB", 1))
	b.json(&cs, "conflicts")
	if len(cs.Conflicts) != 0 {
		t.Fatalf("conflict not cleared: %+v", cs)
	}
	b.ok("label", "Resolve")
	b.ok("share")
	b.ok("approve")
	// The review diff is against the refreshed base, so it shows only Bob's work.
	var d struct {
		Diff struct {
			Text string `json:"diff"`
		} `json:"diff"`
	}
	b.json(&d, "review", "2")
	if !strings.Contains(d.Diff.Text, "-ALICE\n+ALICE and BOB") {
		t.Fatalf("review diff:\n%s", d.Diff.Text)
	}
}

func TestRefreshCarriesUnlabeledWork(t *testing.T) {
	w := newWorld(t)
	a, b := w.person("alice"), w.person("bob")
	a.seed(map[string]string{"f.txt": lines, "g.txt": "g\n"})
	bhome := b.cwd
	b.newTask("Work")
	b.write("f.txt", strings.Replace(lines, "one", "labeled", 1))
	b.ok("label", "labeled")
	b.write("f.txt", strings.Replace(strings.Replace(lines, "one", "labeled", 1), "three", "unlabeled", 1))
	b.cd(bhome)

	a.newTask("Other")
	a.write("f.txt", strings.Replace(lines, "eight", "theirs", 1))
	a.write("g.txt", "g changed\n")
	a.ok("label", "other")
	a.ok("share")
	a.ok("approve")

	b.cd(filepath.Join(bhome, "work"))
	b.ok("refresh")
	want := strings.Replace(strings.Replace(strings.Replace(lines, "one", "labeled", 1), "three", "unlabeled", 1), "eight", "theirs", 1)
	if got := b.read("f.txt"); got != want {
		t.Fatalf("after refresh:\n%s\nwant\n%s", got, want)
	}
	if b.read("g.txt") != "g changed\n" {
		t.Fatal("g.txt not refreshed")
	}
	// The unlabeled edit is still unlabeled.
	var st struct {
		Unlabeled []struct{ Path string } `json:"unlabeled"`
	}
	b.json(&st, "status")
	if len(st.Unlabeled) != 1 || st.Unlabeled[0].Path != "f.txt" {
		t.Fatalf("unlabeled after refresh: %+v", st.Unlabeled)
	}
	// Undo the refresh: back to before, unlabeled work intact.
	b.ok("undo")
	if got := b.read("f.txt"); got != strings.Replace(strings.Replace(lines, "one", "labeled", 1), "three", "unlabeled", 1) {
		t.Fatalf("after undo:\n%s", got)
	}
	if b.read("g.txt") != "g\n" {
		t.Fatal("undo didn't restore g.txt")
	}
	b.ok("redo")
	if got := b.read("f.txt"); got != want {
		t.Fatalf("after redo:\n%s", got)
	}
}

func TestStaleShare(t *testing.T) {
	w := newWorld(t)
	a, b := w.person("alice"), w.person("bob")
	a.seed(map[string]string{"f.txt": lines})
	a.newTask("Shared work")
	a.write("f.txt", strings.Replace(lines, "one", "alice", 1))
	a.ok("label", "alice 1")
	a.ok("share")

	b.ok("get", "2")
	b.cd("shared-work")
	b.write("f.txt", strings.Replace(strings.Replace(lines, "one", "alice", 1), "eight", "bob", 1))
	b.ok("label", "bob 1")
	b.ok("share")

	a.write("new.txt", "alice's new file\n")
	a.ok("label", "alice 2")
	a.fails("bob shared newer changes", "share")
	a.ok("refresh")
	if !strings.Contains(a.read("f.txt"), "bob") || a.read("new.txt") != "alice's new file\n" {
		t.Fatalf("refresh didn't combine: %q %q", a.read("f.txt"), a.read("new.txt"))
	}
	a.ok("share")
	var info struct {
		Task struct {
			Changes []struct{ Label string } `json:"changes"`
		} `json:"task"`
	}
	a.json(&info, "review")
	var labels []string
	for _, c := range info.Task.Changes {
		labels = append(labels, c.Label)
	}
	if got := strings.Join(labels, ","); got != "alice 1,bob 1,alice 2,Refreshed with changes shared by bob" {
		t.Fatalf("changes: %s", got)
	}
}

func TestStackedTasks(t *testing.T) {
	w := newWorld(t)
	a := w.person("alice")
	a.seed(map[string]string{"f.txt": lines})
	home := a.cwd
	a.newTask("Fix login bug")
	a.write("login.txt", "fixed\n")
	a.ok("label", "fix")
	a.ok("share")
	login := a.cwd

	// New task inside a workspace builds on that task.
	var r struct {
		Path    string `json:"path"`
		BuiltOn string `json:"built_on"`
	}
	a.json(&r, "new", "Add password reset")
	if r.BuiltOn != "task #2" {
		t.Fatalf("built on %q", r.BuiltOn)
	}
	a.cd(r.Path)
	if a.read("login.txt") != "fixed\n" {
		t.Fatal("stacked task lacks the underlying task's files")
	}
	a.write("reset.txt", "reset\n")
	a.ok("label", "reset")
	a.ok("share")
	reset := a.cwd

	// More work underneath marks the top task as needing a refresh.
	a.cd(login)
	a.write("login.txt", "fixed better\n")
	a.ok("label", "better")
	a.ok("share")
	a.cd(reset)
	var st struct {
		Refresh string `json:"refresh"`
	}
	a.json(&st, "status")
	if !strings.Contains(st.Refresh, "task #2") {
		t.Fatalf("refresh reason %q", st.Refresh)
	}
	a.ok("refresh")
	if a.read("login.txt") != "fixed better\n" {
		t.Fatal("refresh didn't bring in the underlying task's change")
	}
	a.ok("share")

	// The review of the top task shows only its own changes.
	var d struct {
		Files []struct{ Path string } `json:"files"`
	}
	a.json(&d, "diff", "3")
	if len(d.Files) != 1 || d.Files[0].Path != "reset.txt" {
		t.Fatalf("stacked diff: %+v", d.Files)
	}

	// Approving the top task applies both, in order.
	var ap struct {
		Versions []struct{ Num, Task int64 } `json:"versions"`
	}
	a.json(&ap, "approve")
	if len(ap.Versions) != 2 || ap.Versions[0].Task != 2 || ap.Versions[1].Task != 3 {
		t.Fatalf("approve: %+v", ap)
	}
	a.cd(home)
	a.ok("export", filepath.Join(w.dir, "out"))
	for f, want := range map[string]string{"login.txt": "fixed better\n", "reset.txt": "reset\n"} {
		if got, _ := os.ReadFile(filepath.Join(w.dir, "out", f)); string(got) != want {
			t.Fatalf("%s = %q", f, got)
		}
	}
}

func TestCloseTaskUnderneath(t *testing.T) {
	w := newWorld(t)
	a := w.person("alice")
	a.seed(map[string]string{"f.txt": lines})
	a.newTask("Bad idea")
	a.write("bad.txt", "bad\n")
	a.ok("label", "bad")
	a.ok("share")
	a.newTask("Good idea")
	a.write("good.txt", "good\n")
	a.ok("label", "good")
	a.ok("share")
	a.fails("already applied", "approve", "1")
	a.ok("close", "2")
	a.fails("was closed", "approve")
	a.ok("refresh")
	if a.exists("bad.txt") || !a.exists("good.txt") {
		t.Fatalf("after refresh: bad=%v good=%v", a.exists("bad.txt"), a.exists("good.txt"))
	}
	a.ok("share")
	a.ok("approve")
}

func TestUndoLabelAndGoBackToSave(t *testing.T) {
	w := newWorld(t)
	a := w.person("alice")
	a.seed(map[string]string{"f.txt": "v1\n"})
	a.newTask("Edits")
	a.write("f.txt", "v2\n")
	a.ok("label", "v2")
	a.write("f.txt", "v3\n")
	a.ok("label", "--edit")
	var info []struct {
		Kind string `json:"kind"`
		ID   any    `json:"id"`
		Desc string `json:"desc"`
	}
	a.json(&info, "history")
	var firstSave any
	for _, e := range info {
		if e.Kind == "save" && e.Desc == "workspace created" {
			firstSave = e.ID
		}
	}
	// Undo the edit, then the label.
	a.ok("undo")
	a.ok("undo")
	a.fails("no change to edit", "label", "--edit")
	var st struct {
		Changes int `json:"changes"`
	}
	a.json(&st, "status")
	if st.Changes != 0 {
		t.Fatalf("changes after undoing label: %d", st.Changes)
	}
	if a.read("f.txt") != "v3\n" {
		t.Fatal("undoing a label must not change files")
	}
	// Go back to the first save.
	a.ok("undo", jsonNum(firstSave))
	if a.read("f.txt") != "v1\n" {
		t.Fatalf("after going back: %q", a.read("f.txt"))
	}
	a.ok("undo")
	if a.read("f.txt") != "v3\n" {
		t.Fatalf("after undoing the go-back: %q", a.read("f.txt"))
	}
}

func jsonNum(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestBinaryConflict(t *testing.T) {
	w := newWorld(t)
	a, b := w.person("alice"), w.person("bob")
	a.seed(map[string]string{"logo.png": "PNG\x00base"})
	bhome := b.cwd
	b.newTask("Bob logo")
	b.write("logo.png", "PNG\x00bob")
	b.ok("label", "bob logo")
	b.cd(bhome)
	a.newTask("Alice logo")
	a.write("logo.png", "PNG\x00alice")
	a.ok("label", "alice logo")
	a.ok("share")
	a.ok("approve")

	b.cd(filepath.Join(bhome, "bob-logo"))
	b.ok("refresh")
	if b.exists("logo.png") || !b.exists("logo (task #2).png") && !b.exists("logo (task 2).png") {
		entries, _ := os.ReadDir(b.cwd)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("side-by-side files: %v", names)
	}
	os.Remove(filepath.Join(b.cwd, "logo (project 2).png"))
	var cs struct {
		Conflicts []any `json:"conflicts"`
	}
	b.json(&cs, "conflicts")
	if len(cs.Conflicts) != 0 || b.read("logo.png") != "PNG\x00bob" {
		t.Fatalf("conflicts %v, logo %q", cs.Conflicts, b.read("logo.png"))
	}
}

func TestRevert(t *testing.T) {
	w := newWorld(t)
	a := w.person("alice")
	a.seed(map[string]string{"f.txt": lines})
	home := a.cwd
	a.newTask("Oops")
	a.write("f.txt", strings.Replace(lines, "two", "oops", 1))
	a.ok("label", "oops")
	a.ok("share")
	a.ok("approve")
	a.cd(home)
	var r struct {
		ID int64 `json:"id"`
	}
	a.json(&r, "revert", "2")
	a.ok("approve", jsonNum(r.ID))
	a.ok("export", filepath.Join(w.dir, "out"))
	if got, _ := os.ReadFile(filepath.Join(w.dir, "out", "f.txt")); string(got) != lines {
		t.Fatalf("after revert: %q", got)
	}
}

func TestErrorsNameTheFix(t *testing.T) {
	w := newWorld(t)
	a := w.person("alice")
	a.fails("not in a workspace", "status")
	a.newTask("Something")
	a.fails("ply label", "share")
	out, _ := a.run("share", "--json")
	var e struct {
		Code string `json:"code"`
		Fix  string `json:"fix"`
	}
	if err := json.Unmarshal([]byte(out), &e); err != nil || e.Fix == "" {
		t.Fatalf("json error: %v %s", err, out)
	}
	a.fails("unknown flag", "status", "--bogus")
}

func TestCopyIgnored(t *testing.T) {
	w := newWorld(t)
	a := w.person("alice")
	a.seed(map[string]string{"package.json": "{}\n"})
	a.newTask("One")
	a.write("node_modules/dep/index.js", "dep\n")
	a.newTask("Two", "--copy-ignored", "--on", "project")
	if a.read("node_modules/dep/index.js") != "dep\n" {
		t.Fatal("ignored files weren't copied")
	}
	var st struct {
		Unlabeled []any `json:"unlabeled"`
	}
	a.json(&st, "status")
	if len(st.Unlabeled) != 0 {
		t.Fatalf("ignored files show as work: %v", st.Unlabeled)
	}
}

func TestNotesAndStatuses(t *testing.T) {
	w := newWorld(t)
	a, b := w.person("alice"), w.person("bob")
	a.seed(map[string]string{"f.txt": lines})
	a.newTask("Feature")
	a.write("f.txt", "changed\n")
	a.ok("label", "change")
	a.ok("share")
	a.ok("ready")
	b.ok("note", "2", "Looks good, but rename the file")
	a.ok("note", "Will do")
	var r struct {
		Task struct {
			Status string `json:"status"`
			Notes  []struct {
				Author string `json:"author"`
				Body   string `json:"body"`
			} `json:"notes"`
		} `json:"task"`
	}
	a.json(&r, "review")
	if r.Task.Status != "ready" || len(r.Task.Notes) != 2 || r.Task.Notes[0].Author != "bob" {
		t.Fatalf("review: %+v", r.Task)
	}
	// Sharing more keeps it ready.
	a.write("f.txt", "changed again\n")
	a.ok("label", "more")
	a.ok("share")
	a.json(&r, "review")
	if r.Task.Status != "ready" {
		t.Fatalf("status after share: %s", r.Task.Status)
	}
	b.ok("close", "2")
	a.write("f.txt", "while closed\n")
	a.ok("label", "while closed")
	a.fails("closed", "share")
	b.ok("reopen", "2")
	a.ok("share")
	out := b.ok("tasks")
	if !strings.Contains(out, "Feature") {
		t.Fatalf("tasks:\n%s", out)
	}
	a.ok("approve")
	a.fails("already applied", "close")
	a.ok("remove")
	if _, err := os.Stat(a.cwd); err == nil {
		t.Fatal("workspace folder still exists")
	}
}
