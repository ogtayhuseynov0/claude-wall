# Claude Wall

![views](https://gettrack.link/p/claude-wall/badge.svg?color=%2300a2ff&label_color=%23000000)

**Mission control for Claude Code — monitor, orchestrate, and interact with all your AI coding agents from one dashboard.**

![Claude Wall Dashboard](screenshot.png)

## Features

- **Web Dashboard** — Agent list on the left, a wall of live terminal tiles in the middle, an inbox on the right. Both sidebars collapse.
- **Free-form Wall** — Pick which agents go on the wall. Drag a tile edge to resize, drag a header to reorder, double-click an edge for an even grid
- **Live Terminal Capture** — 25fps streaming via tmux control mode (zero subprocess, ~2% CPU)
- **Agent States** — Working (green), waiting for you (red), recent (orange, stopped in the last 30 minutes), idle (gray). The same states show in the web dashboard, the phone view, the macOS menu bar app and push alerts
- **Status Rack** — One light per agent in the top bar, so you see the whole fleet at a glance
- **Scroll Back** — Wheel past the top of a tile to scroll Claude Code's own transcript (full-screen agents keep no tmux scrollback); one click jumps back to the latest output
- **Active / Recent / All** — One click puts the busy agents, the recently stopped agents, or every agent on the wall
- **Permission Mode Badge** — Shows current mode: default, plan, acceptEdits, bypassPermissions
- **Approval Queue** — One-click approve/deny pending permissions in the inbox and in the tile itself, with AskUserQuestion option buttons
- **Telegram Bot** — Approve permissions, answer questions, and send messages to agents from Telegram
- **Tasks View** — See all agents' Claude Code tasks and progress, continue an agent, assign work
- **Scheduled Tasks** — Send recurring commands to agents on a timer with progress tracking
- **Webhooks** — Slack, Discord, Telegram, or generic HTTP webhooks for permission, error, and completion events
- **Secrets Management** — Secure storage for bot tokens and API keys (chmod 600, masked in UI)
- **Voice Input** — Real-time speech-to-text per tile via Web Speech API (auto-stops on silence)
- **Mastermind AI** — Orchestrator chat that reads, instructs, and coordinates all agents
- **Usage and Cost** — Plan limits in the top bar, per-session token usage and estimated cost (`/finance.html`)
- **Chrome Extension** — Send selected text or full page content to any agent
- **Files and Images** — Drop or paste files and screenshots onto a tile; they upload and the agent gets the path
- **Hooks Integration** — Native HTTP hooks (Claude Code 2.1+) with command fallback, 12 events
- **Keyboard First** — `Cmd+K` command palette, `1-9` and `h j k l` to move, `Enter` to type, `?` for all shortcuts
- **Search and Filters** — Search agents, filter by project and by state
- **Daemon Mode** — Runs in background with `start`/`stop`/`restart`/`status`/`logs`
- **Remote Access** — `--public` flag binds to `0.0.0.0`, `--token` for authentication
- **Per-Pane Font Size** — Change the font size per tile from its menu
- **Blur Mode** — Hide sensitive content per tile
- **Auto-detect** — New/removed Claude instances appear automatically
- **Notifications** — Browser alerts and sounds for permission requests and finished work
- **Phone View** — A separate mobile page (`/m.html`), usable from your phone via Tailscale, with push alerts
- **Classic Dashboard** — The previous grid layout stays at `/classic.html`
- **macOS Code Signing** — Signed and notarized binaries, no Gatekeeper warnings

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

## Telegram Bot

Control your agents from your phone via Telegram. Approve permissions, answer questions, and send instructions — all without opening a terminal.

**Setup:**
1. Create a bot via [@BotFather](https://t.me/BotFather) and get the bot token
2. Message your bot to get your chat ID (open `https://api.telegram.org/bot<TOKEN>/getUpdates`)
3. In the dashboard: Tools → Webhooks and secrets → add the secrets `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID`
4. Create a Telegram webhook using `$TELEGRAM_BOT_TOKEN` and `$TELEGRAM_CHAT_ID`

**What you can do:**
- **Permission requests** arrive with `[Yes] [Always] [No]` inline buttons — tap to approve
- **AskUserQuestion** shows the actual options as buttons — tap to select and approve in one step
- **Reply to any notification** to send text to that agent's pane
- `/agents` — list active agents, pick one, then type a message to send
- `/send AgentName your message here` — send directly to an agent

The bot auto-starts when a Telegram webhook with valid credentials exists. Rich notifications include session name, folder, branch, and last user prompt.

## Tasks View (Project Manager)

See what each Claude Code agent is working on. The Tasks tab in the inbox reads Claude Code's internal task system (`~/.claude/tasks/`) and shows:

- **Per-tile progress badge** — `3/7` in the tile header (completed/total)
- **Tasks tab** in the inbox — all agents grouped with progress bars
- **Task list** — expand any agent to see individual tasks (pending, in progress, completed)
- **Continue button** — send `/continue` to any agent
- **Assign work** — tap a pending task to send it to the agent

## Scheduled Tasks

Send recurring commands to agents on a timer — review loops, test runs, periodic checks.

**From the dashboard:** Inbox → Schedules tab → fill in agent, command, interval, max runs → Create.

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

## Webhooks

Get notified in Slack, Discord, Telegram, or any HTTP endpoint when agents need attention.

**From the dashboard:** Tools → Webhooks and secrets → add URL, select type, choose events → Create.

**Supported types:**
- **Telegram** — rich HTML notifications with inline action buttons (requires bot token + chat ID stored in Secrets)
- **Slack** — sends `{"text": "message"}` to your webhook URL
- **Discord** — sends `{"content": "message"}` to your webhook URL
- **Generic** — sends full JSON event payload to any HTTP endpoint

**Events:** `permission` (agent needs approval), `error` (agent failure), `stopped` (agent completed), `task_completed` (scheduled task finished)

**Management:** Enable/disable toggle, test button, delete with confirmation per webhook.

Webhooks are debounced (max 1 per event type per 30s) and persisted to `~/.claude/claude-wall-webhooks.json`.

## Secrets Management

Store sensitive values (bot tokens, API keys) securely on disk.

- Stored at `~/.claude/claude-wall-secrets.json` with `chmod 600`
- Values **never returned to the browser** — UI shows masked versions (e.g. `85****Cg`)
- Reference in webhook fields as `$SECRET_NAME` — resolved server-side at send time
- Add/delete from Tools → Webhooks and secrets

## Voice Input

Click the mic button on any tile to dictate text using your browser's speech recognition.

- Real-time transcription — words appear as you speak
- Auto-stops after 3 seconds of silence
- Text is typed into the pane without pressing Enter (review first)
- Click mic again to stop manually
- Works in Chrome and Edge (Web Speech API)

## Dashboard Shortcuts

Press `?` in the dashboard for the full list.

| Shortcut | Action |
|---|---|
| `Cmd+K` | Search agents or run a command |
| `Cmd+M` | Toggle Mastermind chat |
| `Ctrl+1-9` | Focus agent by number (also while typing in a tile) |
| `1-9`, `h j k l` | Select an agent (when not typing) |
| `Enter` | Start typing in the selected agent |
| `Shift+Esc` | Leave the tile, so plain keys work again |
| `Ctrl+Tab` / `Ctrl+Shift+Tab` | Cycle between tiles |
| `Alt+Z` | Expand or collapse the selected tile |
| `Alt+M` | Take the selected tile off the wall, or put it back |
| `p` / `n` / `t` | Wall on/off, new session, open in terminal |
| `[` / `]` | Show or hide the agent list / the inbox |
| `Ctrl+F` | Search agents |
| Double-click header | Expand tile |

## Mastermind AI

The Mastermind button in the top bar (or `Cmd+M`) opens an orchestrator chat that can:

- Summarize what all agents are doing
- Send instructions to any agent
- Approve/deny permissions
- Control dashboard UI (focus, expand, take tiles off the wall, jump to the latest output)
- Coordinate multi-agent workflows

Model selector: Haiku (fast/cheap), Sonnet, or Opus per message. Uses `claude -p` — no separate API key needed.

## Architecture

```
Browser <--WebSocket--> Go Server <--tmux -C--> tmux server
                          |
                     Hook Server <-- Claude Code HTTP hooks
                          |
                     Telegram Bot <--> Telegram API (long poll)
                          |
                     Mastermind --> claude CLI (orchestrator)
                          |
                     Scheduler --> timed commands to agents
```

1. **tmux Control Mode** — persistent `tmux -C` connection, zero subprocess spawning
2. Captures all panes at 25fps via protocol messages (not `exec.Command`)
3. Content streamed to browsers via WebSocket (only sends diffs)
4. Claude Code hooks push structured events (PreToolUse, Stop, PermissionRequest, etc.)
5. Telegram bot enables remote approval and messaging via long-polling
6. Mastermind AI gets full context and can control agents + dashboard UI
7. Scheduler sends recurring commands to agents when they go idle
8. CPU usage: ~2-5% for 10+ agents (vs 10%+ with subprocess polling)

## Chrome Extension

Send text from any webpage to any Claude Code agent.

1. Download `extension.zip` from [releases](https://github.com/ogtayhuseynov0/claude-wall/releases)
2. Unzip, open `chrome://extensions`, enable Developer mode
3. Click "Load unpacked", select the `extension/` folder
4. Select text → right-click → **Send selection to Agent**
5. Or right-click anywhere → **Send page text to Agent** (works on Notion, etc.)

## Cost Tracking

Visit `/finance.html` (or click the cost button in the header) for:

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
