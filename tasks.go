package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// claudeTask represents a task from Claude Code's internal task system
type claudeTask struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Description string   `json:"description,omitempty"`
	ActiveForm  string   `json:"activeForm,omitempty"`
	Status      string   `json:"status"` // pending, in_progress, completed, deleted
	Blocks      []string `json:"blocks"`
	BlockedBy   []string `json:"blockedBy"`
}

// paneTasks groups tasks with pane metadata
type paneTasks struct {
	Target    string       `json:"target"`
	Session   string       `json:"session"`
	SessionID string       `json:"sessionId,omitempty"`
	Tasks     []claudeTask `json:"tasks"`
	Summary   taskSummary  `json:"summary"`
}

type taskSummary struct {
	Total      int `json:"total"`
	Pending    int `json:"pending"`
	InProgress int `json:"inProgress"`
	Completed  int `json:"completed"`
}

// tasksDir returns the path to Claude Code's task storage
func tasksDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "tasks")
}

// getSessionIDForPane returns the Claude Code session UUID mapped to a pane target
func getSessionIDForPane(target string) string {
	if hooks == nil {
		return ""
	}
	hooks.mu.RLock()
	defer hooks.mu.RUnlock()
	for _, state := range hooks.sessions {
		if state.PaneTarget == target {
			return state.SessionID
		}
	}
	return ""
}

// readTasksForSession reads all tasks from ~/.claude/tasks/{sessionID}/
func readTasksForSession(sessionID string) []claudeTask {
	dir := filepath.Join(tasksDir(), sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var tasks []claudeTask
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		// Skip metadata files
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var t claudeTask
		if json.Unmarshal(data, &t) != nil {
			continue
		}
		if t.Status == "deleted" {
			continue
		}
		tasks = append(tasks, t)
	}
	return tasks
}

func summarize(tasks []claudeTask) taskSummary {
	var s taskSummary
	for _, t := range tasks {
		s.Total++
		switch t.Status {
		case "pending":
			s.Pending++
		case "in_progress":
			s.InProgress++
		case "completed":
			s.Completed++
		}
	}
	return s
}

// handleTasksAPI serves GET /api/tasks (all panes) and GET /api/tasks/{target}
func handleTasksAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	target := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	target = strings.TrimSuffix(target, "/")

	// Single pane
	if target != "" && target != "all" && !strings.Contains(target, "/") {
		sessionID := getSessionIDForPane(target)
		if sessionID == "" {
			json.NewEncoder(w).Encode(paneTasks{Target: target, Tasks: []claudeTask{}})
			return
		}
		tasks := readTasksForSession(sessionID)
		if tasks == nil {
			tasks = []claudeTask{}
		}
		json.NewEncoder(w).Encode(paneTasks{
			Target:    target,
			SessionID: sessionID,
			Tasks:     tasks,
			Summary:   summarize(tasks),
		})
		return
	}

	// All panes
	if hooks == nil {
		json.NewEncoder(w).Encode([]paneTasks{})
		return
	}

	hooks.mu.RLock()
	type sessionInfo struct {
		sessionID  string
		paneTarget string
	}
	var infos []sessionInfo
	for _, state := range hooks.sessions {
		if state.PaneTarget != "" {
			infos = append(infos, sessionInfo{state.SessionID, state.PaneTarget})
		}
	}
	hooks.mu.RUnlock()

	var results []paneTasks
	for _, info := range infos {
		tasks := readTasksForSession(info.sessionID)
		if tasks == nil {
			tasks = []claudeTask{}
		}
		session := info.paneTarget
		if idx := strings.Index(info.paneTarget, ":"); idx >= 0 {
			session = info.paneTarget[:idx]
		}
		results = append(results, paneTasks{
			Target:    info.paneTarget,
			Session:   session,
			SessionID: info.sessionID,
			Tasks:     tasks,
			Summary:   summarize(tasks),
		})
	}
	json.NewEncoder(w).Encode(results)
}

// handleTaskAction serves POST /api/tasks/{target}/continue and /api/tasks/{target}/work-on
func handleTaskAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}

	// Parse: /api/tasks/{target}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		http.Error(w, "need /api/tasks/{target}/{action}", 400)
		return
	}
	target, action := parts[0], parts[1]

	// Resolve partial target names
	resolved := resolveTarget(target)
	if resolved == "" {
		http.Error(w, "unknown target: "+target, 404)
		return
	}

	switch action {
	case "continue":
		sendTextToPane(resolved, "/continue")
	case "work-on":
		var body struct {
			TaskID  string `json:"taskId"`
			Subject string `json:"subject"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		msg := "work on task: " + body.Subject
		if body.TaskID != "" {
			msg = "work on task #" + body.TaskID + ": " + body.Subject
		}
		sendTextToPane(resolved, msg)
	default:
		http.Error(w, "unknown action: "+action, 400)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}
