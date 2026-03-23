# Claude Wall

**Mission control for Claude Code — monitor all your AI coding agents from one dashboard.**

![Claude Wall Dashboard](https://github.com/user-attachments/assets/placeholder-screenshot.png)

## Features

- **Web Dashboard** — Real-time grid of all Claude Code instances in tmux
- **Live Terminal Capture** — See exactly what each agent is doing, streamed via WebSocket
- **Status Indicators** — Idle, working, or waiting-for-permission per tile
- **Activity Feed** — Chronological log of tool calls across all agents
- **Approval Queue** — See and act on pending permission requests
- **Mastermind AI** — Orchestrator that can read, instruct, and coordinate all agents
- **Chrome Extension** — Capture browser context and send it to any agent
- **Hooks Integration** — Claude Code hooks push real-time status (no polling needed)
- **Keyboard Shortcuts** — `Ctrl+1-9` to focus tiles, `/` to search, `?` for help

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
claude-wall init    # install Claude Code hooks (one-time)
claude-wall         # launch dashboard
```

The dashboard opens at `http://127.0.0.1:7685` and auto-detects all Claude Code instances running in tmux.

## CLI Usage

| Command | Description |
|---|---|
| `claude-wall` | Launch web dashboard (default port 7685) |
| `claude-wall 8080` | Launch on custom port |
| `claude-wall --list` | List detected Claude Code panes |
| `claude-wall --kill` | Destroy the tmux dashboard session |
| `claude-wall init` | Install Claude Code hooks for real-time status |
| `claude-wall uninstall` | Remove Claude Wall hooks from settings |

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                    Browser (Web UI)                  │
│  Grid of terminal tiles, activity feed, mastermind  │
└──────────────────────┬──────────────────────────────┘
                       │ WebSocket + HTTP
┌──────────────────────▼──────────────────────────────┐
│                  Go Server (claude-wall)             │
│                                                     │
│  Capture Hub ──► tmux capture-pane (100ms polling)  │
│  Hook Server ──► receives POST from hook scripts    │
│  Mastermind  ──► claude CLI with agent context      │
│  UI Commands ──► SSE stream for dashboard control   │
└──────────────────────┬──────────────────────────────┘
                       │
┌──────────────────────▼──────────────────────────────┐
│                 tmux sessions                        │
│  Each pane runs a Claude Code instance              │
│  capture-pane reads terminal content                │
│  send-keys forwards input                           │
└─────────────────────────────────────────────────────┘
```

**Data flow:**
1. The Go server polls `tmux capture-pane` for each detected Claude Code pane (100ms interval)
2. Content is diffed and streamed to connected browsers via WebSocket
3. Claude Code hooks (`PreToolUse`, `PostToolUse`, `Stop`, etc.) POST events to the server for precise status tracking
4. The Mastermind AI gets full context (terminal snapshots + activity feed) and can send commands back to agents

## Chrome Extension

The Chrome extension lets you capture the current page and send context to any Claude Code agent.

### Install

1. Open `chrome://extensions/`
2. Enable **Developer mode**
3. Click **Load unpacked** and select the `extension/` directory
4. Click the extension icon, configure the Claude Wall server URL (`http://127.0.0.1:7685`)

## Mastermind

The Mastermind is an orchestrator AI that has full visibility into all your agents. Open it from the dashboard sidebar.

**What it can do:**
- Summarize what all agents are working on
- Send instructions to any agent
- Approve pending permissions across agents
- Detect conflicts (two agents editing the same file)
- Control the dashboard UI (focus, zoom, minimize tiles)

It uses the `claude` CLI under the hood with a system prompt containing live terminal snapshots and activity context.

## Hooks Setup

`claude-wall init` installs a hook script that sends real-time events to the dashboard server. This gives you instant status updates (working/idle/permission) without relying on terminal parsing alone.

The init command:
1. Creates `~/.claude/hooks/claude-wall-hook.sh`
2. Adds hook entries to `~/.claude/settings.json` for `PreToolUse`, `PostToolUse`, `Stop`, and `Notification` events

To remove: `claude-wall uninstall`

## Requirements

- **Go 1.21+**
- **tmux 3.3+**
- **Claude Code** — running in tmux sessions

## License

[MIT](LICENSE) — Ogtay Huseynov
