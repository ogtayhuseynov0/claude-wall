package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Sibling-pane switching: a session/window typically holds a Claude pane plus an
// editor (nvim) and a shell for commands. handlePanes lists every pane in the
// window that contains the given pane so the phone can flip between them (the
// pane view already streams + types into whatever target it points at).

type sibPane struct {
	Target string `json:"target"`
	Label  string `json:"label"`
	Cmd    string `json:"cmd"`
	Active bool   `json:"active"`
}

func handlePanes(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimPrefix(r.URL.Path, "/api/panes/")
	if target == "" {
		http.Error(w, "missing target", http.StatusBadRequest)
		return
	}
	win := target
	if i := strings.LastIndex(target, "."); i >= 0 {
		win = target[:i] // session:window
	}
	out, err := tmuxOutput("list-panes", "-t", win, "-F",
		"#{session_name}:#{window_index}.#{pane_index}\t#{pane_current_command}\t#{pane_title}\t#{pane_active}")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	list := []sibPane{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		p := strings.SplitN(line, "\t", 4)
		if len(p) < 4 {
			continue
		}
		list = append(list, sibPane{
			Target: p[0],
			Label:  paneLabel(p[1], p[2]),
			Cmd:    p[1],
			Active: p[3] == "1",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(list)
}

// paneLabel gives a short, friendly name for a pane from its command/title.
func paneLabel(cmd, title string) string {
	shells := map[string]bool{"zsh": true, "bash": true, "sh": true, "fish": true}
	switch {
	case cmd == "claude" || isVersionString(cmd) || (strings.Contains(title, "Claude Code") && !shells[cmd]):
		return "claude"
	case cmd == "codex" || (!shells[cmd] && strings.Contains(strings.ToLower(title), "codex")):
		return "codex"
	case cmd == "nvim" || cmd == "vim":
		return "nvim"
	case shells[cmd]:
		return "shell"
	default:
		return cmd
	}
}
