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

// projectRoots are the directories scanned for projects. Immediate subdirs that
// contain a .git are offered. Override with CLAUDE_WALL_PROJECTS_DIRS (colon-sep).
func projectRoots() []string {
	if env := os.Getenv("CLAUDE_WALL_PROJECTS_DIRS"); env != "" {
		return strings.Split(env, ":")
	}
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, "Desktop", "Projects"),
		filepath.Join(home, "Desktop", "Projects", "PERSONAL"),
	}
}

func listProjects() []projectEntry {
	seen := map[string]bool{}
	var out []projectEntry
	for _, root := range projectRoots() {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			p := filepath.Join(root, e.Name())
			if seen[p] {
				continue
			}
			// only real project dirs (have a .git)
			if fi, err := os.Stat(filepath.Join(p, ".git")); err != nil || !fi.IsDir() {
				continue
			}
			seen[p] = true
			out = append(out, projectEntry{Name: e.Name(), Path: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
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
func agentCommand(agent string) string {
	switch agent {
	case "codex":
		return "codex"
	case "claude-work":
		return `CLAUDE_CONFIG_DIR="$HOME/.claude-ts" claude`
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

	sess := tmuxSessionName(filepath.Base(req.Path))
	if err := tmuxExec("new-session", "-d", "-s", sess, "-c", req.Path); err != nil {
		http.Error(w, "tmux failed: "+err.Error(), 500)
		return
	}
	// run the agent in the new session's shell
	tmuxExec("send-keys", "-t", sess, agentCommand(req.Agent), "Enter")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": sess})
}
