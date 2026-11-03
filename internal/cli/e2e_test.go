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
	w     *world
	name  string
	cwd   string
	agent string // PLY_AGENT
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
	os.Setenv("PLY_AGENT", p.agent)
	os.Setenv("CLAUDECODE", "")
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

	b.ok("go", "2")
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

	// New task inside a workspace builds on that task.
	var r struct {
		Path    string `json:"path"`
		BuiltOn string `json:"built_on"`
	}
	a.json(&r, "new", "Add password reset", "--on")
	if r.BuiltOn != "task #2" {
		t.Fatalf("built on %q", r.BuiltOn)
	}
	if a.read("login.txt") != "fixed\n" {
		t.Fatal("stacked task lacks the underlying task's files")
	}
	a.write("reset.txt", "reset\n")
	a.ok("label", "reset")
	a.ok("share")
	// More work underneath marks the top task as needing a refresh.
	a.ok("go", "2")
	a.write("login.txt", "fixed better\n")
	a.ok("label", "better")
	a.ok("share")
	a.ok("go", "3")
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
	a.newTask("Good idea", "--on")
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
		if e.Kind == "save" && e.Desc == "first save" {
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
	a.cd("..")
	a.fails("not in a project folder", "status")
	a.cd("demo")
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

func TestSeparateFolderCopiesIgnored(t *testing.T) {
	w := newWorld(t)
	a := w.person("alice")
	a.seed(map[string]string{"package.json": "{}\n"})
	main := a.cwd
	a.newTask("One")
	a.write("node_modules/dep/index.js", "dep\n")
	a.write("one.txt", "one\n")
	a.newTask("Two", "--separate")
	if a.cwd == main {
		t.Fatal("--separate didn't make a new folder")
	}
	if a.read("node_modules/dep/index.js") != "dep\n" {
		t.Fatal("ignored files weren't copied")
	}
	if a.exists("one.txt") {
		t.Fatal("task One's work showed up in task Two's folder")
	}
	var st struct {
		Unlabeled []any `json:"unlabeled"`
		Folders   []struct {
			Path string `json:"path"`
			Task int64  `json:"task"`
		} `json:"other_folders"`
	}
	a.json(&st, "status")
	if len(st.Unlabeled) != 0 {
		t.Fatalf("ignored files show as work: %v", st.Unlabeled)
	}
	if len(st.Folders) != 1 || st.Folders[0].Path != main || st.Folders[0].Task != 2 {
		t.Fatalf("other folders: %+v", st.Folders)
	}
	// A task can only show in one folder at a time.
	a.fails("showing in another folder", "go", "2")
	// Removing the extra folder keeps the task's work: show it in the main
	// folder.
	a.write("two.txt", "two\n")
	extra := a.cwd
	a.ok("remove")
	if _, err := os.Stat(extra); err == nil {
		t.Fatal("folder still exists")
	}
	a.cd(main)
	a.fails("main folder", "remove")
	a.ok("go", "3")
	if a.read("two.txt") != "two\n" || a.exists("one.txt") {
		t.Fatalf("after switching: two=%q one=%v", a.read("two.txt"), a.exists("one.txt"))
	}
	if a.read("node_modules/dep/index.js") != "dep\n" {
		t.Fatal("switching touched ignored files")
	}
}

func TestSwitchKeepsWork(t *testing.T) {
	w := newWorld(t)
	a, b := w.person("alice"), w.person("bob")
	a.seed(map[string]string{"f.txt": lines})
	a.newTask("First")
	a.write("f.txt", "first labeled\n")
	a.ok("label", "first")
	a.write("f.txt", "first unlabeled\n")
	a.write("new.txt", "new\n")
	a.write("node_modules/x.js", "x\n")

	// A new task starts from the project; First's work stays with First.
	a.newTask("Second")
	if a.read("f.txt") != lines || a.exists("new.txt") {
		t.Fatalf("second task shows first's work: %q new=%v", a.read("f.txt"), a.exists("new.txt"))
	}
	if a.read("node_modules/x.js") != "x\n" {
		t.Fatal("ignored files were touched")
	}
	a.write("f.txt", "second\n")

	var r struct {
		Restored bool `json:"restored_work"`
	}
	a.json(&r, "go", "2")
	if !r.Restored || a.read("f.txt") != "first unlabeled\n" || a.read("new.txt") != "new\n" {
		t.Fatalf("switch back: restored=%v f=%q", r.Restored, a.read("f.txt"))
	}
	// Undo history belongs to the task too.
	a.ok("undo")
	a.fails("no change to edit", "label", "--edit")
	a.ok("redo")
	a.ok("share")
	a.ok("go", "3")
	if a.read("f.txt") != "second\n" {
		t.Fatalf("second task's work: %q", a.read("f.txt"))
	}

	// Anyone's task: Bob looks at Alice's shared task, then goes back to
	// the project.
	b.ok("go", "2")
	if b.read("f.txt") != "first labeled\n" {
		t.Fatalf("bob sees %q", b.read("f.txt"))
	}
	var mk struct {
		Task  int64  `json:"task"`
		Title string `json:"title"`
	}
	json.Unmarshal([]byte(b.read(".ply")), &mk)
	if mk.Task != 2 || mk.Title != "First" {
		t.Fatalf(".ply marker: %+v", mk)
	}
	b.ok("go", "project")
	mk.Title = ""
	json.Unmarshal([]byte(b.read(".ply")), &mk)
	if mk.Task != 0 || mk.Title != "" {
		t.Fatalf(".ply marker on the project: %+v", mk)
	}
	if b.read("f.txt") != lines {
		t.Fatalf("project view: %q", b.read("f.txt"))
	}
	b.fails("showing the project", "label", "x")
}

func TestProjectViewEditsComeAlong(t *testing.T) {
	w := newWorld(t)
	a, b := w.person("alice"), w.person("bob")
	a.seed(map[string]string{"f.txt": lines})
	// Bob joined before the seed: his folder shows project #0.
	var st struct {
		Project   int64 `json:"project"`
		Refresh   string
		Unlabeled []struct{ Path string } `json:"unlabeled"`
	}
	b.json(&st, "status")
	if st.Refresh == "" {
		t.Fatalf("project view should need a refresh: %+v", st)
	}
	b.ok("refresh")
	if b.read("f.txt") != lines {
		t.Fatal("refresh didn't bring the project's files")
	}
	b.write("f.txt", strings.Replace(lines, "one", "bob", 1))
	b.json(&st, "status")
	if len(st.Unlabeled) != 1 {
		t.Fatalf("edits: %+v", st)
	}
	// The project moves on underneath; the edits still come along.
	a.newTask("Bottom")
	a.write("f.txt", strings.Replace(lines, "eight", "alice", 1))
	a.ok("label", "bottom")
	a.ok("share")
	a.ok("approve")

	var r struct {
		Carried int `json:"carried_files"`
	}
	b.json(&r, "new", "Bob's idea")
	if r.Carried != 1 {
		t.Fatalf("carried %d", r.Carried)
	}
	if got, want := b.read("f.txt"), strings.Replace(strings.Replace(lines, "one", "bob", 1), "eight", "alice", 1); got != want {
		t.Fatalf("carried edits:\n%s\nwant\n%s", got, want)
	}
	b.ok("label", "idea")
	b.ok("share")
	// Going back to the project: the edits moved into the task.
	b.ok("go", "project")
	if b.read("f.txt") != strings.Replace(lines, "eight", "alice", 1) {
		t.Fatalf("project view after new: %q", b.read("f.txt"))
	}
}

func TestStart(t *testing.T) {
	w := newWorld(t)
	a := &person{w: w, name: "alice", cwd: filepath.Join(w.dir, "my-app")}
	a.write("main.go", "package main\n")
	a.write("docs/readme.md", "hi\n")
	a.write("node_modules/dep.js", "ignored\n")
	var r struct {
		Task struct {
			ID     int64  `json:"id"`
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"task"`
		Files int `json:"files"`
	}
	a.json(&r, "start", w.url+"/my-app")
	if r.Files != 2 || r.Task.Title != "Initial files" {
		t.Fatalf("start: %+v", r)
	}
	a.fails("already a Ply folder", "start", w.url+"/other")
	a.ok("approve")
	// Someone else joins and gets the files.
	b := &person{w: w, name: "bob", cwd: filepath.Join(w.dir, "bob")}
	os.MkdirAll(b.cwd, 0o755)
	b.ok("join", w.url+"/my-app")
	b.cd("my-app")
	if b.read("main.go") != "package main\n" || b.read("docs/readme.md") != "hi\n" || b.exists("node_modules/dep.js") {
		t.Fatal("joined project lacks the starting files")
	}
	// Starting an existing project with work refuses.
	c := &person{w: w, name: "carol", cwd: filepath.Join(w.dir, "carol")}
	c.write("x", "x\n")
	c.fails("ply join", "start", w.url+"/my-app")
}

func TestShip(t *testing.T) {
	w := newWorld(t)
	a, b := w.person("alice"), w.person("bob")
	a.seed(map[string]string{"f.txt": lines, "g.txt": "g\n"})

	// From a task with its own work: ship just the typo fix.
	a.newTask("Big feature")
	a.write("feature.txt", "feature\n")
	a.ok("label", "feature")
	a.write("g.txt", "g fixed\n")
	a.write("wip.txt", "wip\n")
	var r struct {
		Task struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"task"`
		Applied *struct{ Num int64 } `json:"applied"`
	}
	a.json(&r, "ship", "Fix typo in g", "g.txt")
	if r.Applied == nil || r.Applied.Num != 2 || r.Task.Status != "applied" {
		t.Fatalf("ship: %+v", r)
	}
	a.ok("export", filepath.Join(w.dir, "out"))
	if got, _ := os.ReadFile(filepath.Join(w.dir, "out", "g.txt")); string(got) != "g fixed\n" {
		t.Fatalf("g.txt = %q", got)
	}
	for _, f := range []string{"feature.txt", "wip.txt"} {
		if _, err := os.Stat(filepath.Join(w.dir, "out", f)); err == nil {
			t.Fatalf("%s shipped too", f)
		}
	}
	// Files weren't touched; after a refresh the fix is no longer unlabeled.
	if a.read("g.txt") != "g fixed\n" || a.read("wip.txt") != "wip\n" {
		t.Fatal("ship changed files")
	}
	a.ok("refresh")
	var st struct {
		Unlabeled []struct{ Path string } `json:"unlabeled"`
	}
	a.json(&st, "status")
	if len(st.Unlabeled) != 1 || st.Unlabeled[0].Path != "wip.txt" {
		t.Fatalf("unlabeled after refresh: %+v", st.Unlabeled)
	}

	// From the project view, with self-approval turned off: left ready.
	b.ok("refresh")
	b.write("f.txt", strings.Replace(lines, "one", "1", 1))
	b.ok("setting", "approve", "others")
	r.Applied = nil
	b.json(&r, "ship", "Number one")
	if r.Applied != nil || r.Task.Status != "ready" {
		t.Fatalf("ship without self-approval: %+v", r)
	}
	b.fails("other than the task's owner", "approve", jsonNum(r.Task.ID))
	a.ok("approve", jsonNum(r.Task.ID))
	b.fails("none of those files", "ship", "Again", "g.txt")
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
	var ap struct {
		ShowsProject int64 `json:"folder_shows_project"`
	}
	a.json(&ap, "approve")
	if ap.ShowsProject != 2 {
		t.Fatalf("after approving, folder shows project %d", ap.ShowsProject)
	}
	a.fails("already applied", "close", "2")
}

func TestAgentGetsItsOwnFolder(t *testing.T) {
	w := newWorld(t)
	a := w.person("alice")
	a.seed(map[string]string{"f.txt": lines})
	main := a.cwd
	a.write("f.txt", "alice is editing\n")

	a.agent = "1"
	var r struct {
		Path     string `json:"path"`
		Separate string `json:"separate_because"`
	}
	a.json(&r, "new", "Agent work")
	if r.Path == main || !strings.Contains(r.Separate, "agent") {
		t.Fatalf("agent's task went in the main folder: %+v", r)
	}
	if got, _ := os.ReadFile(filepath.Join(main, "f.txt")); string(got) != "alice is editing\n" {
		t.Fatalf("main folder changed under alice: %q", got)
	}
	// In its own folder, the agent works in place.
	a.cd(r.Path)
	r.Separate = ""
	a.json(&r, "new", "More agent work")
	if r.Path != a.cwd || r.Separate != "" {
		t.Fatalf("agent's second task: %+v", r)
	}
	// --here uses the main folder anyway.
	a.cd(main)
	a.json(&r, "new", "Here", "--here")
	if r.Path != main {
		t.Fatalf("--here: %+v", r)
	}
}

func TestStaleEditorWrites(t *testing.T) {
	w := newWorld(t)
	a := w.person("alice")
	a.seed(map[string]string{"f.txt": lines, "g.txt": "g\n"})
	a.newTask("A")
	a.write("f.txt", "A's version\n")
	a.write("a-only.txt", "A's file\n")
	a.newTask("B")
	a.write("g.txt", "B's edit\n")
	var st struct {
		Stale []struct {
			Path string `json:"path"`
			From int64  `json:"from_task"`
		} `json:"stale_writes"`
	}
	a.json(&st, "status")
	if len(st.Stale) != 0 {
		t.Fatalf("ordinary edit flagged: %+v", st.Stale)
	}
	// An editor saves its open buffers from before the switch.
	a.write("f.txt", "A's version\n")
	a.write("a-only.txt", "A's file\n")
	a.json(&st, "status")
	if len(st.Stale) != 2 || st.Stale[0].From != 2 {
		t.Fatalf("stale writes: %+v", st.Stale)
	}
	if out := a.ok("label", "B work"); !strings.Contains(out, "Warning") || !strings.Contains(out, "f.txt") {
		t.Fatalf("label didn't warn:\n%s", out)
	}
	// Fixing the file clears it.
	a.write("f.txt", lines)
	os.Remove(filepath.Join(a.cwd, "a-only.txt"))
	st.Stale = nil
	a.json(&st, "status")
	if len(st.Stale) != 0 {
		t.Fatalf("stale writes not cleared: %+v", st.Stale)
	}
}
