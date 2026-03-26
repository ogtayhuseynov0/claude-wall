# Claude Wall

**Mission control for Claude Code — monitor and orchestrate all your AI coding agents from one dashboard.**

![Claude Wall Dashboard](screenshot.png)

## Features

- **Web Dashboard** — Real-time grid of all Claude Code instances in tmux
- **Live Terminal Capture** — See what each agent is doing, streamed via WebSocket
- **Status Indicators** — Idle (gray), working (green), permission needed (pulsing amber), error (red)
- **Activity Feed** — Chronological timeline of tool calls across all agents
- **Approval Queue** — One-click approve/deny pending permissions (Yes / Always / No)
- **Scheduled Tasks** — Send recurring commands to agents on a timer (review loops, test runs, deploys)
- **Mastermind AI** — Orchestrator chat that reads, instructs, and coordinates all agents
- **Cost Tracking** — Per-session token usage and estimated cost breakdown (`/finance.html`)
- **Chrome Extension** — Send selected text or full page content to any agent
- **Hooks Integration** — Native HTTP hooks (Claude Code 2.1+) with command fallback, 12 events
- **Keyboard Shortcuts** — `Ctrl+1-9` focus tiles, `Cmd+K` command palette, `Ctrl+Tab` cycle
- **Group Filter** — Filter tiles by project group or search by name
- **Daemon Mode** — Runs in background with `start`/`stop`/`restart`/`status`/`logs`
- **Remote Access** — `--public` flag binds to `0.0.0.0`, `--token` for authentication
- **Blur Mode** — Hide sensitive content on any tile with one click
- **Drag to Reorder** — Rearrange tiles, order persists across reloads
- **Auto-detect** — New/removed Claude instances appear automatically
- **Sound Notifications** — Alerts for permission requests and task completion
- **Mobile Responsive** — Usable from phone via Tailscale

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
| `claude-wall init` | Install hooks + start dashboard + open browser |
| `claude-wall start` | Start dashboard in background |
| `claude-wall stop` | Stop dashboard |
| `claude-wall restart` | Restart dashboard |
| `claude-wall status` | Check if running |
| `claude-wall logs` | Tail the dashboard log file |
| `claude-wall open` | Open dashboard in browser |
| `claude-wall list` | List detected Claude Code agents |
| `claude-wall uninstall` | Remove hooks + stop dashboard |

**Flags:**

| Flag | Description |
|---|---|
| `--public` | Bind to `0.0.0.0` for Tailscale/remote access |
| `--port PORT` | Custom port (default: `7685`) |
| `--token TOKEN` | Require authentication token (recommended with `--public`) |
| `--dry-run` | Preview `init` changes without applying them |

## Daemon Mode

Claude Wall runs as a background daemon. The server persists across terminal sessions.

```bash
claude-wall start                        # start in background
claude-wall start --public --token s3cr3t  # remote access with auth
claude-wall status                       # check if running
claude-wall logs                         # tail log output
claude-wall restart                      # restart the daemon
claude-wall stop                         # stop the daemon
```

The PID is stored at `~/.claude-wall.pid` and logs are written to `~/.claude/claude-wall.log`.

## Scheduled Tasks

Send recurring commands to agents on a timer — review loops, test runs, periodic checks.

**From the dashboard:** Side panel → Tasks tab → fill in agent, command, interval, max runs → Create.

**From the API:**
```bash
# Create a task: run /review-fix-loop every 10 min, max 5 attempts
curl -X POST http://localhost:7685/api/scheduler/create \
  -H "Content-Type: application/json" \
  -d '{"target":"RiverAI","command":"/review-fix-loop 617","intervalMin":10,"maxAttempts":5}'

# Pause / resume / stop / delete
curl -X POST http://localhost:7685/api/scheduler/{id}/pause
curl -X POST http://localhost:7685/api/scheduler/{id}/resume
curl -X POST http://localhost:7685/api/scheduler/{id}/stop
curl -X POST http://localhost:7685/api/scheduler/{id}/delete
```

Tasks wait for the agent to go idle before sending the next cycle. Progress shows as `⏱ 2/5` badge on the tile header. Tasks persist across server restarts.

## Dashboard Shortcuts

| Shortcut | Action |
|---|---|
| `Cmd+K` | Command palette |
| `Cmd+M` | Toggle Mastermind chat |
| `Ctrl+1-9` | Focus tile by number |
| `Ctrl+Tab` | Cycle between tiles |
| `Ctrl+F` | Filter/search tiles |
| Double-click header | Zoom tile fullscreen |
| `Escape` | Exit zoom |

## Mastermind AI

The 🧠 button (or `Cmd+M`) opens an orchestrator chat that can:

- Summarize what all agents are doing
- Send instructions to any agent
- Approve/deny permissions
- Control dashboard UI (focus, zoom, minimize tiles)
- Coordinate multi-agent workflows

Model selector: Haiku (fast/cheap), Sonnet, or Opus per message. Uses `claude -p` — no separate API key needed.

## Architecture

```
Browser <--WebSocket--> Go Server <--tmux--> Claude Code instances
                          |
                     Hook Server <-- Claude Code HTTP hooks
                          |
                     Mastermind --> claude CLI (orchestrator)
                          |
                     Scheduler --> timed commands to agents
```

1. Go server polls `tmux capture-pane` for each Claude pane (100ms)
2. Content streamed to browsers via WebSocket (only sends diffs)
3. Claude Code hooks push structured events (PreToolUse, Stop, PermissionRequest, etc.)
4. Mastermind AI gets full context and can control agents + dashboard UI
5. Scheduler sends recurring commands to agents when they go idle

## Chrome Extension

Send text from any webpage to any Claude Code agent.

1. Download `extension.zip` from [releases](https://github.com/ogtayhuseynov0/claude-wall/releases)
2. Unzip, open `chrome://extensions`, enable Developer mode
3. Click "Load unpacked", select the `extension/` folder
4. Select text → right-click → **Send selection to Agent**
5. Or right-click anywhere → **Send page text to Agent** (works on Notion, etc.)

## Cost Tracking

Visit `/finance.html` (or click 💰 in the header) for:

- Total cost across all agents + mastermind
- Per-session breakdown: input/output/cache tokens, estimated cost, turns
- Data persists across restarts at `~/.claude/claude-wall-finance.json`

## Hooks

`claude-wall init` installs hooks that send real-time events to the dashboard:

- **HTTP hooks** (Claude Code 2.1+) — native, fast, no shell script
- **Command hooks** (fallback) — for older Claude Code versions
- **12 events tracked**: PreToolUse, PostToolUse, PostToolUseFailure, Stop, StopFailure, PermissionRequest, Notification, SessionStart, SessionEnd, SubagentStart, SubagentStop, TaskCompleted
- Safe merge — never overrides existing hooks
- `claude-wall init --dry-run` to preview changes
- `claude-wall uninstall` removes only claude-wall hooks

## Requirements

- **tmux 3.3+**
- **Claude Code** running in tmux sessions
- **Go 1.21+** (only for building from source)

## License

[MIT](LICENSE)
