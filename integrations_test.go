package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestAgentPluginsInstallAndRemove(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(tmp, "pi"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("PATH", tmp) // no agent on PATH: only the config dirs count
	if err := os.MkdirAll(filepath.Join(tmp, "pi"), 0755); err != nil {
		t.Fatal(err)
	}
	// OpenCode is not "installed": no config dir, so no plugin for it.
	installAgentPlugins()

	want, _ := integrationFS.ReadFile("integrations/pi/claude-wall.ts")
	got, err := os.ReadFile(filepath.Join(tmp, "pi", "extensions", "claude-wall.ts"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("pi extension not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "config", "opencode")); !os.IsNotExist(err) {
		t.Fatalf("opencode plugin written without opencode: %v", err)
	}

	os.MkdirAll(filepath.Join(tmp, "config", "opencode"), 0755)
	installAgentPlugins()
	for _, f := range []string{"index.ts", "tui.ts"} {
		if _, err := os.Stat(filepath.Join(tmp, "config", "opencode", "plugins", "claude-wall", f)); err != nil {
			t.Fatalf("opencode %s: %v", f, err)
		}
	}

	uninstallAgentPlugins()
	for _, p := range []string{
		filepath.Join(tmp, "pi", "extensions", "claude-wall.ts"),
		filepath.Join(tmp, "config", "opencode", "plugins", "claude-wall"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still there after uninstall", p)
		}
	}
	// The agents' own folders stay.
	if _, err := os.Stat(filepath.Join(tmp, "pi", "extensions")); err != nil {
		t.Errorf("pi extensions dir removed: %v", err)
	}
}
