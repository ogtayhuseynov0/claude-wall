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

// hookScriptContent is the shell script fallback for older Claude Code versions
// that don't support "type": "http" hooks.
var hookScriptContent = `#!/usr/bin/env bash
# Claude Wall hook (fallback for older Claude Code versions)
# Newer versions use native HTTP hooks — this is only needed for Claude Code < 2.1

WALL_PORT="${CLAUDE_WALL_PORT:-7685}"
WALL_URL="http://127.0.0.1:${WALL_PORT}/api/hooks/event"

EVENT_JSON=$(cat)

TMUX_PANE_ID="${TMUX_PANE}"
if [ -n "$TMUX_PANE_ID" ]; then
  PANE_ENCODED="${TMUX_PANE_ID//%/%25}"
  WALL_URL="${WALL_URL}?tmux_pane=${PANE_ENCODED}"
fi

curl -s -X POST "$WALL_URL" \
  -H "Content-Type: application/json" \
  -d "$EVENT_JSON" \
  --connect-timeout 1 \
  --max-time 2 \
  >/dev/null 2>&1 &
`

// hookEvents — all events we want to track
var hookEvents = []string{
	"PreToolUse", "PostToolUse", "PostToolUseFailure",
	"Stop", "StopFailure",
	"PermissionRequest", "Notification",
	"SessionStart", "SessionEnd",
	"SubagentStart", "SubagentStop",
	"TaskCompleted",
}

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

	// 2. Update settings.json (MERGE with existing hooks, never replace)
	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	settings := map[string]interface{}{}

	data, err := os.ReadFile(settingsPath)
	if err == nil {
		backupPath := settingsPath + ".backup." + time.Now().Format("20060102-150405")
		if err := os.WriteFile(backupPath, data, 0644); err != nil {
			fatal("cannot create backup: %v", err)
		}
		fmt.Printf("  Backed up settings to %s\n", filepath.Base(backupPath))

		if err := json.Unmarshal(data, &settings); err != nil {
			fatal("cannot parse settings.json: %v", err)
		}
	}

	hooksSection, _ := settings["hooks"].(map[string]interface{})
	if hooksSection == nil {
		hooksSection = map[string]interface{}{}
	}

	// Claude Code hook format:
	// "EventName": [ { "hooks": [ {"type":"command","command":"..."} ] } ]
	// We add our hook to existing groups or create a new group

	for _, event := range hookEvents {
		groups, _ := hooksSection[event].([]interface{})

		// Check if claude-wall hook already exists in any group
		alreadyExists := false
		for _, g := range groups {
			group, ok := g.(map[string]interface{})
			if !ok {
				continue
			}
			hooks, _ := group["hooks"].([]interface{})
			for _, h := range hooks {
				hook, ok := h.(map[string]interface{})
				if !ok {
					continue
				}
				if cmd, _ := hook["command"].(string); strings.Contains(cmd, hookScriptName) {
					alreadyExists = true
					break
				}
			}
			if alreadyExists {
				break
			}
		}

		if !alreadyExists {
			// Use native HTTP hook (Claude Code 2.1+) with command fallback
			port := "7685"
			if p := os.Getenv("CLAUDE_WALL_PORT"); p != "" {
				port = p
			}
			httpHook := map[string]interface{}{
				"type":           "http",
				"url":            fmt.Sprintf("http://127.0.0.1:%s/api/hooks/event", port),
				"timeout":        5,
				"allowedEnvVars": []string{"TMUX_PANE"},
				"headers": map[string]string{
					"X-Tmux-Pane": "$TMUX_PANE",
				},
			}
			// Also add command hook as fallback for older Claude Code
			cmdHook := map[string]interface{}{
				"type":    "command",
				"command": hookPath,
				"async":   true,
			}
			newGroup := map[string]interface{}{
				"hooks": []interface{}{httpHook, cmdHook},
			}
			groups = append(groups, newGroup)
			hooksSection[event] = groups
			fmt.Printf("  Added hook for %s (http + command fallback)\n", event)
		} else {
			fmt.Printf("  Hook for %s already exists, skipping\n", event)
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
		groups, ok := hooksSection[event].([]interface{})
		if !ok {
			continue
		}

		var filteredGroups []interface{}
		for _, g := range groups {
			group, ok := g.(map[string]interface{})
			if !ok {
				filteredGroups = append(filteredGroups, g)
				continue
			}

			hooks, _ := group["hooks"].([]interface{})
			var filteredHooks []interface{}
			for _, h := range hooks {
				hook, ok := h.(map[string]interface{})
				if !ok {
					filteredHooks = append(filteredHooks, h)
					continue
				}
				if cmd, _ := hook["command"].(string); strings.Contains(cmd, hookScriptName) {
					modified = true
					continue // remove claude-wall hook
				}
				filteredHooks = append(filteredHooks, h)
			}

			if len(filteredHooks) > 0 {
				group["hooks"] = filteredHooks
				filteredGroups = append(filteredGroups, group)
			}
			// If group has no hooks left, drop the whole group
		}

		if len(filteredGroups) == 0 {
			delete(hooksSection, event)
		} else {
			hooksSection[event] = filteredGroups
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
