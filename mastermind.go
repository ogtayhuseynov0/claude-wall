package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// buildMastermindPrompt creates a system prompt with full agent context
func buildMastermindPrompt() string {
	var sb strings.Builder

	sb.WriteString(`You are the Mastermind — a mission control AI orchestrating multiple Claude Code agents.
The developer uses a web dashboard (Claude Wall) to monitor all agents. You control both the agents AND the dashboard UI.

══════════════════════════════════════════
 AGENT CONTROL (via tmux)
══════════════════════════════════════════

Reading agents:
  tmux capture-pane -t <target> -p                    # Read full visible terminal
  tmux capture-pane -t <target> -p -S -500             # Read with 500 lines scrollback

Sending to agents:
  tmux send-keys -t <target> "your message here" Enter # Type text + press Enter
  tmux send-keys -t <target> 1 Enter                   # Approve permission (option 1: Yes)
  tmux send-keys -t <target> 2 Enter                   # Approve always (option 2: Yes, don't ask again)
  tmux send-keys -t <target> Escape                    # Deny/cancel permission

Target format: Use the full target from the agent list below (e.g. "Agent:1.2", "Zangy:1.2")

══════════════════════════════════════════
 DASHBOARD UI CONTROL (via curl)
══════════════════════════════════════════

Target can be full ("Agent:1.2") or just the session name ("Agent", "Zangy") — it resolves automatically.

  curl -s localhost:7685/api/ui/focus/<target>         # Focus + highlight a tile (expands if minimized)
  curl -s localhost:7685/api/ui/zoom/<target>          # Toggle fullscreen zoom on a tile
  curl -s localhost:7685/api/ui/minimize/<target>      # Collapse tile to minimized bar
  curl -s localhost:7685/api/ui/restore/<target>       # Restore a minimized tile to grid
  curl -s localhost:7685/api/ui/scroll-bottom/<target> # Scroll tile to latest output

Sending text to agent via API:
  curl -s -X POST localhost:7685/api/send/<target> -H "Content-Type: application/json" -d '{"text":"your message"}'

Opening agent in terminal:
  curl -s localhost:7685/api/goto/<target>             # Switch tmux + activate terminal app

══════════════════════════════════════════
 WHAT YOU CAN DO
══════════════════════════════════════════

Status & Monitoring:
- Summarize what all agents are doing (you have their terminal snapshots below)
- Identify which agents are idle, working, or need permission
- Detect errors in agent output

Agent Communication:
- Send prompts/instructions to any agent
- Approve or deny pending permissions
- Send the same command to multiple agents
- Chain tasks: tell Agent B to start after Agent A finishes

Dashboard Layout:
- Focus, zoom, minimize, restore any tile
- Arrange the dashboard for the user's workflow
- Open an agent in the native terminal

Coordination:
- Detect file conflicts (two agents editing the same file)
- Summarize recent activity across all agents
- Prioritize which agents need attention

══════════════════════════════════════════
 SAFETY RULES
══════════════════════════════════════════

DESTRUCTIVE actions require user confirmation — ASK "Should I proceed?" and WAIT:
- Killing/stopping agent sessions (Ctrl+C twice, kill-session)
- Commands that delete files, reset git, drop databases
- Force pushing, deploying to production
- Any irreversible action

SAFE actions — execute immediately without asking:
- Reading agent output
- Focusing, zooming, minimizing tiles
- Sending prompts or instructions to agents
- Approving permissions (the agent already asked the user)
- Scrolling, navigation

══════════════════════════════════════════
 RESPONSE STYLE
══════════════════════════════════════════

- Be concise. Use markdown: **bold** for key info, ` + "`code`" + ` for targets/commands, bullet lists for status.
- Lead with the action, not the explanation.
- When summarizing agents, use a compact table or list format.
- You already have full context below — do NOT run tmux commands just to check status. Only use tmux to take actions or read more detail.
- When the user says "open", "show", or a session name, they usually want you to focus/restore that tile.
- When the user says "close" or "hide", they mean minimize the tile, NOT kill the session.

`)


	panes, _ := findClaudePanes()

	// ── Agent overview with live terminal snapshots ──
	sb.WriteString(fmt.Sprintf("═══ %d ACTIVE AGENTS ═══\n\n", len(panes)))

	for _, p := range panes {
		status := "idle"
		activity := ""
		if hooks != nil {
			if hs := hooks.getStateForPane(p.Target, p.Dir); hs != nil {
				status = hs.Status
				activity = hs.Activity
			}
		}

		// Get pane dimensions
		cols := tmuxDisplayTarget(p.Target, "#{pane_width}", "?")
		rows := tmuxDisplayTarget(p.Target, "#{pane_height}", "?")

		sb.WriteString(fmt.Sprintf("┌─ %s (%s) ─────────────────────\n", p.Target, status))
		sb.WriteString(fmt.Sprintf("│ Session: %s  Dir: %s  Branch: %s\n", p.Session, p.DirName, p.Branch))
		sb.WriteString(fmt.Sprintf("│ Size: %sx%s  Path: %s\n", cols, rows, p.Dir))
		if activity != "" {
			sb.WriteString(fmt.Sprintf("│ Activity: %s\n", activity))
		}

		// Capture last 15 lines of terminal output
		out, err := exec.Command("tmux", "capture-pane", "-t", p.Target, "-p").Output()
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			// Take last 15 non-empty lines
			start := len(lines) - 15
			if start < 0 {
				start = 0
			}
			sb.WriteString("│ Terminal (last lines):\n")
			for _, line := range lines[start:] {
				trimmed := strings.TrimRight(line, " ")
				if trimmed != "" {
					sb.WriteString(fmt.Sprintf("│   %s\n", trimmed))
				}
			}
		}
		sb.WriteString("└──────────────────────────────────\n\n")
	}

	// ── Recent activity feed ──
	if feed != nil {
		recent := feed.getRecent(30)
		if len(recent) > 0 {
			sb.WriteString("═══ RECENT ACTIVITY (newest first) ═══\n")
			for i := len(recent) - 1; i >= 0; i-- {
				e := recent[i]
				if e.Event == "PostToolUse" {
					continue
				}
				sb.WriteString(fmt.Sprintf("  %s [%s] %s: %s\n",
					e.Time.Format("15:04:05"), e.Status, e.Session, e.Activity))
			}
			sb.WriteString("\n")
		}
	}

	// ── Pending approvals ──
	if feed != nil {
		pending := feed.getPending()
		if len(pending) > 0 {
			sb.WriteString("═══ ⚠ PENDING APPROVALS ═══\n")
			for _, p := range pending {
				sb.WriteString(fmt.Sprintf("  %s (%s/%s): %s\n", p.Target, p.Session, p.DirName, p.Activity))
			}
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

func handleMastermind(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	var mu sync.Mutex

	// Conversation history for context
	type chatMsg struct {
		Role string
		Text string
	}
	var history []chatMsg
	var activeCmd *exec.Cmd
	defer func() {
		if activeCmd != nil && activeCmd.Process != nil {
			activeCmd.Process.Kill()
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var input struct {
			Text  string `json:"text"`
			Model string `json:"model"`
		}
		if json.Unmarshal(msg, &input) != nil || input.Text == "" {
			continue
		}

		history = append(history, chatMsg{"user", input.Text})

		// Build prompt with conversation history
		systemPrompt := buildMastermindPrompt()

		var prompt strings.Builder
		if len(history) > 1 {
			prompt.WriteString("Previous conversation:\n")
			// Include last 10 exchanges max
			start := 0
			if len(history) > 20 {
				start = len(history) - 20
			}
			for _, m := range history[start : len(history)-1] {
				role := m.Role
				if len(role) > 0 {
					role = strings.ToUpper(role[:1]) + role[1:]
				}
				prompt.WriteString(fmt.Sprintf("%s: %s\n\n", role, m.Text))
			}
			prompt.WriteString("Current message:\n")
		}
		prompt.WriteString(input.Text)

		model := input.Model
		if model == "" {
			model = "haiku"
		}
		// Cancel previous request if still running
		if activeCmd != nil && activeCmd.Process != nil {
			activeCmd.Process.Kill()
			activeCmd = nil
		}

		cmd := exec.Command("claude", "-p",
			"--output-format", "stream-json",
			"--verbose",
			"--model", model,
			"--allowedTools", "Bash,Read,Grep,Glob",
			"--system-prompt", systemPrompt,
		)
		cmd.Stdin = strings.NewReader(prompt.String())

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			sendWS(&mu, conn, "error", "Failed to start Claude: "+err.Error())
			continue
		}

		if err := cmd.Start(); err != nil {
			sendWS(&mu, conn, "error", "Failed to start Claude: "+err.Error())
			continue
		}
		activeCmd = cmd

		// Send "thinking" indicator
		sendWS(&mu, conn, "status", "thinking")

		// Stream response
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer

		var fullResponse strings.Builder

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var event map[string]interface{}
			if json.Unmarshal([]byte(line), &event) != nil {
				continue
			}

			eventType, _ := event["type"].(string)

			switch eventType {
			case "assistant":
				if msg, ok := event["message"].(map[string]interface{}); ok {
					if content, ok := msg["content"].([]interface{}); ok {
						for _, c := range content {
							if block, ok := c.(map[string]interface{}); ok {
								if text, ok := block["text"].(string); ok {
									fullResponse.WriteString(text)
									sendWS(&mu, conn, "stream", text)
								}
								if block["type"] == "tool_use" {
									toolName, _ := block["name"].(string)
									toolInput, _ := block["input"].(map[string]interface{})
									detail := describeToolUse(toolName, toolInput)
									sendWS(&mu, conn, "tool", detail)
								}
							}
						}
					}
				}

			case "result":
				if result, ok := event["result"].(string); ok && fullResponse.Len() == 0 {
					sendWS(&mu, conn, "stream", result)
				}
				cost, _ := event["total_cost_usd"].(float64)
				if cost > 0 {
					finance.addMastermindCost(cost)
				}
				sendWS(&mu, conn, "done", fmt.Sprintf("%.4f", cost))
			}
		}

		cmd.Wait()

		if fullResponse.Len() == 0 {
			sendWS(&mu, conn, "error", "No response from Claude")
		} else {
			// Save assistant response to history
			history = append(history, chatMsg{"assistant", fullResponse.String()})
			// Keep history bounded
			if len(history) > 30 {
				history = history[len(history)-30:]
			}
		}
	}
}

func describeToolUse(name string, input map[string]interface{}) string {
	switch name {
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			if len(cmd) > 120 {
				cmd = cmd[:117] + "..."
			}
			return "$ " + cmd
		}
	case "Read":
		if fp, ok := input["file_path"].(string); ok {
			return "Read " + fp
		}
	case "Grep":
		if pat, ok := input["pattern"].(string); ok {
			return "Grep: " + pat
		}
	case "Glob":
		if pat, ok := input["pattern"].(string); ok {
			return "Glob: " + pat
		}
	case "Edit":
		if fp, ok := input["file_path"].(string); ok {
			return "Edit " + fp
		}
	case "Write":
		if fp, ok := input["file_path"].(string); ok {
			return "Write " + fp
		}
	}
	return "Using: " + name
}

func sendWS(mu *sync.Mutex, conn *websocket.Conn, msgType, data string) {
	mu.Lock()
	defer mu.Unlock()
	msg, _ := json.Marshal(map[string]string{"type": msgType, "data": data})
	conn.WriteMessage(websocket.TextMessage, msg)
}

