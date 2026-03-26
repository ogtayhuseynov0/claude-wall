package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

type scheduledTask struct {
	ID           string    `json:"id"`
	Target       string    `json:"target"`       // tmux pane target
	Command      string    `json:"command"`       // text to send to the pane
	IntervalMin  int       `json:"intervalMin"`   // minutes between runs
	MaxAttempts  int       `json:"maxAttempts"`   // stop after this many runs
	MaxEmpty     int       `json:"maxEmpty"`      // stop after N consecutive idle results
	Attempts     int       `json:"attempts"`      // runs so far
	EmptyCount   int       `json:"emptyCount"`    // consecutive runs where agent was idle after
	Status       string    `json:"status"`        // running, paused, completed, stopped
	CreatedAt    time.Time `json:"createdAt"`
	LastRunAt    time.Time `json:"lastRunAt,omitempty"`
	NextRunAt    time.Time `json:"nextRunAt,omitempty"`
	WaitingIdle  bool      `json:"waitingIdle"`   // waiting for pane to go idle before sending
}

type scheduler struct {
	mu    sync.RWMutex
	tasks map[string]*scheduledTask
	stop  chan struct{}
}

var sched = &scheduler{
	tasks: make(map[string]*scheduledTask),
	stop:  make(chan struct{}),
}

func schedulerFile() string {
	home, _ := os.UserHomeDir()
	return home + "/.claude/claude-wall-scheduler.json"
}

// save writes tasks to disk. Caller must NOT hold s.mu.
func (s *scheduler) save() {
	s.mu.RLock()
	data, _ := json.MarshalIndent(s.tasks, "", "  ")
	s.mu.RUnlock()
	os.WriteFile(schedulerFile(), data, 0644)
}

func (s *scheduler) load() {
	data, err := os.ReadFile(schedulerFile())
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var tasks map[string]*scheduledTask
	if json.Unmarshal(data, &tasks) == nil && tasks != nil {
		s.tasks = tasks
		// Resume running tasks
		for _, t := range s.tasks {
			if t.Status == "running" {
				t.NextRunAt = time.Now().Add(30 * time.Second) // give 30s after restart
			}
		}
	}
}

func (s *scheduler) run() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

func (s *scheduler) tick() {
	s.mu.Lock()

	now := time.Now()
	dirty := false

	for _, task := range s.tasks {
		if task.Status != "running" {
			continue
		}

		// Check if it's time to run
		if now.Before(task.NextRunAt) {
			continue
		}

		// Check if waiting for pane to go idle
		if task.WaitingIdle {
			paneStatus := getPaneHookStatus(task.Target)
			if paneStatus == "working" || paneStatus == "permission" {
				continue // still busy, check again in 5s
			}
			task.WaitingIdle = false
		}

		// Check stop conditions
		if task.Attempts >= task.MaxAttempts {
			task.Status = "completed"
			fmt.Printf("  [scheduler] Task %s completed: max attempts reached (%d)\n", task.ID, task.MaxAttempts)
			continue
		}
		if task.MaxEmpty > 0 && task.EmptyCount >= task.MaxEmpty {
			task.Status = "completed"
			fmt.Printf("  [scheduler] Task %s completed: %d consecutive empty cycles\n", task.ID, task.MaxEmpty)
			continue
		}

		// Send command to pane (unlock first — tmux can be slow)
		target := task.Target
		command := task.Command
		task.Attempts++
		task.LastRunAt = now
		task.NextRunAt = now.Add(time.Duration(task.IntervalMin) * time.Minute)
		task.WaitingIdle = true
		dirty = true
		s.mu.Unlock()
		sendTextToPane(target, command)
		s.mu.Lock()

		fmt.Printf("  [scheduler] Task %s: sent cycle %d/%d to %s, next in %dm\n",
			task.ID, task.Attempts, task.MaxAttempts, task.Target, task.IntervalMin)
	}

	s.mu.Unlock()
	if dirty {
		s.save()
	}
}

// getPaneHookStatus returns the hook-derived status of a pane
func getPaneHookStatus(target string) string {
	if hooks == nil {
		return "idle"
	}
	dir := getPaneDir(target)
	if dir == "" {
		return "idle"
	}
	hs := hooks.getStateForPane(target, dir)
	if hs == nil {
		return "idle"
	}
	return hs.Status
}

// sendTextToPane types text + Enter into a tmux pane
func sendTextToPane(target, text string) {
	hexes := make([]string, 0, len(text)+1)
	for _, b := range []byte(text + "\r") {
		hexes = append(hexes, fmt.Sprintf("%02x", b))
	}
	args := append([]string{"send-keys", "-t", target, "-H"}, hexes...)
	exec.Command("tmux", args...).Run()
}

// ── HTTP handlers ──

func handleSchedulerList(w http.ResponseWriter, r *http.Request) {
	sched.mu.RLock()
	defer sched.mu.RUnlock()

	// Optional filter by target
	target := r.URL.Query().Get("target")

	tasks := make([]*scheduledTask, 0, len(sched.tasks))
	for _, t := range sched.tasks {
		if target != "" && t.Target != target {
			continue
		}
		tasks = append(tasks, t)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tasks)
}

// getTasksForPane returns copies of active tasks for a specific pane
func (s *scheduler) getTasksForPane(target string) []scheduledTask {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []scheduledTask
	for _, t := range s.tasks {
		if t.Target == target && (t.Status == "running" || t.Status == "paused") {
			result = append(result, *t) // copy, not pointer
		}
	}
	return result
}

func handleSchedulerCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}

	var req struct {
		Target      string `json:"target"`
		Command     string `json:"command"`
		IntervalMin int    `json:"intervalMin"`
		MaxAttempts int    `json:"maxAttempts"`
		MaxEmpty    int    `json:"maxEmpty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	// Resolve partial target
	target := resolveTarget(req.Target)
	if target == "" {
		http.Error(w, "unknown target: "+req.Target, 404)
		return
	}

	if req.IntervalMin <= 0 {
		req.IntervalMin = 10
	}
	if req.MaxAttempts <= 0 {
		req.MaxAttempts = 5
	}
	if req.MaxEmpty <= 0 {
		req.MaxEmpty = 3
	}

	id := fmt.Sprintf("task-%d", time.Now().UnixNano()%100000)

	task := &scheduledTask{
		ID:          id,
		Target:      target,
		Command:     req.Command,
		IntervalMin: req.IntervalMin,
		MaxAttempts: req.MaxAttempts,
		MaxEmpty:    req.MaxEmpty,
		Status:      "running",
		CreatedAt:   time.Now(),
		NextRunAt:   time.Now(), // run immediately
	}

	sched.mu.Lock()
	sched.tasks[id] = task
	sched.mu.Unlock()
	sched.save()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(task)
}

func handleSchedulerAction(w http.ResponseWriter, r *http.Request) {
	// /api/scheduler/:id/:action
	parts := splitPath(r.URL.Path, "/api/scheduler/")
	if len(parts) < 2 {
		http.Error(w, "need /api/scheduler/{id}/{action}", 400)
		return
	}
	id, action := parts[0], parts[1]

	sched.mu.Lock()

	task, ok := sched.tasks[id]
	if !ok {
		sched.mu.Unlock()
		http.Error(w, "task not found", 404)
		return
	}

	switch action {
	case "pause":
		task.Status = "paused"
	case "resume":
		task.Status = "running"
		task.NextRunAt = time.Now()
	case "stop":
		task.Status = "stopped"
	case "delete":
		delete(sched.tasks, id)
	default:
		sched.mu.Unlock()
		http.Error(w, "unknown action: "+action, 400)
		return
	}

	target := task.Target
	sched.mu.Unlock()
	sched.save()

	// Clear hub cache so tile gets fresh content (removes stale badge)
	// Safe: sched.mu is released, hub.mu is not held by us
	if hub != nil {
		hub.mu.Lock()
		delete(hub.latest, target)
		hub.mu.Unlock()
	}

	w.Write([]byte(`{"status":"ok"}`))
}

func splitPath(path, prefix string) []string {
	trimmed := path[len(prefix):]
	parts := []string{}
	for _, p := range split(trimmed, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func split(s, sep string) []string {
	result := []string{}
	for len(s) > 0 {
		idx := indexOf(s, sep)
		if idx < 0 {
			result = append(result, s)
			break
		}
		result = append(result, s[:idx])
		s = s[idx+len(sep):]
	}
	return result
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// Track when a pane goes idle (for empty cycle detection)
func (s *scheduler) onPaneIdle(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, task := range s.tasks {
		if task.Target == target && task.WaitingIdle {
			task.EmptyCount++
		}
	}
}

// Track when a pane does work (reset empty counter)
func (s *scheduler) onPaneWorking(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, task := range s.tasks {
		if task.Target == target {
			task.EmptyCount = 0
		}
	}
}
