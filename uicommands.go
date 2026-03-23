package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// uiCommand represents a command from the backend to the frontend
type uiCommand struct {
	Action string `json:"action"` // focus, zoom, minimize, restore, scroll-bottom
	Target string `json:"target"` // pane target e.g. Agent:1.2
}

// uiBroadcast sends commands to all connected SSE clients
type uiBroadcast struct {
	mu      sync.Mutex
	clients map[chan uiCommand]bool
}

var uiBcast = &uiBroadcast{clients: make(map[chan uiCommand]bool)}

func (b *uiBroadcast) subscribe() chan uiCommand {
	ch := make(chan uiCommand, 8)
	b.mu.Lock()
	b.clients[ch] = true
	b.mu.Unlock()
	return ch
}

func (b *uiBroadcast) unsubscribe(ch chan uiCommand) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
	close(ch)
}

func (b *uiBroadcast) send(cmd uiCommand) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- cmd:
		default:
		}
	}
}

// handleUIAction handles POST/GET /api/ui/:action/:target
// Target can be full (Agent:1.2) or partial (Agent, Zangy) — we resolve it
func handleUIAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	path := strings.TrimPrefix(r.URL.Path, "/api/ui/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		http.Error(w, "need /api/ui/{action}/{target}", 400)
		return
	}

	action := parts[0]
	target := resolveTarget(parts[1])
	if target == "" {
		http.Error(w, "unknown target: "+parts[1], 404)
		return
	}

	cmd := uiCommand{Action: action, Target: target}
	uiBcast.send(cmd)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "sent", "target": target})
}

// resolveTarget takes a partial target (e.g. "Zangy") and finds the full pane target
func resolveTarget(input string) string {
	if strings.Contains(input, ".") {
		return input
	}

	panes, _ := findClaudePanes()
	lower := strings.ToLower(input)

	for _, p := range panes {
		if strings.ToLower(p.Session) == lower || strings.ToLower(p.Target) == lower || strings.ToLower(p.SessWin) == lower {
			return p.Target
		}
	}
	for _, p := range panes {
		if strings.Contains(strings.ToLower(p.Session), lower) || strings.Contains(strings.ToLower(p.DirName), lower) {
			return p.Target
		}
	}
	return ""
}

// handleUIEvents handles GET /api/ui/events (SSE stream)
func handleUIEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := uiBcast.subscribe()
	defer uiBcast.unsubscribe(ch)

	// Send initial ping
	fmt.Fprintf(w, "data: {\"action\":\"ping\"}\n\n")
	flusher.Flush()

	for {
		select {
		case cmd, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(cmd)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
