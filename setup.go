package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const hookScriptName = "claude-wall-hook.sh"

// hookScriptContent is the shell script that Claude Code hooks will execute.
// It POSTs hook event JSON to the claude-wall server.
var hookScriptContent = `#!/usr/bin/env bash
# Claude Wall hook — sends Claude Code events to the dashboard server
# Installed by: claude-wall init

WALL_PORT="${CLAUDE_WALL_PORT:-7685}"
WALL_URL="http://127.0.0.1:${WALL_PORT}/api/hooks/event"

# Read event JSON from stdin
EVENT_JSON=$(cat)

# Append tmux pane ID as query param for pane resolution
TMUX_PANE_ID="${TMUX_PANE}"
if [ -n "$TMUX_PANE_ID" ]; then
  WALL_URL="${WALL_URL}?tmux_pane=${TMUX_PANE_ID}"
fi

# Fire-and-forget POST (don't block Claude Code)
curl -s -X POST "$WALL_URL" \
  -H "Content-Type: application/json" \
  -d "$EVENT_JSON" \
  --connect-timeout 1 \
  --max-time 2 \
  >/dev/null 2>&1 &
`

// hookEvents are the Claude Code hook events we register for
var hookEvents = []string{"PreToolUse", "PostToolUse", "Stop", "Notification"}

func runInit() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fatal("cannot find home directory: %v", err)
	}

	// 1. Create hook script
	hooksDir := filepath.Join(homeDir, ".claude", "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		fatal("cannot create hooks directory: %v", err)
	}

	hookPath := filepath.Join(hooksDir, hookScriptName)
	if err := os.WriteFile(hookPath, []byte(hookScriptContent), 0755); err != nil {
		fatal("cannot write hook script: %v", err)
	}
	fmt.Printf("  Created %s\n", hookPath)

	// 2. Update settings.json
	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	settings := map[string]interface{}{}

	data, err := os.ReadFile(settingsPath)
	if err == nil {
		// Backup existing settings
		backupPath := settingsPath + ".backup." + time.Now().Format("20060102-150405")
		if err := os.WriteFile(backupPath, data, 0644); err != nil {
			fatal("cannot create backup: %v", err)
		}
		fmt.Printf("  Backed up settings to %s\n", filepath.Base(backupPath))

		if err := json.Unmarshal(data, &settings); err != nil {
			fatal("cannot parse settings.json: %v", err)
		}
	}

	// Get or create hooks section
	hooksSection, _ := settings["hooks"].(map[string]interface{})
	if hooksSection == nil {
		hooksSection = map[string]interface{}{}
	}

	hookCommand := hookPath

	for _, event := range hookEvents {
		existing, _ := hooksSection[event].([]interface{})

		// Check if claude-wall hook already exists
		alreadyExists := false
		for _, h := range existing {
			if hm, ok := h.(map[string]interface{}); ok {
				if cmd, ok := hm["command"].(string); ok && strings.Contains(cmd, hookScriptName) {
					alreadyExists = true
					break
				}
			}
		}

		if !alreadyExists {
			hookEntry := map[string]interface{}{
				"type":    "command",
				"command": hookCommand,
			}
			existing = append(existing, hookEntry)
			hooksSection[event] = existing
		}
	}

	settings["hooks"] = hooksSection

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		fatal("cannot serialize settings: %v", err)
	}

	if err := os.WriteFile(settingsPath, out, 0644); err != nil {
		fatal("cannot write settings.json: %v", err)
	}

	fmt.Printf("  Updated %s\n", settingsPath)
	fmt.Println()
	fmt.Println("  Claude Wall hooks installed. Restart Claude Code sessions to activate.")
	fmt.Println("  To remove: claude-wall uninstall")
}

func runUninstall() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fatal("cannot find home directory: %v", err)
	}

	// 1. Remove hook script
	hookPath := filepath.Join(homeDir, ".claude", "hooks", hookScriptName)
	if err := os.Remove(hookPath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("  Warning: could not remove %s: %v\n", hookPath, err)
	} else if err == nil {
		fmt.Printf("  Removed %s\n", hookPath)
	}

	// 2. Clean settings.json
	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		fmt.Println("  No settings.json found, nothing to clean.")
		return
	}

	// Backup
	backupPath := settingsPath + ".backup." + time.Now().Format("20060102-150405")
	if err := os.WriteFile(backupPath, data, 0644); err != nil {
		fatal("cannot create backup: %v", err)
	}
	fmt.Printf("  Backed up settings to %s\n", filepath.Base(backupPath))

	settings := map[string]interface{}{}
	if err := json.Unmarshal(data, &settings); err != nil {
		fatal("cannot parse settings.json: %v", err)
	}

	hooksSection, _ := settings["hooks"].(map[string]interface{})
	if hooksSection == nil {
		fmt.Println("  No hooks found in settings.")
		return
	}

	modified := false
	for _, event := range hookEvents {
		existing, ok := hooksSection[event].([]interface{})
		if !ok {
			continue
		}

		var filtered []interface{}
		for _, h := range existing {
			if hm, ok := h.(map[string]interface{}); ok {
				if cmd, ok := hm["command"].(string); ok && strings.Contains(cmd, hookScriptName) {
					modified = true
					continue // skip claude-wall entries
				}
			}
			filtered = append(filtered, h)
		}

		if len(filtered) == 0 {
			delete(hooksSection, event)
		} else {
			hooksSection[event] = filtered
		}
	}

	if !modified {
		fmt.Println("  No claude-wall hooks found in settings.")
		return
	}

	if len(hooksSection) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooksSection
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		fatal("cannot serialize settings: %v", err)
	}

	if err := os.WriteFile(settingsPath, out, 0644); err != nil {
		fatal("cannot write settings.json: %v", err)
	}

	fmt.Printf("  Updated %s\n", settingsPath)
	fmt.Println()
	fmt.Println("  Claude Wall hooks removed.")
}
