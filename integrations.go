package main

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// OpenCode and Pi have no Claude Code hooks; a small plugin for each posts
// their events to /api/hooks/event instead (see integrations/). `claude-wall
// init` installs the plugin for each agent it finds, `uninstall` removes it.

//go:embed integrations/pi/claude-wall.ts integrations/opencode/claude-wall/index.ts integrations/opencode/claude-wall/tui.ts
var integrationFS embed.FS

type agentPlugin struct {
	name  string            // shown in the init output
	bin   string            // the command that shows the agent is installed
	dir   func() string     // the agent's config directory
	files map[string]string // embedded file → path inside dir
	owned string            // path inside dir that uninstall removes
}

var agentPlugins = []agentPlugin{
	{
		name: "Pi",
		bin:  "pi",
		dir: func() string {
			if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
				return d
			}
			return filepath.Join(os.Getenv("HOME"), ".pi", "agent")
		},
		files: map[string]string{"integrations/pi/claude-wall.ts": "extensions/claude-wall.ts"},
		owned: "extensions/claude-wall.ts",
	},
	{
		name: "OpenCode",
		bin:  "opencode",
		dir: func() string {
			if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
				return filepath.Join(d, "opencode")
			}
			return filepath.Join(os.Getenv("HOME"), ".config", "opencode")
		},
		files: map[string]string{
			"integrations/opencode/claude-wall/index.ts": "plugins/claude-wall/index.ts",
			"integrations/opencode/claude-wall/tui.ts":   "plugins/claude-wall/tui.ts",
		},
		owned: "plugins/claude-wall",
	},
}

// installed is true when the agent's command is on PATH or its config
// directory exists.
func (p agentPlugin) installed() bool {
	if _, err := exec.LookPath(p.bin); err == nil {
		return true
	}
	_, err := os.Stat(p.dir())
	return err == nil
}

// installAgentPlugins writes each plugin whose agent is installed. It leaves a
// file alone when it already holds the same content.
func installAgentPlugins() {
	for _, p := range agentPlugins {
		if !p.installed() {
			continue
		}
		changed := false
		for src, rel := range p.files {
			data, err := integrationFS.ReadFile(src)
			if err != nil {
				fatal("missing embedded %s: %v", src, err)
			}
			dst := filepath.Join(p.dir(), rel)
			if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, data) {
				continue
			}
			changed = true
			if dryRun {
				fmt.Printf("  [dry-run] Would write %s\n", dst)
				continue
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				fatal("cannot create %s: %v", filepath.Dir(dst), err)
			}
			if err := os.WriteFile(dst, data, 0644); err != nil {
				fatal("cannot write %s: %v", dst, err)
			}
			fmt.Printf("  Wrote %s\n", dst)
		}
		if changed && !dryRun {
			fmt.Printf("  %s plugin installed. Restart %s sessions to activate.\n", p.name, p.name)
		}
	}
}

// uninstallAgentPlugins removes the plugin files that init wrote.
func uninstallAgentPlugins() {
	for _, p := range agentPlugins {
		path := filepath.Join(p.dir(), p.owned)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			fmt.Printf("  Warning: could not remove %s: %v\n", path, err)
			continue
		}
		fmt.Printf("  Removed %s\n", path)
	}
}
