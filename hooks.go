package main

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

// hookEvent represents a Claude Code hook event received via HTTP
type hookEvent struct {
	SessionID      string          `json:"session_id"`
	CWD            string          `json:"cwd"`
	EventName      string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	Notification   json.RawMessage `json:"notification"`
	TranscriptPath string          `json:"transcript_path"`
	ReceivedAt     time.Time       `json:"-"`
	PaneTarget     string          `json:"-"`
}

// hookState tracks the derived state for a Claude Code session
type hookState struct {
	SessionID    string
	CWD          string
	Status       string // "working", "permission", "idle"
	Activity     string // e.g. "$ npm test", "Edit main.go"
	ToolName     string
	UpdatedAt    time.Time
	LastWorkingAt time.Time // when status was last set to "working"
	PaneTarget   string    // mapped pane target (if resolved)
}

// hookStore maps session_id → hookState
type hookStore struct {
	mu       sync.RWMutex
	sessions map[string]*hookState // session_id → state
}

func newHookStore() *hookStore {
	return &hookStore{
		sessions: make(map[string]*hookState),
	}
}

// cleanup removes sessions that haven't received events in a while
func (s *hookStore) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, state := range s.sessions {
		if time.Since(state.UpdatedAt) > 10*time.Minute {
			delete(s.sessions, id)
		}
	}
}

// processEvent updates state based on a hook event
func (s *hookStore) processEvent(event hookEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.sessions[event.SessionID]
	if !ok {
		state = &hookState{
			SessionID: event.SessionID,
			CWD:       normalizePath(event.CWD),
			Status:    "idle",
		}
		s.sessions[event.SessionID] = state
	}

	state.UpdatedAt = time.Now()
	state.CWD = normalizePath(event.CWD)
	if event.PaneTarget != "" {
		state.PaneTarget = event.PaneTarget
	}

	// recentlyWorking returns true if agent was working within the cooldown window.
	// Used to prevent flicker: don't drop out of "working" too quickly.
	recentlyWorking := func() bool {
		return state.Status == "working" && time.Since(state.LastWorkingAt) < 8*time.Second
	}

	switch event.EventName {
	case "PreToolUse":
		state.Status = "working"
		state.LastWorkingAt = time.Now()
		state.ToolName = event.ToolName
		state.Activity = formatActivity(event.ToolName, event.ToolInput)

	case "PostToolUse":
		state.Status = "working"
		state.LastWorkingAt = time.Now()

	case "PermissionRequest":
		state.Status = "permission"
		state.Activity = formatActivity(event.ToolName, event.ToolInput)

	case "Stop":
		if !recentlyWorking() {
			state.Status = "idle"
			state.Activity = ""
			state.ToolName = ""
		}

	case "Notification":
		var notif map[string]interface{}
		if json.Unmarshal(event.Notification, &notif) == nil {
			if t, ok := notif["type"].(string); ok {
				switch t {
				case "permission_prompt":
					state.Status = "permission"
				case "elicitation_dialog":
					state.Status = "permission"
					state.Activity = "Waiting for input"
				case "idle_prompt":
					if state.Status == "permission" {
						break
					}
					if !recentlyWorking() {
						state.Status = "idle"
						state.Activity = ""
					}
				}
			}
		}

	case "PostToolUseFailure":
		state.Status = "working"
		state.LastWorkingAt = time.Now()
		state.Activity = "⚠ " + formatActivity(event.ToolName, event.ToolInput) + " (failed)"

	case "StopFailure":
		if !recentlyWorking() {
			state.Status = "idle"
			state.Activity = "⚠ API error"
		}

	case "SubagentStart":
		state.Status = "working"
		state.LastWorkingAt = time.Now()
		state.Activity = "Subagent started"

	case "SubagentStop":
		state.Status = "working"
		state.LastWorkingAt = time.Now()

	case "TaskCompleted":
		state.Status = "idle"
		state.Activity = "Task completed"

	case "SessionStart":
		state.Status = "idle"
		state.Activity = ""

	case "SessionEnd":
		state.Status = "idle"
		state.Activity = ""
	}
}

// getStateForPane finds the hook session mapped to this pane target.
// Only returns a match when the session was explicitly linked via TMUX_PANE
// (X-Tmux-Pane header). No CWD guessing — prevents cross-pane status leaking.
func (s *hookStore) getStateForPane(paneTarget, paneDir string) *hookState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, state := range s.sessions {
		if state.PaneTarget == paneTarget {
			return state
		}
	}
	return nil
}


func normalizePath(p string) string {
	p = strings.TrimRight(p, "/")
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}
	return p
}

// formatActivity creates a human-readable activity string
func formatActivity(toolName string, toolInput json.RawMessage) string {
	var input map[string]interface{}
	if json.Unmarshal(toolInput, &input) != nil {
		return toolName
	}

	switch toolName {
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			if len(cmd) > 60 {
				cmd = cmd[:57] + "..."
			}
			return "$ " + cmd
		}
	case "Edit":
		if fp, ok := input["file_path"].(string); ok {
			return "Edit " + baseName(fp)
		}
	case "Write":
		if fp, ok := input["file_path"].(string); ok {
			return "Write " + baseName(fp)
		}
	case "Read":
		if fp, ok := input["file_path"].(string); ok {
			return "Read " + baseName(fp)
		}
	case "Grep":
		if pat, ok := input["pattern"].(string); ok {
			return "Grep: " + pat
		}
	case "Glob":
		if pat, ok := input["pattern"].(string); ok {
			return "Glob: " + pat
		}
	case "Agent":
		if desc, ok := input["description"].(string); ok {
			return "Agent: " + desc
		}
	case "WebSearch":
		if q, ok := input["query"].(string); ok {
			return "Search: " + q
		}
	}

	return toolName
}
