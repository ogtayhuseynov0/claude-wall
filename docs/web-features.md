# Web dashboard: feature inventory

All features of the first desktop web UI ("v1", now `static/classic.html`), with how each one works.
It is also the checklist for the v2 web UI (`static/index.html`, the default page since 2026-11-24): v2 must keep every row.
The last column says where the feature lives in v2.

Mobile-only features (`m.html`, `pip.html`) are in section 11. v2 brings them to the desktop.

## 1. Page

| Feature | How it works (v1) | v2 place |
|---|---|---|
| Phone redirect | On a small touch screen (≤640 px and coarse pointer), `/` goes to `/m.html`. `?desktop=1` forces the desktop page and saves `cw-force-desktop` in localStorage. | Same script in `index.html`. |
| Auto-detect agents | `GET /api/panes` at start and every 5 s (only while the page shows). New panes get a tile; removed panes fade out. | Same; also feeds the roster. |
| Hidden-tab savings | Polls run only while the page shows (`pollVisible`). Tile frames render once per animation frame, in batches. | Same. |

## 2. Header (top bar)

| Feature | How it works (v1) | v2 place |
|---|---|---|
| Instance count | "17 instances" from the number of tiles. | Top bar fleet strip + counts. |
| Status summary | Counts tiles by status every 1 s: working, perm, idle. | Roster chips with counts: Waiting, Working, Recent, Idle (Recent comes from `/api/summary`). |
| Group filter | Dropdown of project groups. Group = the folder after `PROJECTS`/`WORK`/`PERSONAL`/`DESKTOP` in the pane path, else the parent folder. Hides tiles of other groups. | Roster filter (applies to the roster and the wall). |
| System metrics | `/ws/system` stream: CPU, RAM, SSD bars (amber ≥70 %, red ≥90 %), network ↓↑ rate. Tooltips show totals and core count. Hidden below 760 px. | Top bar meters, plus GPU from `/api/sysstats` (mobile had it). |
| Filter box (⌃F) | Matches the tile header text. Enter selects the first match; ⌘Enter opens it in the terminal; Esc clears. | Roster search (same keys). |
| Alerts toggle 🔔 | localStorage `cw-notifs`. When on: browser notification + short beep when a tile needs permission, and when a tile goes from working to idle after more than 5 s. Also POSTs `/api/notifications`, but the server has no such route (404). | Settings → Alerts in this browser. The dead POST is removed. |
| Finance 💰 | Link to `/finance.html`. | Tools menu → Cost and usage history. |
| Tile header elements ⚙ | Checkboxes hide: tile number, session, directory, branch, pane ID, permission mode, status label. Saved in `cw-header-vis`. | Settings → Tile header. |
| Reload ↻ | Reloads the page. | Tools menu + command palette. |

## 3. Wall (tiles)

| Feature | How it works (v1) | v2 place |
|---|---|---|
| One tile per agent pane | Each tmux pane with Claude Code or Codex gets a tile and its own `/ws/<target>` websocket (jittered reconnect, 0.5 s → 10 s). | Same, for agents on the wall. Agents off the wall stay in the roster (websocket kept for status). |
| Free-form layout | Tiles are rectangles in 0..1 units. Default: near-square grid; the last tile fills its row. Drag any edge or corner: only the tiles that share that edge move (min 160×90 px). Double-click an edge: even grid again. Saved per tile count (`cw-rects-<n>`). | Same. |
| Reorder | Drag a tile header (SortableJS). Order saved in `cw-tile-order`. | Same. |
| Tile header | Status dot, number, agent mark (Claude ✳ / Codex ◇), session, folder, branch, pane ID, task progress (done/total), schedule badge (⏱ runs/max, tooltip with the command and interval), permission mode (bypass / plan / edits / default), status label. | Same data, one clean line, no emoji. Live activity text is shown (v1 had the element but never filled it). |
| Header buttons | Open in terminal ↗ (`/api/goto/<target>`), minimize ▁, more ⋯. | Same + restart. |
| More menu | Expand, Blur content (blurs the body and the header text), font A−/A+ (6–24 px, not saved), pane info (columns · target). | Same, font size saved per pane, plus the pane and session actions below. |
| Expand | Double-click the header, ⌥Z, or the menu. The tile fills the window. Esc (typed in the tile) or ⌥Z collapses. ⌥arrows carry the expanded state to the next tile. | Same; the tile fills the wall area, so the top bar and roster stay visible. |
| Status | From the websocket. Permission and working show at once; leaving working waits 3 s. Borders: amber pulse = permission, green tint = working, red = error, green ring = selected. | Same rules; colours match mobile (red-pink = waiting, green = working, orange = recent). |
| Terminal body | Scrollback ("history") + live screen. ANSI colours, OSC-8 links stripped, horizontal rules. History streams only for the selected or expanded tile ("history mode"). Keeps scroll position; follows when at the bottom. No render while minimized or hidden. | Same. |
| Typing | Click a tile, then type: printable keys, Ctrl+letter, Enter, Backspace, Tab/Shift+Tab, Esc, arrows, PageUp/Down, Home/End, Delete. Paste sends clipboard text. | Same. |
| Scroll to bottom ↓ | Floating button when scrolled up. | Same. |
| Voice input 🎤 | Web Speech API (en-US). Types the words as you speak; stops after 3 s of silence or on a second click. | Same. |
| Drop files | Drop files or folders on a tile: find the real path (`/api/resolve-path`, mdfind) or upload (`/api/upload`), then type the paths (quoted if they have spaces). | Same + an Attach button (file picker, from mobile). |
| Close session ✕ | Hover button → `confirm()` → Ctrl+C twice → tile removed after 2 s. | More menu → Close session, with an in-page confirm. |
| Desktop alerts | "X needs attention" on permission; "X finished" after more than 5 s of work. | Same. |

## 4. Minimized strip

| Feature | How it works (v1) | v2 place |
|---|---|---|
| Minimize / restore | ▁ button, ⌥M, or Mastermind. The tile moves to a strip above the wall (newest first), header only, and keeps its status. Saved per pane (`cw-tile-<target>`). | "Off the wall": the agent stays in the roster. The roster row toggles it back. |
| Strip expand | Collapsed = one scrolling line; expanded = wraps. Saved in `cw-min-expanded`. | Not needed: the roster shows every agent. |

## 5. Side panel (right, 320 px, ◀ ▶ toggle)

| Feature | How it works (v1) | v2 place |
|---|---|---|
| Approvals | `GET /api/approvals` every 2 s. Each item: session, folder, branch, tool. Yes (sends `1`), Always (`2`), No (Esc), Focus. Clears with `/api/approvals/clear/<target>`. Badge count on the tab. | Inbox panel + an approval bar inside the waiting tile. |
| Questions (AskUserQuestion) | Question text, one button per option (sends its number), Other… (sends `1`, then option N+1 after 1.5 s, then the typed text after 2.5 s), Deny, Focus. | Same, in the Inbox and the tile bar. |
| PM (Claude Code tasks) | `GET /api/tasks` every 5 s. Per agent: progress bar (done + active), done/total, ▶ sends `/continue` (`/api/tasks/<t>/continue`). Expand: tasks in progress, pending, done. A pending task's → button assigns it (`/api/tasks/<t>/work-on`). Also feeds the tile badge. | Inbox panel → Tasks tab. |
| Scheduled tasks | Form: agent, command, every N min, max runs → `/api/scheduler/create`. List: status, runs/max, command, interval, empty count, next run. Pause, Resume, Stop, Delete (`/api/scheduler/<id>/<action>`). | Inbox panel → Schedules tab. |
| Webhooks | Form: type (Telegram, Slack, Discord, Generic JSON), URL or bot token (or a `$SECRET`), chat ID for Telegram, events (permission, error, stopped, task_completed) → `POST /api/webhooks`. List: type, masked URL, events, Test, Enable/Disable, Delete (confirm). | Settings → Webhooks. |
| Secrets | Masked list, + Add (NAME, value), Delete (confirm). `$NAME` can be used in webhook fields. | Settings → Secrets. |
| Activity feed | Was a tab (tool-call timeline from `/api/feed`). Removed from the page in commit ae7b0a7; the API still works. | Deprecated (owner decision 2026-11-24): not in v2, and v2 does not call `/api/feed`. The server's event buffer stays, because the approval queue and Mastermind's context read from it. |

## 6. Command palette (⌘K)

Jump to tile (search session · folder · branch; ⌘Enter = open in terminal), expand selected tile, scroll all to bottom, open selected tile in terminal, alerts on/off, reload, keyboard shortcuts, send text to the selected tile (`prompt()`), broadcast text to all tiles (`prompt()`).

v2: same commands, plus every Tools menu item. Send and broadcast use an in-page dialog, not `prompt()`.

## 7. Keyboard

| Keys | Action |
|---|---|
| ⌃1–⌃9 | Focus tile 1–9 (also while typing in a tile) |
| ⌥← ⌥→ / ⌥↑ ⌥↓ | Previous / next tile; tile above / below |
| ⌃` | Open the selected tile in the terminal |
| ⌥Z / ⌥M | Expand / minimize the selected tile |
| ⇧Esc | Leave the tile so plain keys work |
| ⌃Tab / ⌃⇧Tab | Cycle tiles |
| 1–9, h j k l | Select a tile (plain keys, not typing) |
| t / Enter | Open in terminal / start typing in the selected tile |
| ? or ⌃/ | Shortcut help |
| ⌘K / ⌃F / ⌘M | Palette / search / Mastermind |

v2 keeps all of them and adds: `n` new session, `p` put the selected agent on or off the wall, `[` show or hide the roster, `]` show or hide the inbox.

## 8. Mastermind (⌘M)

Drawer (400 px). Model: Haiku, Sonnet or Opus per message. `/ws/mastermind`: streaming markdown (sanitized with DOMPurify), tool lines, status lines, errors, cost per reply. Enter sends.

## 9. Remote UI control

`EventSource /api/ui/events`: actions focus, zoom, minimize, restore, scroll-bottom for a target. Mastermind uses it. v2 keeps it (minimize/restore = off/on the wall).

## 10. Other pages

`/finance.html` (cost: totals, daily cards, year heatmap, last 30 days, by model, by project, Mastermind, per-session table, day detail). `/log` (Fleet Log). `/ports` (local ports, open a port through `/proxy/`). `/browser` (remote Chrome view). Only Finance is linked from the v1 desktop page. v2 links all of them.

## 11. Mobile-only features (no desktop UI in v1)

| Feature | How it works (mobile) | v2 place |
|---|---|---|
| Recent status | Was "stale" (idle 30 min or more). Now `/api/summary` gives `recent`: the agent worked and stopped less than 30 min ago (`recentWindow` in `web.go`); after that it is `idle`. Orange. | Same rule in v2, mobile, the Mac app (button badge, menu bar, panel, picker) and the push badge. |
| Status filter chips | All / Working / Waiting / Recent / Idle. | Roster chips. |
| Favorites ★ | Pinned to the top of the list. `cw_favs`. | Roster star (same key, shared with mobile on the same browser). |
| Attention sort | Working → waiting → recently stopped → never worked. | Roster: waiting → working → recent → idle (newest stop first). |
| Compaction progress | A trailing `NN%` in the activity text draws a bar. | Roster row and tile header. |
| Account chip | `claude · work` profile. | Roster row. |
| New session | Agent: Claude / Claude · work / Codex. Project search (`/api/projects`), start (`/api/launch`). | Top bar button + `n`. |
| Usage limits | `/api/limits`: 5-hour and weekly windows per account, reset times, refresh. | Top bar Usage popover. |
| Push notifications | Service worker + VAPID (`/api/push/*`): on/off, per event (permission, stopped, errors), quiet hours, muted sessions with unmute, test. | Settings → Push notifications. Tile menu → Mute 1 h. |
| Fleet Log | `/log`. | Tools menu. |
| Shared drive | Browse, upload, download, delete (second click), copy path (`/api/drive*`). | Tools menu → Drive dialog. |
| Camera | Take a picture at the Mac, history by day, viewer, delete one / all (`/api/camera*`). | Tools menu → Camera dialog. |
| Local ports / Remote browser | `/ports`, `/browser`. | Tools menu. |
| Rebuild & restart server | `/api/redeploy`, then waits for `/api/health` and reloads. | Tools menu. |
| Lock this Mac | `/api/lock`. | Tools menu. |
| Restart & resume agent | Pick account (Claude / Claude · work / Codex), preselected from `/api/restart-agent/<t>`. | Tile header button + menu. |
| Sibling panes | Panes in the same tmux window (`/api/panes/<t>`), new pane (`/api/pane/new`), remove pane (`/api/pane/kill`, second click). | Tile menu. |
| Attach file / photo | Hash lookup (`/api/upload/lookup`), image downscale, upload with progress, types the paths. | Tile Attach button. |
| Version self-heal | `/api/version`; reload once when the front end changed. | Same. |
