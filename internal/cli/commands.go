package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
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
		return 0, plyerr.New(plyerr.Invalid, "", "name a task, or run this inside its workspace")
	}
	defer w.Close()
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
		summary: "Connect to a project on a server.\nWorkspaces are created in --dir (default: a folder named after the project, here).",
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
			c.say("Joined %s as %s.\nWorkspaces go in %s. Start a task with `ply new \"Title\"`.", p.URL, e.Cfg.User, p.Dir)
			return p, nil
		},
	})
	register(&command{
		name: "new", usage: `new "<title>" [--on project|<task>] [--description <text>] [--copy-ignored] [--path <folder>]`,
		minPos: 1, maxPos: 1, valued: []string{"on", "description", "path"}, bools: []string{"copy-ignored"},
		summary: "Create a task and its workspace.\nInside a workspace, the new task builds on that workspace's task; elsewhere, on the project.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			e, err := c.env()
			if err != nil {
				return nil, err
			}
			r, err := e.New(ctx, local.NewOptions{Title: a.Arg(0), Description: a.Str("description"), On: a.Str("on"),
				CopyIgnored: a.Bool("copy-ignored"), Path: a.Str("path"), Project: a.Str("project")})
			if err != nil {
				return nil, err
			}
			local.EnsureWatcher()
			c.say("Created task #%d %q, built on %s.\nWorkspace: %s", r.Task.ID, r.Task.Title, r.BuiltOn, r.Path)
			if r.Warning != "" {
				c.say("Note: %s", r.Warning)
			}
			return r, nil
		},
	})
	register(&command{
		name: "get", usage: "get <task> [--path <folder>]", minPos: 1, maxPos: 1, valued: []string{"path"},
		summary: "Make a workspace for an existing task, to review it closely or take it over.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			e, err := c.env()
			if err != nil {
				return nil, err
			}
			r, err := e.Get(ctx, a.Arg(0), a.Str("path"), a.Str("project"))
			if err != nil {
				return nil, err
			}
			local.EnsureWatcher()
			c.say("Task #%d %q (%s), built on %s.\nWorkspace: %s", r.Task.ID, r.Task.Title, r.Task.Status, r.BuiltOn, r.Path)
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
			status := st.Task.Status
			if st.ServerStatus != "" {
				status = st.ServerStatus
			}
			c.say("Task #%d %q (%s), built on %s", st.Task.ID, st.Task.Title, status, st.BuiltOn)
			if st.Server != "" {
				c.say("  Couldn't check the server: %s", st.Server)
			}
			if st.Refresh != "" {
				c.say("  Needs a refresh: %s. Run `ply refresh`.", st.Refresh)
			}
			if len(st.Conflicts) > 0 {
				c.say("  %d unresolved conflicts (see `ply conflicts`).", len(st.Conflicts))
			}
			c.say("  %d labeled changes, %d not shared", st.Changes, st.Unshared)
			if len(st.Unlabeled) == 0 {
				c.say("  No unlabeled work.")
			} else {
				c.say("  Unlabeled work:")
				for _, d := range st.Unlabeled {
					c.say("    %-8s %s", d.Kind, d.Path)
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
			vs, err := e.Approve(ctx, strconv.FormatInt(id, 10), a.Str("project"))
			if err != nil {
				return nil, err
			}
			for _, v := range vs {
				c.say("Applied task #%d as project #%d.", v.Task, v.Num)
			}
			return map[string]any{"versions": vs}, nil
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
			c.say("Created task #%d %q. Review it with `ply review %d`, or work on it with `ply get %d`.", info.ID, info.Title, info.ID, info.ID)
			if len(info.Conflicts) > 0 {
				c.say("  It conflicts with later work in %d files; resolve them in its workspace.", len(info.Conflicts))
			}
			return info, nil
		},
	})
	register(&command{
		name: "remove", usage: "remove [--force]", maxPos: 0, bools: []string{"force"},
		summary: "Remove this task's workspace and its saves. Labeled changes that were shared stay with the task.",
		run: func(ctx context.Context, c *CLI, a *Args) (any, error) {
			w, err := c.ws()
			if err != nil {
				return nil, err
			}
			defer w.Close()
			if err := w.Remove(ctx, a.Bool("force")); err != nil {
				return nil, err
			}
			c.say("Removed the workspace for task #%d (%s).", w.Task, w.Root)
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
