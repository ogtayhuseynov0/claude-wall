# Claude Wall — Roadmap

## High Value
- [x] Status indicators per tile (idle / working / waiting for permission)
- [x] Auto-detect new/removed Claude instances without page reload
- [x] Notification when a Claude instance needs attention (permission prompt, error)

## UX
- [ ] Drag to reorder/resize tiles
- [x] Tile minimize/maximize
- [x] Search/filter tiles by session or directory
- [x] Keyboard shortcuts (Ctrl+1-9 to focus tiles, Ctrl+Tab to cycle)
- [x] Sound/browser notification when a tile's Claude finishes a task

## Performance
- [x] Central capture hub — single goroutine for all pane captures
- [ ] Binary WebSocket messages instead of JSON (less overhead)
- [x] Delta updates (status-only messages when content unchanged)

## Features
- [x] Persist layout preferences in localStorage
