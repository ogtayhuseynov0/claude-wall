package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// WebhookType represents the format used for webhook payloads
type WebhookType string

const (
	WebhookSlack    WebhookType = "slack"
	WebhookDiscord  WebhookType = "discord"
	WebhookGeneric  WebhookType = "generic"
	WebhookTelegram WebhookType = "telegram"
	// WebhookBridge is a structured, NON-debounced machine endpoint (e.g. the
	// project-dashboard agent bridge). Unlike the others it fires per-session
	// every time so the receiver can match Stop events to in-flight requests.
	WebhookBridge WebhookType = "bridge"
)

// WebhookConfig holds the configuration for a single webhook endpoint
type WebhookConfig struct {
	ID      string      `json:"id"`
	URL     string      `json:"url"`              // For telegram: bot token (without "bot" prefix)
	ChatID  string      `json:"chatId,omitempty"`  // Telegram chat ID
	Type    WebhookType `json:"type"`              // slack, discord, generic, telegram
	Events  []string    `json:"events"`            // permission, error, stopped, task_completed
	Enabled bool        `json:"enabled"`
}

// webhookStore manages webhook configurations and sending
type webhookStore struct {
	mu       sync.RWMutex
	webhooks map[string]*WebhookConfig

	// Debounce: track last send time per event type
	debounceMu sync.Mutex
	lastSent   map[string]time.Time // key: eventType → last send time
}

var webhooks *webhookStore

func webhooksFile() string {
	home, _ := os.UserHomeDir()
	return home + "/.claude/claude-wall-webhooks.json"
}

func newWebhookStore() *webhookStore {
	ws := &webhookStore{
		webhooks: make(map[string]*WebhookConfig),
		lastSent: make(map[string]time.Time),
	}
	ws.load()
	return ws
}

// load reads webhook configs from disk
func (ws *webhookStore) load() {
	data, err := os.ReadFile(webhooksFile())
	if err != nil {
		return
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	var configs map[string]*WebhookConfig
	if json.Unmarshal(data, &configs) == nil && configs != nil {
		ws.webhooks = configs
	}
}

// save writes webhook configs to disk. Caller must NOT hold ws.mu.
func (ws *webhookStore) save() {
	ws.mu.RLock()
	data, _ := json.MarshalIndent(ws.webhooks, "", "  ")
	ws.mu.RUnlock()
	os.WriteFile(webhooksFile(), data, 0644)
}

// list returns all configured webhooks
func (ws *webhookStore) list() []*WebhookConfig {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	result := make([]*WebhookConfig, 0, len(ws.webhooks))
	for _, wh := range ws.webhooks {
		result = append(result, wh)
	}
	return result
}

// add creates a new webhook config and saves to disk
func (ws *webhookStore) add(wh *WebhookConfig) {
	ws.mu.Lock()
	ws.webhooks[wh.ID] = wh
	ws.mu.Unlock()
	ws.save()
}

// remove deletes a webhook config and saves to disk
func (ws *webhookStore) remove(id string) bool {
	ws.mu.Lock()
	_, ok := ws.webhooks[id]
	if ok {
		delete(ws.webhooks, id)
	}
	ws.mu.Unlock()
	if ok {
		ws.save()
	}
	return ok
}

// sendWebhook dispatches a webhook notification for a given event type.
// Debounces: max 1 webhook per event type per 30 seconds.
func (ws *webhookStore) sendWebhook(eventType string, message string) {
	// Debounce check
	ws.debounceMu.Lock()
	if last, ok := ws.lastSent[eventType]; ok && time.Since(last) < 30*time.Second {
		ws.debounceMu.Unlock()
		return
	}
	ws.lastSent[eventType] = time.Now()
	ws.debounceMu.Unlock()

	// Collect matching webhooks (read lock only)
	ws.mu.RLock()
	var matching []*WebhookConfig
	for _, wh := range ws.webhooks {
		if !wh.Enabled {
			continue
		}
		for _, ev := range wh.Events {
			if ev == eventType {
				matching = append(matching, wh)
				break
			}
		}
	}
	ws.mu.RUnlock()

	if len(matching) == 0 {
		return
	}

	// Fire webhooks in background
	for _, wh := range matching {
		go fireWebhook(wh, eventType, message)
	}
}

// sendBridge pushes a structured, NON-debounced event to every enabled
// bridge-type webhook whose Events list includes eventType. Bridge receivers
// (the dashboard agent bridge) need the raw per-session signal every time —
// no debounce, no telegram-skip, no human-formatted message.
func (ws *webhookStore) sendBridge(event hookEvent, eventType string) {
	ws.mu.RLock()
	var matching []*WebhookConfig
	for _, wh := range ws.webhooks {
		if !wh.Enabled || wh.Type != WebhookBridge {
			continue
		}
		for _, ev := range wh.Events {
			if ev == eventType {
				matching = append(matching, wh)
				break
			}
		}
	}
	ws.mu.RUnlock()
	if len(matching) == 0 {
		return
	}

	ctx := resolveAgentContext(event)
	// On Stop, include the agent's actual reply from the transcript so receivers
	// don't have to scrape the TUI.
	reply := ""
	if event.EventName == "Stop" {
		reply = lastAssistantText(event.TranscriptPath)
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"event":    eventType,
		"rawEvent": event.EventName,
		"session":  ctx.Session,
		"pane":     ctx.Pane,
		"folder":   ctx.Folder,
		"branch":   ctx.Branch,
		"prompt":   ctx.LastPrompt,
		"reply":    reply,
		"source":   "claude-wall",
		"time":     time.Now().UTC().Format(time.RFC3339),
	})
	for _, wh := range matching {
		go func(rawURL string) {
			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Post(secrets.resolve(rawURL), "application/json", bytes.NewReader(payload))
			if err != nil {
				log.Printf("[webhook] bridge send failed: %v", err)
				return
			}
			resp.Body.Close()
		}(wh.URL)
	}
}

// fireWebhook sends a single HTTP POST to the webhook URL
func fireWebhook(wh *WebhookConfig, eventType string, message string) {
	// Skip Telegram if bot poller is running (it sends interactive messages instead)
	if wh.Type == WebhookTelegram && tgBot.isRunning() {
		return
	}

	var payload []byte
	var url string

	// Resolve secret references ($SECRET_NAME → actual value)
	resolvedURL := secrets.resolve(wh.URL)
	resolvedChatID := secrets.resolve(wh.ChatID)

	switch wh.Type {
	case WebhookSlack:
		url = resolvedURL
		payload, _ = json.Marshal(map[string]string{
			"text": message,
		})
	case WebhookDiscord:
		url = resolvedURL
		payload, _ = json.Marshal(map[string]string{
			"content": message,
		})
	case WebhookTelegram:
		url = fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", resolvedURL)
		payload, _ = json.Marshal(map[string]interface{}{
			"chat_id":    resolvedChatID,
			"text":       message,
			"parse_mode": "HTML",
		})
	default: // generic
		url = resolvedURL
		payload, _ = json.Marshal(map[string]string{
			"event":   eventType,
			"message": message,
			"source":  "claude-wall",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[webhook] send failed for %s: %v", wh.ID, err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Printf("[webhook] %s returned status %d", wh.ID, resp.StatusCode)
	}
}

// agentContext gathers rich context about an agent for webhook messages.
type agentContext struct {
	Pane       string
	Session    string
	Folder     string
	Branch     string
	Activity   string
	LastPrompt string
}

func resolveAgentContext(event hookEvent) agentContext {
	ctx := agentContext{}
	if hooks == nil {
		return ctx
	}
	hooks.mu.RLock()
	state := hooks.sessions[event.SessionID]
	hooks.mu.RUnlock()

	if state != nil {
		ctx.Pane = state.PaneTarget
		ctx.Activity = state.Activity
		if state.PaneTarget != "" {
			if idx := strings.Index(state.PaneTarget, ":"); idx >= 0 {
				ctx.Session = state.PaneTarget[:idx]
			}
		}
		if state.CWD != "" {
			parts := strings.Split(state.CWD, "/")
			if len(parts) > 0 {
				ctx.Folder = parts[len(parts)-1]
			}
		}
	}

	if event.CWD != "" {
		ctx.Branch = gitBranch(event.CWD)
	}

	// Extract last user prompt from transcript JSONL
	if event.TranscriptPath != "" {
		ctx.LastPrompt = lastUserPrompt(event.TranscriptPath)
	}
	return ctx
}

// lastUserPrompt reads the transcript JSONL and returns the last external user message.
func lastUserPrompt(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var last string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		// Quick filter before parsing JSON
		if !strings.Contains(line, `"type":"user"`) {
			continue
		}
		if !strings.Contains(line, `"userType":"external"`) {
			continue
		}
		if strings.Contains(line, `"tool_result"`) {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Type != "user" {
			continue
		}
		// Content can be a string or an array
		var text string
		if json.Unmarshal(entry.Message.Content, &text) == nil && text != "" {
			last = text
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(entry.Message.Content, &parts) == nil {
			for _, p := range parts {
				if p.Type == "text" && p.Text != "" {
					last = p.Text
				}
			}
		}
	}
	if len(last) > 200 {
		last = last[:200] + "..."
	}
	return last
}

// lastAssistantText reads the transcript JSONL and returns the text of the last
// assistant turn — i.e. the agent's reply. Used by bridge webhooks so receivers
// get the actual answer without scraping the TUI. Returns "" if unavailable.
func lastAssistantText(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var last string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || !strings.Contains(line, `"type":"assistant"`) {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Type != "assistant" {
			continue
		}
		// Assistant content is an array of blocks; collect the text blocks.
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(entry.Message.Content, &parts) == nil {
			var sb strings.Builder
			for _, p := range parts {
				if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(p.Text)
				}
			}
			if sb.Len() > 0 {
				last = sb.String()
			}
		}
	}
	if len(last) > 8000 {
		last = last[len(last)-8000:]
	}
	return last
}

// formatAgentBlock builds a multi-line context block with icons for webhook messages.
func formatAgentBlock(ctx agentContext) string {
	var lines []string
	if ctx.Session != "" {
		lines = append(lines, fmt.Sprintf("🖥 <b>Session:</b> %s", ctx.Session))
	}
	if ctx.Folder != "" {
		lines = append(lines, fmt.Sprintf("📂 <b>Folder:</b> %s", ctx.Folder))
	}
	if ctx.Branch != "" {
		lines = append(lines, fmt.Sprintf("🌿 <b>Branch:</b> %s", ctx.Branch))
	}
	if ctx.LastPrompt != "" {
		lines = append(lines, fmt.Sprintf("💬 <b>Prompt:</b> %s", ctx.LastPrompt))
	} else if ctx.Activity != "" {
		lines = append(lines, fmt.Sprintf("💬 <b>Last:</b> %s", ctx.Activity))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// mapEventToWebhook maps a hook event to a webhook event type and message.
// Returns ("", "") if this event should not trigger a webhook.
// Messages use HTML tags for Telegram; Slack/Discord/generic render them naturally.
func mapEventToWebhook(event hookEvent) (string, string) {
	ctx := resolveAgentContext(event)
	block := formatAgentBlock(ctx)

	switch event.EventName {
	case "PermissionRequest":
		activity := formatActivity(event.ToolName, event.ToolInput)
		msg := fmt.Sprintf("🟡 <b>Permission Required</b>\n%s⚡ <b>Action:</b> %s\n\n<i>Open dashboard to approve</i>", block, activity)
		return "permission", msg

	case "Notification":
		var notif map[string]interface{}
		if json.Unmarshal(event.Notification, &notif) == nil {
			if t, _ := notif["type"].(string); t == "permission_prompt" || t == "elicitation_dialog" {
				msg := fmt.Sprintf("🟡 <b>Waiting for Approval</b>\n%s\n<i>Agent is paused until you respond</i>", block)
				return "permission", msg
			}
		}

	case "PostToolUseFailure":
		activity := formatActivity(event.ToolName, event.ToolInput)
		msg := fmt.Sprintf("🔴 <b>Tool Failed</b>\n%s⚡ <b>Tool:</b> %s\n\n<i>Agent will retry or try a different approach</i>", block, activity)
		return "error", msg

	case "StopFailure":
		msg := fmt.Sprintf("🔴 <b>API Error</b>\n%s\n<i>Agent stopped due to an API failure</i>", block)
		return "error", msg

	case "Stop":
		msg := fmt.Sprintf("✅ <b>Agent Finished</b>\n%s\n<i>Task completed or conversation ended</i>", block)
		return "stopped", msg

	case "TaskCompleted":
		msg := fmt.Sprintf("📋 <b>Scheduled Task Done</b>\n%s\n<i>All scheduled runs completed</i>", block)
		return "task_completed", msg
	}

	return "", ""
}

// ── HTTP handlers ──

func handleWebhookList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(webhooks.list())
}

func handleWebhookCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}

	var req struct {
		URL    string   `json:"url"`
		ChatID string   `json:"chatId"`
		Type   string   `json:"type"`
		Events []string `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	if req.URL == "" {
		http.Error(w, "url is required", 400)
		return
	}
	if WebhookType(req.Type) == WebhookTelegram && req.ChatID == "" {
		http.Error(w, "chatId is required for telegram", 400)
		return
	}

	whType := WebhookType(req.Type)
	if whType != WebhookSlack && whType != WebhookDiscord && whType != WebhookGeneric && whType != WebhookTelegram && whType != WebhookBridge {
		whType = WebhookGeneric
	}

	// Validate events
	validEvents := map[string]bool{"permission": true, "error": true, "stopped": true, "task_completed": true}
	var events []string
	for _, ev := range req.Events {
		if validEvents[ev] {
			events = append(events, ev)
		}
	}
	if len(events) == 0 {
		events = []string{"permission", "error", "stopped", "task_completed"}
	}

	id := fmt.Sprintf("wh-%d", time.Now().UnixNano()%100000)
	wh := &WebhookConfig{
		ID:      id,
		URL:     req.URL,
		ChatID:  req.ChatID,
		Type:    whType,
		Events:  events,
		Enabled: true,
	}

	webhooks.add(wh)
	tgBot.start() // pick up new telegram credentials if applicable

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(wh)
}

func handleWebhookTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/webhooks/test/")
	if id == "" {
		http.Error(w, "missing webhook id", 400)
		return
	}

	webhooks.mu.RLock()
	wh, ok := webhooks.webhooks[id]
	webhooks.mu.RUnlock()

	if !ok {
		http.Error(w, "webhook not found", 404)
		return
	}

	msg := "🧪 <b>Test Notification</b>\n🖥 <b>Source:</b> Claude Wall\n\n<i>If you see this, your webhook is working!</i>"
	go fireWebhook(wh, "test", msg)
	w.Write([]byte(`{"status":"sent"}`))
}

func handleWebhookToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/webhooks/toggle/")
	if id == "" {
		http.Error(w, "missing webhook id", 400)
		return
	}

	webhooks.mu.Lock()
	wh, ok := webhooks.webhooks[id]
	if ok {
		wh.Enabled = !wh.Enabled
	}
	webhooks.mu.Unlock()

	if !ok {
		http.Error(w, "webhook not found", 404)
		return
	}
	webhooks.save()
	tgBot.start() // re-check credentials on toggle
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(wh)
}

func handleWebhookDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" && r.Method != "POST" {
		http.Error(w, "DELETE or POST only", 405)
		return
	}

	id := r.URL.Path[len("/api/webhooks/"):]
	if id == "" {
		http.Error(w, "missing webhook id", 400)
		return
	}

	if webhooks.remove(id) {
		w.Write([]byte(`{"status":"ok"}`))
	} else {
		http.Error(w, "webhook not found", 404)
	}
}
