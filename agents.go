package main

import "strings"

// Agents claude-wall recognizes in tmux panes. Claude Code and Codex report
// status through Claude Code hooks or the terminal; OpenCode and Pi report it
// through the small plugins that `claude-wall init` installs (integrations/).
const (
	agentClaude   = "claude"
	agentCodex    = "codex"
	agentOpenCode = "opencode"
	agentPi       = "pi"
)

var shellCmds = map[string]bool{"zsh": true, "bash": true, "sh": true, "fish": true}

// detectAgent names the coding agent running in a pane, or "" for any other
// program. tmux gives the foreground command and the title the program set.
// A title check alone is not enough: a shell keeps the title of the last
// program, and other programs can carry the words (nvim opencode.json,
// brew install …/opencode), so each title rule also checks the command.
func detectAgent(cmd, title string) string {
	if shellCmds[cmd] {
		return ""
	}
	lowTitle := strings.ToLower(title)
	switch {
	// Claude Code shows its version as the command (e.g. "2.1.330").
	case cmd == "claude" || isVersionString(cmd) || strings.Contains(title, "Claude Code"):
		return agentClaude
	// Pi runs on node and sets the title to "π - <folder>".
	case cmd == "pi" || title == "π" || strings.HasPrefix(title, "π - "):
		return agentPi
	// OpenCode is one binary; an npm install runs it under node or bun. Its
	// title is "OC | <session>" (V2) or names OpenCode.
	case strings.HasPrefix(cmd, "opencode") ||
		((cmd == "node" || cmd == "bun") && (strings.HasPrefix(title, "OC | ") || strings.Contains(lowTitle, "opencode"))):
		return agentOpenCode
	case cmd == "codex" || strings.Contains(lowTitle, "codex"):
		return agentCodex
	}
	return ""
}

// agentReportsInOrder is true for agents whose plugin posts its events one
// after another. Claude Code fires its HTTP hooks in parallel, so a late
// PostToolUse can arrive after Stop; the hook store guards against that, and
// the guard must not hold back the Stop of an agent that sends in order.
func agentReportsInOrder(agent string) bool {
	return agent == agentOpenCode || agent == agentPi
}
