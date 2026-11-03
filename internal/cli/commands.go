package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/blatessa/ply/internal/client"
	"github.com/blatessa/ply/internal/local"
	"github.com/blatessa/ply/internal/plyerr"
	"github.com/blatessa/ply/internal/project"
	"github.com/blatessa/ply/internal/server"
	"github.com/blatessa/ply/internal/store"
	"github.com/blatessa/ply/internal/workspace"
)

func (c *CLI) env() (*local.Env, error) { return local.NewEnv(c.Dir) }

func (c *CLI) ws() (*local.WS, error) {
	e, err := c.env()
	if err != nil {
		return nil, err
	}
	w, err := e.Workspace()
	if err == nil {
		local.EnsureWatcher()
	}
	return w, err
}

// taskOrCurrent resolves a task argument, defaulting to the workspace's task.
func (c *CLI) taskOrCurrent(a *Args, i int) (int64, error) {
	if ref := a.Arg(i); ref != "" {
		return project.ParseTask(ref)
	}
	w, err := c.ws()
	if err != nil {
		return 0, plyerr.New(plyerr.Invalid, "", "name a task, or run this in a folder showing it")
	}
	defer w.Close()
	if w.Task == 0 {
		return 0, plyerr.New(plyerr.Invalid, "ply tasks", "this folder is showing the project, not a task; name a task")
	}
	return w.Task, nil
}

func (c *CLI) proj(a *Args) (*local.Proj, error) {
	e, err := c.env()
	if err != nil {
		return nil, err
	}
	return e.Project(a.Str("project"))
}

func init() {
	register(&command{
		name: "join", usage: "join <url> [--dir <folder>]", minPos: 1, maxPos: 1, valued: []string{"dir"},
		summary: "Connect to a project on a server and make its folder (--dir; default: a folder named after the project, here).\nThe folder shows the project; `ply new` starts a task in it.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			e, err := c.env()
			if err != nil {
				return nil, err
			}
			url := strings.TrimRight(a.Arg(0), "/")
			dir := a.Str("dir")
			if dir == "" {
				dir = filepath.Join(c.Dir, filepath.Base(url))
			}
			dir, _ = filepath.Abs(dir)
			p, err := e.Join(ctx, url, dir)
			if err != nil {
				return nil, err
			}
			local.EnsureWatcher()
			c.say("Joined %s as %s.\nThe project's folder is %s. cd there and start a task with `ply new \"Title\"`.", p.URL, e.Cfg.User, p.Dir)
			return p, nil
		},
	})
	register(&command{
		name: "start", usage: "start <url>", minPos: 1, maxPos: 1,
		summary: "Create a project on the server from this folder. Its files become a first task, \"Initial files\",\nlabeled and shared; approving it gives the project its starting files. This folder becomes the project's folder.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			e, err := c.env()
			if err != nil {
				return nil, err
			}
			r, err := e.Start(ctx, strings.TrimRight(a.Arg(0), "/"))
			if err != nil {
				return nil, err
			}
			local.EnsureWatcher()
			c.say("Created project %s with task #%d %q (%d files), shared for review.", r.Project.URL, r.Task.ID, r.Task.Title, r.Files)
			c.say("This folder is now the project's folder, showing task #%d. Run `ply approve` to make these the project's starting files.", r.Task.ID)
			return r, nil
		},
	})
	register(&command{
		name: "new", usage: `new "<title>" [--on [<task>]] [--description <text>] [--separate | --here] [--path <folder>]`,
		minPos: 1, maxPos: 1, valued: []string{"description", "path"}, bools: []string{"separate", "here"}, optional: []string{"on"},
		summary: "Create a task and show it in this folder. It builds on the project; --on builds on the task this folder\nshows, --on <task> on any task. The task you leave keeps its unlabeled work; edits made while the folder\nshowed the project come along. --separate (or --path) makes an extra folder for it instead.\nRun by an agent (PLY_AGENT or CLAUDECODE set) in the project's main folder, it makes an extra folder\nunless given --here, so it doesn't change files under a person.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			e, err := c.env()
			if err != nil {
				return nil, err
			}
			r, err := e.New(ctx, local.NewOptions{Title: a.Arg(0), Description: a.Str("description"), On: a.Str("on"),
				Separate: a.Bool("separate"), Here: a.Bool("here"), Path: a.Str("path"), Project: a.Str("project")})
			if err != nil {
				return nil, err
			}
			local.EnsureWatcher()
			c.say("Created task #%d %q, built on %s.", r.Task.ID, r.Task.Title, r.BuiltOn)
			sayFolder(c, r.Path, r.Separate, r.Rewrote)
			if r.Carried > 0 {
				c.say("  Your edits to %d files came along as unlabeled work.", r.Carried)
			}
			if len(r.Conflicts) > 0 {
				c.say("  %d of them conflict with %s:", len(r.Conflicts), r.BuiltOn)
				printConflicts(c, r.Conflicts)
			}
			if r.Warning != "" {
				c.say("Note: %s", r.Warning)
			}
			return r, nil
		},
	})
	register(&command{
		name: "go", usage: "go <task>|project [--separate | --here] [--path <folder>]", minPos: 1, maxPos: 1,
		valued: []string{"path"}, bools: []string{"separate", "here"},
		summary: "Show another task in this folder: yours or anyone's (or `project` for the project itself).\nThe task you leave keeps its unlabeled work, and coming back restores it. Ignored files stay put.\n--separate (or --path) makes an extra folder for the task instead, copying ignored files over.\nAgents get an extra folder by default when run in the project's main folder (see `ply help new`).",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			e, err := c.env()
			if err != nil {
				return nil, err
			}
			r, err := e.Go(ctx, a.Arg(0), local.GoOptions{Separate: a.Bool("separate"), Here: a.Bool("here"), Path: a.Str("path"), Project: a.Str("project")})
			if err != nil {
				return nil, err
			}
			local.EnsureWatcher()
			if r.Task == nil {
				c.say("Showing the project (#%d).", r.Project)
			} else {
				status := ""
				if r.Task.Status != store.StatusOpen {
					status = fmt.Sprintf(" (%s)", r.Task.Status)
				}
				c.say("Showing task #%d %q%s, built on %s.", r.Task.ID, r.Task.Title, status, r.BuiltOn)
			}
			if r.Restored {
				c.say("  Your work on it is back as you left it.")
			}
			sayFolder(c, r.Path, r.Separate, r.Rewrote)
			return r, nil
		},
	})
	register(&command{
		name: "ship", usage: `ship "<title>" [<file>...]`, minPos: 1, maxPos: -1,
		summary: "Ship a small change straight to the project: wrap unlabeled work (all of it, or the files named)\ninto its own task and apply it, if the project lets you approve your own tasks; otherwise it's\nshared and marked ready for review. Your files aren't changed.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			w, err := c.ws()
			if err != nil {
				return nil, err
			}
			defer w.Close()
			r, err := w.Ship(ctx, a.Arg(0), a.Pos[1:])
			if err != nil {
				return nil, err
			}
			if r.Applied != nil {
				c.say("Shipped task #%d %q (%d files) as project #%d.", r.Task.ID, r.Task.Title, len(r.Files), r.Applied.Num)
			} else {
				c.say("Shared task #%d %q (%d files) and marked it ready for review: %s.", r.Task.ID, r.Task.Title, len(r.Files), r.NotApproved)
			}
			if r.LeftInTask != 0 {
				c.say("  The edits are still unlabeled work on task #%d here; `ply refresh` drops them once they're in the project.", r.LeftInTask)
			}
			return r, nil
		},
	})
	register(&command{
		name: "status", usage: "status", maxPos: 0,
		summary: "Show unlabeled work, conflicts, what the task is built on, and whether it needs a refresh.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			w, err := c.ws()
			if err != nil {
				return nil, err
			}
			defer w.Close()
			st, err := w.Status(ctx)
			if err != nil {
				return nil, err
			}
			if st.Task.ID == 0 {
				c.say("Showing the project (#%d). Start a task with `ply new \"Title\"`.", st.Project)
			} else {
				status := st.Task.Status
				if st.ServerStatus != "" {
					status = st.ServerStatus
				}
				c.say("Task #%d %q (%s), built on %s", st.Task.ID, st.Task.Title, status, st.BuiltOn)
			}
			if st.Server != "" {
				c.say("  Couldn't check the server: %s", st.Server)
			}
			if st.Refresh != "" {
				c.say("  Needs a refresh: %s. Run `ply refresh`.", st.Refresh)
			}
			if len(st.Conflicts) > 0 {
				c.say("  %d unresolved conflicts (see `ply conflicts`).", len(st.Conflicts))
			}
			if st.Task.ID != 0 {
				c.say("  %d labeled changes, %d not shared", st.Changes, st.Unshared)
			}
			switch {
			case len(st.Unlabeled) == 0 && st.Task.ID == 0:
				c.say("  No edits.")
			case len(st.Unlabeled) == 0:
				c.say("  No unlabeled work.")
			default:
				if st.Task.ID == 0 {
					c.say("  Edits (they come along into your next `ply new`):")
				} else {
					c.say("  Unlabeled work:")
				}
				for _, d := range st.Unlabeled {
					c.say("    %-8s %s", d.Kind, d.Path)
				}
			}
			sayStale(c, st.Stale)
			if len(st.Folders) > 0 {
				c.say("  Other folders:")
				for _, f := range st.Folders {
					what := "the project"
					if f.Task != 0 {
						what = fmt.Sprintf("task #%d", f.Task)
					}
					c.say("    %s (%s)", f.Path, what)
				}
			}
			return st, nil
		},
	})
	register(&command{
		name: "diff", usage: "diff [<task>]", maxPos: 1,
		summary: "Show unlabeled work in this workspace, or a task's changes (what reviewers see).",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			var r *local.DiffResult
			if a.Arg(0) != "" {
				id, err := project.ParseTask(a.Arg(0))
				if err != nil {
					return nil, err
				}
				p, err := c.proj(a)
				if err != nil {
					return nil, err
				}
				defer p.Close()
				if r, err = p.TaskDiff(ctx, id); err != nil {
					return nil, err
				}
			} else {
				w, err := c.ws()
				if err != nil {
					return nil, err
				}
				defer w.Close()
				if r, err = w.Diff(ctx); err != nil {
					return nil, err
				}
			}
			if !c.json {
				if r.Text == "" {
					c.say("No differences between %s and %s.", r.From, r.To)
				} else {
					fmt.Fprint(c.Out, r.Text)
				}
			}
			return r, nil
		},
	})
	register(&command{
		name: "label", usage: `label "<what changed>" | label --edit ["<new label>"]`, maxPos: 1, bools: []string{"edit"},
		summary: "Label the work since the last label as a change.\n--edit folds new work into the latest change instead (until the task is approved).",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			w, err := c.ws()
			if err != nil {
				return nil, err
			}
			defer w.Close()
			ch, err := w.Label(ctx, a.Arg(0), a.Bool("edit"))
			if err != nil {
				return nil, err
			}
			if a.Bool("edit") {
				c.say("Updated change %q. Run `ply share` to share it.", ch.Label)
			} else {
				c.say("Labeled change %q. Run `ply share` to share it.", ch.Label)
			}
			if stale, _ := w.StaleWrites(ctx); len(stale) > 0 {
				sayStale(c, stale)
			}
			return ch, nil
		},
	})
	register(&command{
		name: "history", usage: "history [--limit <n>]", maxPos: 0, valued: []string{"limit"},
		summary: "Show this workspace's timeline: saves, labeled changes and actions.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			w, err := c.ws()
			if err != nil {
				return nil, err
			}
			defer w.Close()
			limit := 30
			if s := a.Str("limit"); s != "" {
				if limit, err = strconv.Atoi(s); err != nil {
					return nil, plyerr.New(plyerr.Invalid, "", "--limit needs a number")
				}
			}
			evs, err := w.History(ctx, limit)
			if err != nil {
				return nil, err
			}
			for _, e := range evs {
				when := e.At.Local().Format("Jan 2 15:04:05")
				switch e.Kind {
				case "save":
					c.say("%s  save %-6v %s", when, e.ID, e.Desc)
				case "change":
					c.say("%s  change      %s", when, e.Desc)
				default:
					undone := ""
					if e.Undone {
						undone = " (undone)"
					}
					c.say("%s  %-11s %s%s", when, e.Kind, e.Desc, undone)
				}
			}
			c.say("\nGo back to a save with `ply undo <save>`.")
			return evs, nil
		},
	})
	register(&command{
		name: "undo", usage: "undo [<save>]", maxPos: 1,
		summary: "Undo the last action in this workspace (run again to go further back).\nGiven a save from `ply history`, go back to that save.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			w, err := c.ws()
			if err != nil {
				return nil, err
			}
			defer w.Close()
			var save int64
			if s := a.Arg(0); s != "" {
				if save, err = strconv.ParseInt(s, 10, 64); err != nil {
					return nil, plyerr.New(plyerr.Invalid, "ply history", "%q isn't a save number", s)
				}
			}
			r, err := w.Undo(ctx, save)
			if err != nil {
				return nil, err
			}
			c.say("%s. (`ply redo` reverses this.)", capitalize(r.Did))
			for _, n := range r.Notices {
				c.say("  %s", n)
			}
			return r, nil
		},
	})
	register(&command{
		name: "redo", usage: "redo", maxPos: 0,
		summary: "Reverse the last undo.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			w, err := c.ws()
			if err != nil {
				return nil, err
			}
			defer w.Close()
			r, err := w.Redo(ctx)
			if err != nil {
				return nil, err
			}
			c.say("%s.", capitalize(r.Did))
			return r, nil
		},
	})
	register(&command{
		name: "share", usage: "share", maxPos: 0,
		summary: "Share this task's labeled changes with the server so others can see and review them.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			w, err := c.ws()
			if err != nil {
				return nil, err
			}
			defer w.Close()
			r, err := w.Share(ctx)
			if err != nil {
				return nil, err
			}
			c.say("Shared %d changes to task #%d.", r.Shared, r.Task.ID)
			if stale, _ := w.StaleWrites(ctx); len(stale) > 0 {
				sayStale(c, stale)
			}
			if r.Unlabeled > 0 {
				c.say("  %d files have unlabeled work, which wasn't shared.", r.Unlabeled)
			}
			if len(r.Task.Conflicts) > 0 {
				c.say("  It has %d unresolved conflicts, so it can't be approved yet.", len(r.Task.Conflicts))
			}
			return r, nil
		},
	})
	register(&command{
		name: "refresh", usage: "refresh", maxPos: 0,
		summary: "Bring this workspace up to date with what its task is built on, and with changes others shared to it.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			w, err := c.ws()
			if err != nil {
				return nil, err
			}
			defer w.Close()
			r, err := w.Refresh(ctx)
			if err != nil {
				return nil, err
			}
			if r.UpToDate {
				c.say("Already up to date with %s.", r.BuiltOn)
				return r, nil
			}
			c.say("%s.", capitalize(joinSteps(r.Steps)))
			for _, n := range r.Notices {
				c.say("  %s", n)
			}
			if len(r.Conflicts) > 0 {
				c.say("%d conflicts to resolve:", len(r.Conflicts))
				printConflicts(c, r.Conflicts)
			}
			c.say("`ply undo` reverses this refresh.")
			return r, nil
		},
	})
	register(&command{
		name: "conflicts", usage: "conflicts", maxPos: 0,
		summary: "List unresolved conflicts in this workspace.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			w, err := c.ws()
			if err != nil {
				return nil, err
			}
			defer w.Close()
			cs, err := w.ConflictList(ctx)
			if err != nil {
				return nil, err
			}
			if len(cs) == 0 {
				c.say("No conflicts.")
			} else {
				printConflicts(c, cs)
			}
			if cs == nil {
				cs = []store.Conflict{}
			}
			return map[string]any{"conflicts": cs}, nil
		},
	})
	register(&command{
		name: "tasks", usage: "tasks [--all]", maxPos: 0, bools: []string{"all"},
		summary: "List tasks, who's on them, and their status. --all includes applied and closed tasks.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			p, err := c.proj(a)
			if err != nil {
				return nil, err
			}
			defer p.Close()
			st, err := p.Client.State(ctx, a.Bool("all"))
			if err != nil {
				return nil, err
			}
			c.say("Project %s is at #%d.", st.Name, st.Head.Num)
			if len(st.Tasks) == 0 {
				c.say("No open tasks. Start one with `ply new \"Title\"`.")
			}
			for _, t := range st.Tasks {
				on := ""
				if t.BaseTask != 0 {
					on = fmt.Sprintf(", on #%d", t.BaseTask)
				}
				extra := ""
				if t.Refresh != "" {
					extra = " (needs refresh)"
				}
				if len(t.Conflicts) > 0 {
					extra += " (conflicts)"
				}
				c.say("  #%-4d %-7s %-40s %s%s%s", t.ID, t.Status, t.Title, t.Assignee, on, extra)
			}
			return st, nil
		},
	})
	register(&command{
		name: "review", usage: "review [<task>]", maxPos: 1,
		summary: "Show a task's changes and notes.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			id, err := c.taskOrCurrent(a, 0)
			if err != nil {
				return nil, err
			}
			p, err := c.proj(a)
			if err != nil {
				return nil, err
			}
			defer p.Close()
			info, err := p.Client.Task(ctx, id)
			if err != nil {
				return nil, err
			}
			d, err := p.TaskDiff(ctx, id)
			if err != nil {
				return nil, err
			}
			c.say("Task #%d %q (%s), by %s, built on %s", info.ID, info.Title, info.Status, info.Owner, builtOnInfo(info))
			if info.Description != "" {
				c.say("\n%s", info.Description)
			}
			c.say("\nChanges:")
			for _, ch := range info.Changes {
				c.say("  %s  %s", ch.Author, ch.Label)
			}
			if len(info.Conflicts) > 0 {
				c.say("\nUnresolved conflicts:")
				printConflicts(c, info.Conflicts)
			}
			if !c.json {
				fmt.Fprintf(c.Out, "\n%s", d.Text)
			}
			if len(info.Notes) > 0 {
				c.say("\nNotes:")
				for _, n := range info.Notes {
					c.say("  %s (%s): %s", n.Author, n.CreatedAt.Local().Format("Jan 2 15:04"), n.Body)
				}
			}
			return map[string]any{"task": info, "diff": d}, nil
		},
	})
	for _, st := range []struct{ name, action, summary, done string }{
		{"ready", "ready", "Mark a task ready for review (defaults to this workspace's task).", "is ready for review"},
		{"close", "close", "Close a task without applying it. `ply reopen` brings it back.", "is closed"},
		{"reopen", "reopen", "Reopen a closed task.", "is open again"},
	} {
		register(&command{
			name: st.name, usage: st.name + " [<task>]", maxPos: 1, summary: st.summary,
			run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
				id, err := c.taskOrCurrent(a, 0)
				if err != nil {
					return nil, err
				}
				p, err := c.proj(a)
				if err != nil {
					return nil, err
				}
				defer p.Close()
				info, err := p.Client.SetStatus(ctx, id, st.action)
				if err != nil {
					return nil, err
				}
				c.say("Task #%d %s.", id, st.done)
				return info, nil
			},
		})
	}
	register(&command{
		name: "approve", usage: "approve [<task>]", maxPos: 1,
		summary: "Apply a task (and any tasks it's built on) to the project.\nCombines with newer project work itself; refuses only on a real conflict.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			id, err := c.taskOrCurrent(a, 0)
			if err != nil {
				return nil, err
			}
			e, err := c.env()
			if err != nil {
				return nil, err
			}
			r, err := e.Approve(ctx, strconv.FormatInt(id, 10), a.Str("project"))
			if err != nil {
				return nil, err
			}
			for _, v := range r.Versions {
				c.say("Applied task #%d as project #%d.", v.Task, v.Num)
			}
			if r.ShowsProject != 0 {
				c.say("This folder now shows the project (#%d).", r.ShowsProject)
			}
			return r, nil
		},
	})
	register(&command{
		name: "note", usage: `note [<task>] "<note>"`, minPos: 1, maxPos: 2,
		summary: "Add a note to a task (defaults to this workspace's task).",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			body := a.Arg(len(a.Pos) - 1)
			var id int64
			var err error
			if len(a.Pos) == 2 {
				id, err = project.ParseTask(a.Arg(0))
			} else {
				id, err = c.taskOrCurrent(&Args{}, 0)
			}
			if err != nil {
				return nil, err
			}
			p, err := c.proj(a)
			if err != nil {
				return nil, err
			}
			defer p.Close()
			n, err := p.Client.AddNote(ctx, id, body)
			if err != nil {
				return nil, err
			}
			c.say("Added a note to task #%d.", id)
			return n, nil
		},
	})
	register(&command{
		name: "revert", usage: "revert <task>", minPos: 1, maxPos: 1,
		summary: "Create a new task that undoes an applied task. It goes through review like any task.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			id, err := project.ParseTask(a.Arg(0))
			if err != nil {
				return nil, err
			}
			p, err := c.proj(a)
			if err != nil {
				return nil, err
			}
			defer p.Close()
			info, err := p.Client.Revert(ctx, id)
			if err != nil {
				return nil, err
			}
			c.say("Created task #%d %q. Review it with `ply review %d`, or work on it with `ply go %d`.", info.ID, info.Title, info.ID, info.ID)
			if len(info.Conflicts) > 0 {
				c.say("  It conflicts with later work in %d files; resolve them in its workspace.", len(info.Conflicts))
			}
			return info, nil
		},
	})
	register(&command{
		name: "remove", usage: "remove [--force]", maxPos: 0, bools: []string{"force"},
		summary: "Remove this extra folder (one made with --separate). The task's work stays on this machine;\n`ply go <task>` in another folder picks it up again. The project's main folder can't be removed.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			w, err := c.ws()
			if err != nil {
				return nil, err
			}
			defer w.Close()
			if err := w.Remove(ctx, a.Bool("force")); err != nil {
				return nil, err
			}
			c.say("Removed the folder %s.", w.Root)
			return map[string]any{"removed": w.Root}, nil
		},
	})
	register(&command{
		name: "export", usage: "export <folder> [--at <task>] [--task <task>] [--no-info]", minPos: 1, maxPos: 1,
		valued: []string{"at", "task"}, bools: []string{"no-info"},
		summary: "Write the project's files as a plain, untracked folder.\n--at <task>: as of an applied task. --task <task>: an open task's latest version.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			e, err := c.env()
			if err != nil {
				return nil, err
			}
			dir, _ := filepath.Abs(a.Arg(0))
			desc, err := e.Export(ctx, local.ExportOptions{Dir: dir, At: a.Str("at"), Task: a.Str("task"), NoInfo: a.Bool("no-info"), Project: a.Str("project")})
			if err != nil {
				return nil, err
			}
			c.say("Exported %s to %s.", desc, dir)
			return map[string]string{"folder": dir, "contents": desc}, nil
		},
	})
	register(&command{
		name: "log", usage: "log", maxPos: 0,
		summary: "The project's history: applied tasks, newest first.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			p, err := c.proj(a)
			if err != nil {
				return nil, err
			}
			defer p.Close()
			vs, err := p.Client.Log(ctx)
			if err != nil {
				return nil, err
			}
			st, err := p.Client.State(ctx, true)
			if err != nil {
				return nil, err
			}
			titles := map[int64]string{}
			for _, t := range st.Tasks {
				titles[t.ID] = t.Title
			}
			if len(vs) == 0 {
				c.say("Nothing applied yet.")
			}
			for _, v := range vs {
				c.say("#%-4d %s  task #%d %q, approved by %s", v.Num, v.AppliedAt.Local().Format("Jan 2 15:04"), v.Task, titles[v.Task], v.AppliedBy)
			}
			return map[string]any{"versions": vs}, nil
		},
	})
	register(&command{
		name: "setting", usage: "setting [<name> <value>]", maxPos: 2,
		summary: "Show the project's settings, or change one.\napprove: who can approve a task, `anyone` (default) or `others` (anyone but its owner).",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			if len(a.Pos) == 1 {
				return nil, plyerr.New(plyerr.Invalid, "ply help setting", "give a setting and its value")
			}
			p, err := c.proj(a)
			if err != nil {
				return nil, err
			}
			defer p.Close()
			var set map[string]string
			if len(a.Pos) == 2 {
				set, err = p.Client.SetSetting(ctx, a.Arg(0), a.Arg(1))
			} else {
				set, err = p.Client.Settings(ctx)
			}
			if err != nil {
				return nil, err
			}
			keys := make([]string, 0, len(set))
			for k := range set {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				c.say("%s = %s", k, set[k])
			}
			return set, nil
		},
	})
	register(&command{
		name: "skill", usage: "skill", maxPos: 0,
		summary: "Print the instructions an agent loads to learn Ply.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			if !c.json {
				fmt.Fprint(c.Out, Skill)
			}
			return map[string]string{"skill": Skill}, nil
		},
	})
	register(&command{
		name: "watch", usage: "watch", maxPos: 0,
		summary: "Record saves in every workspace as files change. Commands start this in the background\nautomatically; run it yourself only to see what it's doing. PLY_NO_WATCH=1 turns it off.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			return nil, local.Watch(ctx, log.New(c.Err, "ply watch: ", log.LstdFlags))
		},
	})
	register(&command{
		name: "server start", usage: "server start [--addr <host:port>] [--dir <folder>]", maxPos: 0,
		valued:  []string{"addr", "dir"},
		summary: "Run a Ply server on this machine (default http://localhost:7070). No setup needed.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			addr := a.Str("addr")
			if addr == "" {
				addr = "localhost:7070"
			}
			dir := a.Str("dir")
			if dir == "" {
				dir = serverDir()
			}
			s, err := server.New(dir)
			if err != nil {
				return nil, err
			}
			defer s.Close()
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			c.say("Ply server running at http://%s (data in %s). Ctrl-C to stop.", addr, dir)
			c.say("Create a project with `ply server new <name>`, then `ply join http://%s/<name>`.", addr)
			return nil, s.ListenAndServe(ctx, addr)
		},
	})
	register(&command{
		name: "server new", usage: "server new <project> [--server <url>]", minPos: 1, maxPos: 1, valued: []string{"server"},
		summary: "Create a new, empty project on the server.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			e, err := c.env()
			if err != nil {
				return nil, err
			}
			url := a.Str("server")
			if url == "" {
				url = "http://localhost:7070"
			}
			if err := client.CreateProject(ctx, url, e.Cfg.User, a.Arg(0)); err != nil {
				return nil, err
			}
			c.say("Created project %s. Join it with `ply join %s/%s`.", a.Arg(0), url, a.Arg(0))
			return map[string]string{"project": a.Arg(0), "url": url + "/" + a.Arg(0)}, nil
		},
	})
}

func serverDir() string {
	if d := os.Getenv("PLY_SERVER_DIR"); d != "" {
		return d
	}
	return filepath.Join(workspace.Home(), "server")
}

func builtOnInfo(t *project.TaskInfo) string {
	if t.BaseTask != 0 {
		return fmt.Sprintf("task #%d", t.BaseTask)
	}
	return fmt.Sprintf("project #%d", t.BaseVersion)
}

// sayFolder reports where a task is shown and reminds people to reload
// files their editor has open.
func sayFolder(c *CLI, path, separate string, rewrote int) {
	if separate != "" {
		c.say("Made a separate folder: %s.", separate)
	}
	if path != c.Dir {
		c.say("Folder: %s", path)
	} else if rewrote > 0 {
		c.say("  Changed %d files here; reload any your editor has open, or it may save the old versions back.", rewrote)
	}
}

// sayStale warns about files that look like an editor saved them from
// before the last switch.
func sayStale(c *CLI, stale []local.StaleWrite) {
	if len(stale) == 0 {
		return
	}
	c.say("  Warning: these files were saved with the version from before the last switch, probably by an editor\n  that still had them open. Reload them in your editor; if the old version shouldn't be here, put\n  back the earlier save with `ply undo <save>` (see `ply history`):")
	for _, s := range stale {
		from := "the project"
		if s.From != 0 {
			from = fmt.Sprintf("task #%d", s.From)
		}
		c.say("    %s (%s's version; save before it: %d)", s.Path, from, s.Save)
	}
}

func printConflicts(c *CLI, cs []store.Conflict) {
	for _, x := range cs {
		if x.Kind == "binary" {
			c.say("  %s: both versions kept as %q and %q; delete the one you don't want", x.Path, x.Ours, x.Theirs)
		} else {
			c.say("  %s: edit the file to resolve the marked sections", x.Path)
		}
	}
}

func joinSteps(s []string) string { return strings.Join(s, ", then ") }

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
