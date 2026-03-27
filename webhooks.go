package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// WebhookType represents the format used for webhook payloads
type WebhookType string

const (
	WebhookSlack   WebhookType = "slack"
	WebhookDiscord WebhookType = "discord"
	WebhookGeneric WebhookType = "generic"
)

// WebhookConfig holds the configuration for a single webhook endpoint
type WebhookConfig struct {
	ID     string      `json:"id"`
	URL    string      `json:"url"`
	Type   WebhookType `json:"type"`   // slack, discord, generic
	Events []string    `json:"events"` // permission, error, stopped, task_completed
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

// fireWebhook sends a single HTTP POST to the webhook URL
func fireWebhook(wh *WebhookConfig, eventType string, message string) {
	var payload []byte

	switch wh.Type {
	case WebhookSlack:
		payload, _ = json.Marshal(map[string]string{
			"text": message,
		})
	case WebhookDiscord:
		payload, _ = json.Marshal(map[string]string{
			"content": message,
		})
	default: // generic
		payload, _ = json.Marshal(map[string]string{
			"event":   eventType,
			"message": message,
			"source":  "claude-wall",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(wh.URL, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[webhook] send failed for %s (%s): %v", wh.ID, wh.URL, err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Printf("[webhook] %s returned status %d", wh.ID, resp.StatusCode)
	}
}

// mapEventToWebhook maps a hook event to a webhook event type and message.
// Returns ("", "") if this event should not trigger a webhook.
func mapEventToWebhook(event hookEvent) (string, string) {
	switch event.EventName {
	case "PermissionRequest":
		activity := formatActivity(event.ToolName, event.ToolInput)
		return "permission", fmt.Sprintf("[claude-wall] Permission required: %s", activity)

	case "Notification":
		var notif map[string]interface{}
		if json.Unmarshal(event.Notification, &notif) == nil {
			if t, _ := notif["type"].(string); t == "permission_prompt" || t == "elicitation_dialog" {
				return "permission", "[claude-wall] Agent waiting for approval"
			}
		}

	case "PostToolUseFailure":
		activity := formatActivity(event.ToolName, event.ToolInput)
		return "error", fmt.Sprintf("[claude-wall] Tool failed: %s", activity)

	case "StopFailure":
		return "error", "[claude-wall] Agent stopped with error (API failure)"

	case "Stop":
		return "stopped", "[claude-wall] Agent stopped/completed"

	case "TaskCompleted":
		return "task_completed", "[claude-wall] Scheduled task finished all runs"
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

	whType := WebhookType(req.Type)
	if whType != WebhookSlack && whType != WebhookDiscord && whType != WebhookGeneric {
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
		ID:     id,
		URL:    req.URL,
		Type:   whType,
		Events: events,
	}

	webhooks.add(wh)

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
