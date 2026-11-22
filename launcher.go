package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Session launcher: list projects and start a fresh claude/codex tmux session
// in one, so new work can be spun up from the UI (incl. the phone) without tmux.

type projectEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// listProjects offers the git repos in the folders Claude Code has been run in:
// each recorded folder's repo, plus the other repos beside it, so a fresh clone
// in the same projects folder shows up before Claude has been run in it. With
// CLAUDE_WALL_PROJECTS_DIRS (colon-sep) set, it offers the repos directly inside
// those directories instead.
func listProjects() []projectEntry {
	var repos []string
	if env := os.Getenv("CLAUDE_WALL_PROJECTS_DIRS"); env != "" {
		repos = reposIn(strings.Split(env, ":"))
	} else {
		home, _ := os.UserHomeDir()
		home = filepath.Clean(home)
		parents := map[string]bool{}
		for _, dir := range claudeProjectDirs(home) {
			repo := repoRoot(dir, home)
			if repo == "" {
				continue
			}
			repos = append(repos, repo)
			if parent := filepath.Dir(repo); parent != home && !parents[parent] {
				parents[parent] = true
				repos = append(repos, reposIn([]string{parent})...)
			}
		}
	}
	seen := map[string]bool{}
	byName := map[string]int{}
	var unique []string
	for _, p := range repos {
		if !seen[p] {
			seen[p] = true
			unique = append(unique, p)
			byName[filepath.Base(p)]++
		}
	}
	out := make([]projectEntry, 0, len(unique))
	for _, p := range unique {
		name := filepath.Base(p)
		if byName[name] > 1 {
			name = filepath.Base(filepath.Dir(p)) + "/" + name // tells apart two "ios"
		}
		out = append(out, projectEntry{Name: name, Path: p})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// reposIn returns the immediate subdirs of roots that are git repos.
func reposIn(roots []string) []string {
	var out []string
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			p := filepath.Join(root, e.Name())
			if fi, err := os.Stat(filepath.Join(p, ".git")); err == nil && fi.IsDir() {
				out = append(out, p)
			}
		}
	}
	return out
}

// claudeProjectDirs lists the folders Claude Code has been run in: the keys of
// "projects" in each config's .claude.json (default, CLAUDE_CONFIG_DIR, and the
// work account's).
func claudeProjectDirs(home string) []string {
	files := []string{filepath.Join(home, ".claude.json")}
	for _, cfg := range []string{os.Getenv("CLAUDE_CONFIG_DIR"), workConfigDir()} {
		if cfg != "" {
			files = append(files, filepath.Join(cfg, ".claude.json"))
		}
	}
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var cfg struct {
			Projects map[string]json.RawMessage `json:"projects"`
		}
		if json.Unmarshal(b, &cfg) != nil {
			continue
		}
		for dir := range cfg.Projects {
			out = append(out, dir)
		}
	}
	return out
}

// repoRoot returns the repo dir sits in, mapping a linked worktree to its main
// checkout. "" when dir is gone, not inside a repo, or the repo is not below home.
func repoRoot(dir, home string) string {
	dir = filepath.Clean(dir)
	if !below(dir, home) {
		return ""
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	for d := dir; below(d, home); d = filepath.Dir(d) {
		fi, err := os.Stat(filepath.Join(d, ".git"))
		if err != nil {
			continue
		}
		if fi.IsDir() {
			return d
		}
		return worktreeMain(d, home)
	}
	return ""
}

// worktreeMain returns the main checkout of the linked worktree at d, whose .git
// file reads "gitdir: <main>/.git/worktrees/<name>" (relative to d with
// --relative-paths). A submodule's .git file names no worktree, so d is its own
// repo. "" when the main checkout is gone or not below home.
func worktreeMain(d, home string) string {
	b, err := os.ReadFile(filepath.Join(d, ".git"))
	if err != nil {
		return ""
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(d, gitdir)
	}
	gitdir = filepath.Clean(gitdir)
	sep := string(filepath.Separator) + ".git" + string(filepath.Separator) + "worktrees" + string(filepath.Separator)
	i := strings.Index(gitdir, sep)
	if i <= 0 {
		return d
	}
	main := gitdir[:i]
	if fi, err := os.Stat(filepath.Join(main, ".git")); err != nil || !fi.IsDir() || !below(main, home) {
		return ""
	}
	return main
}

// below reports whether p is strictly inside dir.
func below(p, dir string) bool {
	return strings.HasPrefix(p, filepath.Clean(dir)+string(filepath.Separator))
}

func handleProjects(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(listProjects())
}

var tmuxNameRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func tmuxSessionName(base string) string {
	name := strings.Trim(tmuxNameRe.ReplaceAllString(base, "-"), "-")
	if name == "" {
		name = "session"
	}
	existing := map[string]bool{}
	if out, err := tmuxOutput("list-sessions", "-F", "#{session_name}"); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			existing[l] = true
		}
	}
	if !existing[name] {
		return name
	}
	for i := 2; ; i++ {
		cand := name + "-" + strconv.Itoa(i)
		if !existing[cand] {
			return cand
		}
	}
}

// agentCommand maps a UI agent choice to the shell command run in the new pane.
// "" for the work account when there is none.
func agentCommand(agent string) string {
	switch agent {
	case "codex":
		return "codex"
	case "claude-work":
		return workClaudeCommand("")
	default: // claude (personal)
		return "claude"
	}
}

func handleLaunch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path  string `json:"path"`
		Agent string `json:"agent"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Path == "" {
		http.Error(w, "bad request", 400)
		return
	}
	// only allow launching in a directory we actually offer (no arbitrary paths)
	ok := false
	for _, p := range listProjects() {
		if p.Path == req.Path {
			ok = true
			break
		}
	}
	if !ok {
		http.Error(w, "unknown project", 400)
		return
	}
	command := agentCommand(req.Agent)
	if command == "" {
		http.Error(w, "no work account: sign in a ~/.claude-* config dir or set CLAUDE_WALL_WORK_CONFIG_DIR", 400)
		return
	}

	sess := tmuxSessionName(filepath.Base(req.Path))
	args := []string{"new-session", "-d", "-s", sess, "-c", req.Path}
	// A detached session is 80x24 until a terminal attaches. At 80 columns
	// Claude Code drops its mode line and the status line goes compact, so
	// start it at the size the desktop terminal gives its windows.
	if w, h := desktopWindowSize(); w > 0 && h > 0 {
		args = append(args, "-x", strconv.Itoa(w), "-y", strconv.Itoa(h))
	}
	if err := tmuxExec(args...); err != nil {
		http.Error(w, "tmux failed: "+err.Error(), 500)
		return
	}
	// run the agent in the new session's shell
	tmuxExec("send-keys", "-t", sess, command, "Enter")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": sess})
}

// desktopWindowSize is the window size an attached terminal gives, else the
// widest existing window, else 0, 0 (tmux's default). The hub's control-mode
// client is attached too but never sizes a window, so it is skipped.
func desktopWindowSize() (int, int) {
	if out, err := tmuxOutput("list-clients", "-F", "#{client_control_mode} #{window_width} #{window_height}"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) == 3 && f[0] == "0" {
				w, _ := strconv.Atoi(f[1])
				h, _ := strconv.Atoi(f[2])
				if w > 0 && h > 0 {
					return w, h
				}
			}
		}
	}
	bw, bh := 0, 0
	if out, err := tmuxOutput("list-windows", "-a", "-F", "#{window_width} #{window_height}"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) == 2 {
				w, _ := strconv.Atoi(f[0])
				h, _ := strconv.Atoi(f[1])
				if w > bw || (w == bw && h > bh) {
					bw, bh = w, h
				}
			}
		}
	}
	return bw, bh
}
