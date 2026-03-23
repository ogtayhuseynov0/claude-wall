# Claude Wall

**Mission control for Claude Code — monitor and orchestrate all your AI coding agents from one dashboard.**

![Claude Wall Dashboard](screenshot.png)

## Features

- **Web Dashboard** — Real-time grid of all Claude Code instances in tmux
- **Live Terminal Capture** — See what each agent is doing, streamed via WebSocket
- **Status Indicators** — Idle (gray), working (green), permission needed (pulsing amber)
- **Activity Feed** — Chronological timeline of tool calls across all agents
- **Approval Queue** — One-click approve/deny pending permissions (Yes / Always / No)
- **Mastermind AI** — Orchestrator chat that reads, instructs, and coordinates all agents
- **Chrome Extension** — Send selected text or full page content to any agent
- **Hooks Integration** — Native HTTP hooks (Claude Code 2.1+) with command fallback
- **Keyboard Shortcuts** — `Ctrl+1-9` focus tiles, `Cmd+K` command palette, `Ctrl+Tab` cycle
- **Daemon Mode** — Runs in background, `start`/`stop`/`restart`/`status`
- **Remote Access** — `--public` flag binds to `0.0.0.0` for Tailscale/remote
- **Blur Mode** — Hide sensitive content on any tile with one click
- **Session Summary** — Hover tile header for status, recent actions, duration
- **Drag to Reorder** — Rearrange tiles, order persists across reloads
- **Auto-detect** — New/removed Claude instances appear automatically
- **Sound Notifications** — Distinct alerts for permission and task completion

## Install

**Homebrew (macOS & Linux):**
```bash
brew tap ogtayhuseynov0/tap
brew install claude-wall
```

**Binary download:**
```bash
# macOS (Apple Silicon)
curl -L https://github.com/ogtayhuseynov0/claude-wall/releases/latest/download/claude-wall-macos-arm64 -o ~/.local/bin/claude-wall
chmod +x ~/.local/bin/claude-wall

# macOS (Intel)
curl -L https://github.com/ogtayhuseynov0/claude-wall/releases/latest/download/claude-wall-macos-amd64 -o ~/.local/bin/claude-wall
chmod +x ~/.local/bin/claude-wall

# Linux (amd64)
curl -L https://github.com/ogtayhuseynov0/claude-wall/releases/latest/download/claude-wall-linux-amd64 -o ~/.local/bin/claude-wall
chmod +x ~/.local/bin/claude-wall
```

**From source:**
```bash
go install github.com/ogtayhuseynov0/claude-wall@latest
```

## Quick Start

```bash
claude-wall init    # install hooks + start dashboard + open browser
```

## CLI

| Command | Description |
|---|---|
| `claude-wall init` | Install hooks + start + open browser |
| `claude-wall start` | Start dashboard in background |
| `claude-wall stop` | Stop dashboard |
| `claude-wall restart` | Restart dashboard |
| `claude-wall status` | Check if running |
| `claude-wall open` | Open dashboard in browser |
| `claude-wall list` | List detected agents |
| `claude-wall uninstall` | Remove hooks + stop |
| `claude-wall start --public` | Bind to 0.0.0.0 (Tailscale/remote) |
| `claude-wall start --port 8080` | Custom port |

## Dashboard Shortcuts

| Shortcut | Action |
|---|---|
| `Cmd+K` | Command palette |
| `Ctrl+1-9` | Focus tile by number |
| `Ctrl+Tab` | Cycle between tiles |
| `Ctrl+F` | Filter/search tiles |
| `Cmd+M` | Toggle Mastermind chat |
| Double-click header | Zoom tile fullscreen |
| `Escape` | Exit zoom |

## Architecture

```
Browser ◄──WebSocket──► Go Server ◄──tmux──► Claude Code instances
                          │
                     Hook Server ◄── Claude Code HTTP hooks
                          │
                     Mastermind ──► claude CLI (orchestrator)
```

1. Go server polls `tmux capture-pane` for each Claude pane (100ms)
2. Content streamed to browsers via WebSocket (only sends changes)
3. Claude Code hooks push structured events (PreToolUse, Stop, PermissionRequest, etc.)
4. Mastermind AI gets full context and can control agents + dashboard UI

## Chrome Extension

Send text from any webpage to any Claude Code agent.

1. Download `extension.zip` from [releases](https://github.com/ogtayhuseynov0/claude-wall/releases)
2. Unzip, open `chrome://extensions`, enable Developer mode
3. Click "Load unpacked", select the `extension/` folder
4. Select text → right-click → **Send to Agent**
5. Or right-click anywhere → **Send page text to Agent** (works on Notion, etc.)

## Mastermind

The 🧠 button (or `Cmd+M`) opens an AI orchestrator chat that can:

- Summarize what all agents are doing
- Send instructions to any agent
- Approve/deny permissions
- Control dashboard UI (focus, zoom, minimize tiles)
- Coordinate multi-agent workflows

Model selector: Haiku (fast/cheap), Sonnet, or Opus per message.

## Hooks

`claude-wall init` installs hooks that send real-time events to the dashboard:

- **HTTP hooks** (Claude Code 2.1+) — native, fast, no shell script
- **Command hooks** (fallback) — for older Claude Code versions
- **12 events tracked**: PreToolUse, PostToolUse, PostToolUseFailure, Stop, StopFailure, PermissionRequest, Notification, SessionStart, SessionEnd, SubagentStart, SubagentStop, TaskCompleted
- Safe merge — never overrides existing hooks
- `claude-wall uninstall` removes only claude-wall hooks

## Requirements

- **tmux 3.3+**
- **Claude Code** running in tmux sessions
- **Go 1.21+** (only for building from source)

## License

[MIT](LICENSE)
