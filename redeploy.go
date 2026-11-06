package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// refreshScript is the rebuild script, relative to the source checkout.
var refreshScript = filepath.Join("macapp", "refresh-bundle.sh")

// sourceRepo finds the claude-wall checkout to rebuild from: CLAUDE_WALL_REPO,
// else the folder this binary was compiled in, else a folder above the running
// binary (the app bundle lives in macapp/). Only a git checkout counts, so a
// `go install` build does not rebuild itself from the read-only module cache.
// "" when none qualifies.
func sourceRepo() string {
	var candidates []string
	if env := os.Getenv("CLAUDE_WALL_REPO"); env != "" {
		candidates = append(candidates, env)
	}
	if _, file, _, ok := runtime.Caller(0); ok && filepath.IsAbs(file) {
		candidates = append(candidates, filepath.Dir(file))
	}
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			for dir := filepath.Dir(exe); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
				candidates = append(candidates, dir)
			}
		}
	}
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, refreshScript)); err == nil && !fi.IsDir() {
			return dir
		}
	}
	return ""
}

// handleRedeploy rebuilds the binary and restarts the server (the same
// `refresh-bundle.sh` used from the terminal), triggered from the phone. It runs
// detached in the tmux server context via `run-shell -b`, so it survives this
// server being restarted mid-script (macOS has no setsid). The phone polls
// /api/version and reloads when the new build is live.
func handleRedeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	repo := sourceRepo()
	if repo == "" {
		http.Error(w, "claude-wall source checkout not found; set CLAUDE_WALL_REPO", http.StatusNotFound)
		return
	}
	script := filepath.Join(repo, refreshScript)
	home, _ := os.UserHomeDir()
	logf := filepath.Join(home, ".claude", "cw-redeploy.log")
	// Login-shell PATH isn't guaranteed here; set an explicit one covering go +
	// codesign + tmux so the build/sign/restart steps resolve their tools.
	path := "/opt/homebrew/bin:/usr/local/bin:/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:" + filepath.Join(home, "go", "bin")
	cmd := fmt.Sprintf("cd %s && PATH=%s bash %s > %s 2>&1", shellQuote(repo), shellQuote(path), shellQuote(script), shellQuote(logf))
	// run-shell expands tmux formats, so a '#' in a path must be doubled.
	if err := tmuxExec("run-shell", "-b", strings.ReplaceAll(cmd, "#", "##")); err != nil {
		http.Error(w, "could not start redeploy: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
