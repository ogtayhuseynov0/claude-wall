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
		state.Status = "idle"
		state.Activity = ""
		state.ToolName = ""

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
					// Don't override permission status — user hasn't responded yet
					if state.Status == "permission" {
						break
					}
					// Only go idle if we haven't been working recently (>3s)
					// This prevents flicker from idle_prompt between rapid tool calls
					if state.Status != "working" || time.Since(state.LastWorkingAt) > 3*time.Second {
						state.Status = "idle"
						state.Activity = ""
					}
				}
			}
		}

	case "PostToolUseFailure":
		state.Status = "error"
		state.Activity = "⚠ " + formatActivity(event.ToolName, event.ToolInput) + " (failed)"

	case "StopFailure":
		state.Status = "error"
		state.Activity = "⚠ API error"

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

// getStateForPane finds the best matching hook session for a pane.
// Uses pane target → session mapping if available, otherwise matches by cwd.
func (s *hookStore) getStateForPane(paneTarget, paneDir string) *hookState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	dir := normalizePath(paneDir)

	// First: check if any session is already mapped to this pane target
	for _, state := range s.sessions {
		if state.PaneTarget == paneTarget {
			return state
		}
	}

	// Second: find unmapped sessions whose cwd matches this pane dir
	// Pick the most recently updated one
	var best *hookState
	for _, state := range s.sessions {
		if state.PaneTarget != "" {
			continue // already claimed by another pane
		}
		cwd := state.CWD
		if cwd == dir || strings.HasPrefix(cwd, dir+"/") {
			if best == nil || state.UpdatedAt.After(best.UpdatedAt) {
				best = state
			}
		}
	}

	// Claim this session for the pane
	if best != nil {
		best.PaneTarget = paneTarget
	}

	return best
}

// isStale returns true if the session's last hook event is older than timeout
func (s *hookStore) isStaleForPane(paneTarget string, timeout time.Duration) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, state := range s.sessions {
		if state.PaneTarget == paneTarget {
			return time.Since(state.UpdatedAt) > timeout
		}
	}
	return true
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
