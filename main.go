package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	arg := ""
	if len(os.Args) >= 2 {
		arg = os.Args[1]
	}

	switch arg {
	case "--list":
		panes, err := findClaudePanes()
		if err != nil {
			fatal("failed to detect panes: %v", err)
		}
		if len(panes) == 0 {
			fatal("no Claude Code panes found")
		}
		fmt.Println()
		fmt.Printf("  %-22s %-20s %s\n", "TARGET", "DIRECTORY", "BRANCH")
		fmt.Printf("  %-22s %-20s %s\n", "──────────────────", "────────────────", "──────")
		for _, p := range panes {
			fmt.Printf("  %-22s %-20s %s\n", p.SessWin, p.DirName, p.Branch)
		}
		fmt.Println()

	case "--kill":
		tmuxRun("kill-session", "-t", dashSession)
		fmt.Println("▸ Dashboard destroyed")

	case "--web":
		port := 7685
		if len(os.Args) >= 3 {
			fmt.Sscanf(os.Args[2], "%d", &port)
		}
		runWeb(port)

	case "init":
		fmt.Println("▸ Installing Claude Wall hooks...")
		runInit()

	case "uninstall":
		fmt.Println("▸ Removing Claude Wall hooks...")
		runUninstall()

	case "":
		runDashboard()
	default:
		fmt.Fprintln(os.Stderr, "usage: claude-wall [--list | --kill | --web [port] | init | uninstall]")
		os.Exit(1)
	}
}

const dashSession = "claude-wall"

type claudePane struct {
	Target  string `json:"target"`  // e.g. Agent:1.2
	SessWin string `json:"sessWin"` // e.g. Agent:1
	Session string `json:"session"` // e.g. Agent
	Dir     string `json:"dir"`     // full path
	DirName string `json:"dirName"` // basename
	Branch  string `json:"branch"`
	Cols    string `json:"cols,omitempty"` // pane width
	Rows    string `json:"rows,omitempty"` // pane height
}

func findClaudePanes() ([]claudePane, error) {
	out, err := tmuxOutput(
		"list-panes", "-a", "-F",
		"#{session_name}:#{window_index}.#{pane_index}\t#{session_name}\t#{pane_current_path}\t#{pane_current_command}\t#{pane_title}",
	)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var panes []claudePane

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 5)
		if len(parts) < 5 {
			continue
		}
		target, session, dir, cmd, title := parts[0], parts[1], parts[2], parts[3], parts[4]

		if session == dashSession {
			continue
		}
		// Match "claude" command OR Claude Code version (e.g. "2.0.76")
		// Title check only if command is NOT a regular shell (stale titles persist after exit)
		shells := map[string]bool{"zsh": true, "bash": true, "sh": true, "fish": true}
		isClaude := cmd == "claude" ||
			isVersionString(cmd) ||
			(strings.Contains(title, "Claude Code") && !shells[cmd])
		if !isClaude {
			continue
		}

		// Dedup by session:window
		dotIdx := strings.LastIndex(target, ".")
		if dotIdx < 0 {
			continue
		}
		sessWin := target[:dotIdx]
		if seen[sessWin] {
			continue
		}
		seen[sessWin] = true

		branch := gitBranch(dir)
		dirName := baseName(dir)

		panes = append(panes, claudePane{
			Target:  target,
			SessWin: sessWin,
			Session: session,
			Dir:     dir,
			DirName: dirName,
			Branch:  branch,
		})
	}
	return panes, nil
}

func runDashboard() {
	panes, err := findClaudePanes()
	if err != nil {
		fatal("detection failed: %v", err)
	}
	if len(panes) == 0 {
		fatal("no Claude Code panes found")
	}

	fmt.Printf("▸ Found %d Claude Code instance(s)\n", len(panes))

	// Clean slate
	tmuxRun("kill-session", "-t", dashSession)

	// Get client size
	cw := tmuxDisplay("#{client_width}", "200")
	ch := tmuxDisplay("#{client_height}", "50")

	// Get helper path (same directory as this binary)
	helperPath := helperBinaryPath()

	// Build dashboard
	var dashWin string

	for i, p := range panes {
		label := fmt.Sprintf("🖥 %s  📂 %s  🌿 %s", p.Session, p.DirName, p.Branch)
		paneCmd := fmt.Sprintf("exec %s %s", helperPath, p.Target)

		if i == 0 {
			tmuxRun("new-session", "-d", "-s", dashSession, "-x", cw, "-y", ch, paneCmd)
			dashWin = tmuxDisplayTarget(dashSession, "#{window_index}", "1")
			firstPane := tmuxDisplayTarget(dashSession, "#{pane_id}", "")
			if firstPane != "" {
				tmuxRun("select-pane", "-t", firstPane, "-T", label)
				tmuxRun("set", "-p", "-t", firstPane, "allow-set-title", "off")
			}
		} else {
			newPane, err := tmuxOutput(
				"split-window", "-t", dashSession+":"+dashWin,
				"-P", "-F", "#{pane_id}", paneCmd,
			)
			if err != nil {
				fmt.Printf("▸ Terminal too small for all %d tiles.\n", len(panes))
				break
			}
			newPane = strings.TrimSpace(newPane)
			tmuxRun("select-pane", "-t", newPane, "-T", label)
			tmuxRun("set", "-p", "-t", newPane, "allow-set-title", "off")
			tmuxRun("select-layout", "-t", dashSession+":"+dashWin, "tiled")
		}
	}

	// Style
	tmuxRun("select-layout", "-t", dashSession+":"+dashWin, "tiled")
	tmuxRun("rename-window", "-t", dashSession+":"+dashWin, fmt.Sprintf("%d instances", len(panes)))

	tmuxRun("set-option", "-t", dashSession, "pane-border-status", "top")
	tmuxRun("set-option", "-t", dashSession, "pane-border-format", " #[fg=colour39,bold]#{pane_title}#[default] ")
	tmuxRun("set-option", "-t", dashSession, "pane-border-lines", "heavy")
	tmuxRun("set-option", "-t", dashSession, "pane-border-style", "fg=colour240")
	tmuxRun("set-option", "-t", dashSession, "pane-active-border-style", "fg=colour39")
	tmuxRun("set-option", "-t", dashSession, "mouse", "on")

	// Attach
	if err := tmuxExec("switch-client", "-t", dashSession); err != nil {
		tmuxExec("attach-session", "-t", dashSession)
	}
}

// helperBinaryPath returns the path to claude-wall-pane binary
// (expected in the same directory as the main binary)
func helperBinaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "claude-wall-pane"
	}
	dir := exe[:strings.LastIndex(exe, "/")+1]
	return dir + "claude-wall-pane"
}

// ─── tmux helpers ────────────────────────────────────────────

func tmuxRun(args ...string) {
	exec.Command("tmux", args...).Run()
}

func tmuxExec(args ...string) error {
	return exec.Command("tmux", args...).Run()
}

func tmuxOutput(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).Output()
	return string(out), err
}

func tmuxDisplay(format, fallback string) string {
	out, err := tmuxOutput("display-message", "-p", format)
	if err != nil || strings.TrimSpace(out) == "" {
		return fallback
	}
	return strings.TrimSpace(out)
}

func tmuxDisplayTarget(target, format, fallback string) string {
	out, err := tmuxOutput("display-message", "-t", target, "-p", format)
	if err != nil || strings.TrimSpace(out) == "" {
		return fallback
	}
	return strings.TrimSpace(out)
}

func gitBranch(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "-"
	}
	return strings.TrimSpace(string(out))
}

// isVersionString checks if cmd looks like a semver (e.g. "2.0.76")
// Claude Code sometimes shows its version as pane_current_command
func isVersionString(cmd string) bool {
	parts := strings.Split(cmd, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
		if len(p) == 0 {
			return false
		}
	}
	return true
}

func baseName(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return path
	}
	return path[idx+1:]
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\033[31merror:\033[0m "+format+"\n", args...)
	os.Exit(1)
}

// Prevent unused import
var _ = signal.Notify
var _ = syscall.SIGTERM
var _ = time.Second
