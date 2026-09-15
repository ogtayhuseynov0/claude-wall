package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Restart-with-resume: kill the agent in a pane and relaunch it resuming its
// last session. The mobile confirm modal picks the account (personal Claude,
// work Claude, or Codex); we default the modal to the account we detect from
// the pane's running process environment.

var claudeConfigDirRe = regexp.MustCompile(`CLAUDE_CONFIG_DIR=(\S+)`)

// paneConfigDir reads CLAUDE_CONFIG_DIR from the processes on the pane's tty
// (all processes in a pane share its tty). Empty when unset (personal account).
func paneConfigDir(target string) string {
	out, err := exec.Command("tmux", "display-message", "-p", "-t", target, "#{pane_tty}").Output()
	if err != nil {
		return ""
	}
	tty := strings.TrimPrefix(strings.TrimSpace(string(out)), "/dev/")
	if tty == "" {
		return ""
	}
	ps, err := exec.Command("ps", "-Eww", "-t", tty, "-o", "command=").Output()
	if err != nil {
		return ""
	}
	if m := claudeConfigDirRe.FindStringSubmatch(string(ps)); len(m) == 2 {
		return m[1]
	}
	return ""
}

// configDirsByTTY maps a pane tty (e.g. "ttys005") → CLAUDE_CONFIG_DIR of the
// agent running on it, from a single `ps` env scan. Cached ~5s because
// findClaudePanes runs often and profiles almost never change mid-session.
var (
	ttyCfgMu    sync.Mutex
	ttyCfgCache map[string]string
	ttyCfgAt    time.Time
)

func configDirsByTTY() map[string]string {
	ttyCfgMu.Lock()
	defer ttyCfgMu.Unlock()
	if ttyCfgCache != nil && time.Since(ttyCfgAt) < 5*time.Second {
		return ttyCfgCache
	}
	m := map[string]string{}
	if out, err := exec.Command("ps", "-Aww", "-E", "-o", "tty=,command=").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.Contains(line, "CLAUDE_CONFIG_DIR=") {
				continue
			}
			sp := strings.IndexByte(line, ' ')
			if sp <= 0 {
				continue
			}
			tty := line[:sp] // full name, e.g. "ttys005" (matches #{pane_tty} minus /dev/)
			if tty == "??" {
				continue
			}
			if mm := claudeConfigDirRe.FindStringSubmatch(line[sp:]); len(mm) == 2 {
				if _, ok := m[tty]; !ok {
					m[tty] = mm[1]
				}
			}
		}
	}
	ttyCfgCache, ttyCfgAt = m, time.Now()
	return m
}

// profileFromConfigDir turns a CLAUDE_CONFIG_DIR into a short profile label:
// "" (personal / default ~/.claude), "work" (~/.claude-ts), else the suffix.
func profileFromConfigDir(cfg string) string {
	if cfg == "" {
		return ""
	}
	name := baseName(cfg) // ".claude-ts" → "claude-ts"
	name = strings.TrimPrefix(name, ".")
	name = strings.TrimPrefix(name, "claude-")
	name = strings.TrimPrefix(name, "claude")
	name = strings.Trim(name, ".-")
	switch name {
	case "", "ts", "work":
		if name == "" {
			return "" // plain ~/.claude → personal
		}
		return "work"
	default:
		return name
	}
}

// paneProfile is the profile label for a pane's tty (empty = personal Claude).
func paneProfile(tty string) string {
	return profileFromConfigDir(configDirsByTTY()[strings.TrimPrefix(tty, "/dev/")])
}

// paneAccount is the best-guess account for the confirm modal's default:
// "codex", "claude-work" (CLAUDE_CONFIG_DIR points at ~/.claude-ts), else "claude".
func paneAccount(p claudePane) string {
	if p.Agent == "codex" {
		return "codex"
	}
	if cfg := paneConfigDir(p.Target); strings.Contains(cfg, ".claude-ts") {
		return "claude-work"
	}
	return "claude"
}

// resumeCommand is the command TYPED into a fresh shell (via send-keys) to
// relaunch the chosen agent resuming its last session. We type it into a real
// interactive shell rather than exec it through `sh -c "…; exec $SHELL"`,
// because that wrapper leaves tmux reporting pane_current_command=zsh (the
// wrapper) instead of the agent — so findClaudePanes stops recognizing the
// pane and it vanishes from the dashboard. Typed into a shell, the agent is the
// pane's foreground process (comm = version string) and stays detected; the
// shell also remains when the agent exits, so the pane doesn't close.
func resumeCommand(account string) string {
	switch account {
	case "codex":
		return "codex resume --last"
	case "claude-work":
		return `CLAUDE_CONFIG_DIR="$HOME/.claude-ts" claude --continue`
	default:
		return "claude --continue"
	}
}

func findPaneByTarget(target string) *claudePane {
	panes, _ := findClaudePanes()
	for i := range panes {
		if panes[i].Target == target {
			return &panes[i]
		}
	}
	return nil
}

// handleRestartAgent: GET returns the default account for the modal; POST kills
// the pane's agent and respawns it resuming the last session with the chosen
// account. POST is destructive — the mobile UI confirms before calling.
func handleRestartAgent(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimPrefix(r.URL.Path, "/api/restart-agent/")
	if target == "" {
		http.Error(w, "no target", 400)
		return
	}
	pane := findPaneByTarget(target)
	if pane == nil {
		http.Error(w, "unknown pane", 404)
		return
	}

	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"account": paneAccount(*pane), "agent": pane.Agent})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	var req struct {
		Account string `json:"account"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	account := req.Account
	switch account {
	case "claude", "claude-work", "codex":
		// explicit choice
	default:
		account = paneAccount(*pane)
	}

	// Respawn a plain interactive shell (kills the current agent), then type the
	// resume command into it. Passing the shell explicitly matters: respawn-pane
	// with no command re-runs the pane's *original* command, not a shell.
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	respawn := []string{"respawn-pane", "-k", "-t", target}
	if pane.Dir != "" {
		respawn = append(respawn, "-c", pane.Dir)
	}
	respawn = append(respawn, shell)
	if err := tmuxExec(respawn...); err != nil {
		http.Error(w, "restart failed: "+err.Error(), 500)
		return
	}
	time.Sleep(600 * time.Millisecond) // let the shell come up before typing
	tmuxExec("send-keys", "-t", target, resumeCommand(account), "Enter")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "account": account})
}
