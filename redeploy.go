package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

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
	repo := filepath.Join(os.Getenv("HOME"), "Desktop", "Projects", "PERSONAL", "claude-wall")
	script := filepath.Join(repo, "macapp", "refresh-bundle.sh")
	if _, err := os.Stat(script); err != nil {
		http.Error(w, "refresh-bundle.sh not found at "+script, http.StatusNotFound)
		return
	}
	logf := filepath.Join(os.Getenv("HOME"), ".claude", "cw-redeploy.log")
	// Login-shell PATH isn't guaranteed here; set an explicit one covering go +
	// codesign + tmux so the build/sign/restart steps resolve their tools.
	path := "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:" + filepath.Join(os.Getenv("HOME"), "go", "bin")
	cmd := fmt.Sprintf("cd %q && PATH=%q /opt/homebrew/bin/bash %q > %q 2>&1", repo, path, script, logf)
	if err := tmuxExec("run-shell", "-b", cmd); err != nil {
		http.Error(w, "could not start redeploy: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
