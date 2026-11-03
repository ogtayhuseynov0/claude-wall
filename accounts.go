package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The work account is a second Claude Code config dir (CLAUDE_CONFIG_DIR) signed
// in to another account. CLAUDE_WALL_WORK_CONFIG_DIR names it, or "off" for none.
// Otherwise it is the ~/.claude-* dir whose .claude.json has a signed-in account,
// the first by name when several do (set the variable to choose another).

var workDirCache struct {
	sync.Mutex
	dir string
	at  time.Time
}

// workConfigDir is the work account's config dir, "" when there is none. Cached
// briefly because the usage and pane views ask often.
func workConfigDir() string {
	workDirCache.Lock()
	defer workDirCache.Unlock()
	if !workDirCache.at.IsZero() && time.Since(workDirCache.at) < 30*time.Second {
		return workDirCache.dir
	}
	workDirCache.dir, workDirCache.at = findWorkConfigDir(), time.Now()
	return workDirCache.dir
}

func findWorkConfigDir() string {
	home, _ := os.UserHomeDir()
	if env := strings.TrimSpace(os.Getenv("CLAUDE_WALL_WORK_CONFIG_DIR")); env != "" {
		if strings.EqualFold(env, "off") {
			return ""
		}
		if rest, ok := strings.CutPrefix(env, "~/"); ok {
			env = filepath.Join(home, rest)
		}
		return filepath.Clean(env)
	}
	dirs, _ := filepath.Glob(filepath.Join(home, ".claude-*")) // sorted by name
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() && signedIn(filepath.Join(d, ".claude.json")) {
			return d
		}
	}
	return ""
}

// workOwnPath is rel inside the work config dir, or "" when it is missing or is
// the personal ~/.claude one (config dirs often symlink projects/ or CLAUDE.md
// to share them), so nothing is counted twice.
func workOwnPath(rel string) string {
	work := workConfigDir()
	if work == "" {
		return ""
	}
	p := filepath.Join(work, rel)
	fi, err := os.Stat(p)
	if err != nil {
		return ""
	}
	home, _ := os.UserHomeDir()
	if personal, err := os.Stat(filepath.Join(home, ".claude", rel)); err == nil && os.SameFile(fi, personal) {
		return ""
	}
	return p
}

// signedIn reports whether a Claude Code .claude.json holds a logged-in account.
func signedIn(claudeJSON string) bool {
	b, err := os.ReadFile(claudeJSON)
	if err != nil {
		return false
	}
	var cfg struct {
		OAuthAccount map[string]any `json:"oauthAccount"`
	}
	return json.Unmarshal(b, &cfg) == nil && len(cfg.OAuthAccount) > 0
}

// isWorkConfigDir reports whether a pane's CLAUDE_CONFIG_DIR is the work account.
func isWorkConfigDir(cfg string) bool {
	work := workConfigDir()
	return cfg != "" && work != "" && filepath.Clean(cfg) == work
}

// workClaudeCommand runs claude with the work account's config dir, plus args.
// "" when there is no work account.
func workClaudeCommand(args string) string {
	work := workConfigDir()
	if work == "" {
		return ""
	}
	return strings.TrimSpace("CLAUDE_CONFIG_DIR=" + shellQuote(work) + " claude " + args)
}

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// handleAccounts tells the UI which accounts it can offer.
func handleAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"work": workConfigDir() != ""})
}
