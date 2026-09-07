package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// keychainServiceFor returns the keychain service name Claude Code uses for a
// given CLAUDE_CONFIG_DIR: the default (~/.claude) is the bare name; any other
// config dir gets a "-<sha256(absPath)[:8]>" suffix (e.g. work / claude-ts).
func keychainServiceFor(configDir string) string {
	base := "Claude Code-credentials"
	home, _ := os.UserHomeDir()
	if configDir == "" || configDir == filepath.Join(home, ".claude") {
		return base
	}
	sum := sha256.Sum256([]byte(configDir))
	return base + "-" + hex.EncodeToString(sum[:])[:8]
}

// Real subscription usage from Claude's own endpoint (what `/usage` shows):
// GET https://api.anthropic.com/api/oauth/usage → per-window utilization.
// The OAuth token is read from the local keychain (personal) or a config-dir
// credentials file (work). Token is used only against Anthropic's own API and
// never logged.

type limitWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    any     `json:"resets_at"`
}

type usageResp struct {
	FiveHour limitWindow `json:"five_hour"`
	SevenDay limitWindow `json:"seven_day"`
}

func parseOAuthToken(b []byte) string {
	// shape: {"claudeAiOauth":{"accessToken":"..."}} or flat {"accessToken":"..."}
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) != nil {
		return ""
	}
	if raw, ok := top["claudeAiOauth"]; ok {
		var o map[string]any
		if json.Unmarshal(raw, &o) == nil {
			if t, ok := o["accessToken"].(string); ok {
				return t
			}
		}
	}
	var flat map[string]any
	if json.Unmarshal(b, &flat) == nil {
		if t, ok := flat["accessToken"].(string); ok {
			return t
		}
	}
	return ""
}

// readOAuthToken tries a config-dir credentials file first (work account uses
// CLAUDE_CONFIG_DIR), then the login keychain (personal default).
func readOAuthToken(configDir, keychainService string) string {
	if configDir != "" {
		if b, err := os.ReadFile(filepath.Join(configDir, ".credentials.json")); err == nil {
			if t := parseOAuthToken(b); t != "" {
				return t
			}
		}
	}
	if keychainService != "" {
		if out, err := exec.Command("security", "find-generic-password", "-w", "-s", keychainService).Output(); err == nil {
			if t := parseOAuthToken(out); t != "" {
				return t
			}
		}
	}
	return ""
}

func fetchUsageLimits(token string) *usageResp {
	if token == "" {
		return nil
	}
	req, err := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/usage?at_wall=1&skip_spend=1", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", "claude-wall")
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var u usageResp
	if json.NewDecoder(resp.Body).Decode(&u) != nil {
		return nil
	}
	return &u
}

// cache so we hit the API (and the keychain) at most once per account per TTL,
// avoiding repeated keychain-access prompts and rate-limit churn.
type limitsCache struct {
	mu   sync.Mutex
	at   map[string]time.Time
	data map[string]*usageResp
}

var limitsC = &limitsCache{at: map[string]time.Time{}, data: map[string]*usageResp{}}

func (c *limitsCache) get(account, configDir, keychain string) *usageResp {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.at[account]; ok && time.Since(t) < 60*time.Second {
		return c.data[account]
	}
	u := fetchUsageLimits(readOAuthToken(configDir, keychain))
	c.at[account] = time.Now()
	c.data[account] = u
	return u
}

func handleLimits(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	out := map[string]interface{}{}
	if u := limitsC.get("personal", "", "Claude Code-credentials"); u != nil {
		out["personal"] = u
	}
	home, _ := os.UserHomeDir()
	workDir := filepath.Join(home, ".claude-ts")
	if fi, err := os.Stat(workDir); err == nil && fi.IsDir() {
		// work account: credentials file, else the config-dir-namespaced keychain item
		if u := limitsC.get("work", workDir, keychainServiceFor(workDir)); u != nil {
			out["work"] = u
		}
	}
	json.NewEncoder(w).Encode(out)
}
