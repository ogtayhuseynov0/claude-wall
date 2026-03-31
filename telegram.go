package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ── Telegram Bot Types ──

type tgUpdate struct {
	UpdateID      int            `json:"update_id"`
	CallbackQuery *tgCallback    `json:"callback_query,omitempty"`
	Message       *tgMessage     `json:"message,omitempty"`
}

type tgCallback struct {
	ID      string     `json:"id"`
	Data    string     `json:"data"`
	Message *tgMessage `json:"message,omitempty"`
}

type tgMessage struct {
	MessageID int     `json:"message_id"`
	Chat      tgChat  `json:"chat"`
	Text      string  `json:"text"`
	ReplyTo   *tgMessage `json:"reply_to_message,omitempty"`
}

type tgChat struct {
	ID int64 `json:"id"`
}

// ── Telegram Bot Poller ──

type telegramBot struct {
	mu        sync.RWMutex
	token     string
	chatID    string
	offset    int
	running   bool

	// Track which Telegram message ID maps to which pane target
	msgToPaneMu sync.RWMutex
	msgToPane   map[int]string // telegram message_id → pane target

	// Session picker: track user waiting to type a message for a pane
	pickerMu   sync.Mutex
	pickerPane map[int64]string // chat_id → selected pane target (waiting for next message)
}

var tgBot = &telegramBot{
	msgToPane:  make(map[int]string),
	pickerPane: make(map[int64]string),
}

func (b *telegramBot) isRunning() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.running
}

// start begins long-polling if a telegram webhook with valid credentials exists.
// Called on server start and whenever webhooks change.
func (b *telegramBot) start() {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Find first enabled telegram webhook
	token, chatID := findTelegramCredentials()
	if token == "" || chatID == "" {
		if b.running {
			log.Println("[telegram] no credentials, stopping poller")
			b.running = false
		}
		return
	}

	if b.running && b.token == token && b.chatID == chatID {
		return // already running with same credentials
	}

	b.token = token
	b.chatID = chatID

	if !b.running {
		b.running = true
		go b.poll()
		log.Println("[telegram] bot poller started")
	}
}

func findTelegramCredentials() (string, string) {
	if webhooks == nil {
		return "", ""
	}
	webhooks.mu.RLock()
	defer webhooks.mu.RUnlock()
	for _, wh := range webhooks.webhooks {
		if wh.Type == WebhookTelegram && wh.Enabled {
			token := secrets.resolve(wh.URL)
			chatID := secrets.resolve(wh.ChatID)
			if token != "" && chatID != "" {
				return token, chatID
			}
		}
	}
	return "", ""
}

func (b *telegramBot) poll() {
	client := &http.Client{Timeout: 35 * time.Second}

	for {
		b.mu.RLock()
		if !b.running {
			b.mu.RUnlock()
			return
		}
		token := b.token
		b.mu.RUnlock()

		url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30", token, b.offset)
		resp, err := client.Get(url)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var result struct {
			OK     bool       `json:"ok"`
			Result []tgUpdate `json:"result"`
		}
		if json.Unmarshal(body, &result) != nil || !result.OK {
			time.Sleep(5 * time.Second)
			continue
		}

		for _, update := range result.Result {
			b.offset = update.UpdateID + 1
			b.handleUpdate(update)
		}
	}
}

func (b *telegramBot) handleUpdate(update tgUpdate) {
	if update.CallbackQuery != nil {
		b.handleCallback(update.CallbackQuery)
		return
	}
	if update.Message != nil {
		b.handleMessage(update.Message)
		return
	}
}

// ── Callback Queries (Button Presses) ──

func (b *telegramBot) handleCallback(cb *tgCallback) {
	// Parse callback data: "action|target" e.g. "yes|Agent:1.2"
	parts := strings.SplitN(cb.Data, "|", 2)
	if len(parts) < 2 {
		b.answerCallback(cb.ID, "Invalid callback")
		return
	}
	action, target := parts[0], parts[1]

	// Agent picker
	if action == "pick" {
		b.handlePickCallback(cb, target)
		return
	}

	var key string
	var feedback string

	switch {
	case action == "yes":
		key = "1"
		feedback = "Approved"
	case action == "always":
		key = "2"
		feedback = "Always allowed"
	case action == "no":
		key = "\x1b" // Escape
		feedback = "Denied"
	case strings.HasPrefix(action, "answer:"):
		// AskUserQuestion: the permission prompt IS the question UI.
		// Just send the option number — it selects the answer and approves implicitly.
		optNum := strings.TrimPrefix(action, "answer:")
		log.Printf("[telegram] answer: selecting option %s for %s", optNum, target)
		sendRawKeys(target, optNum)
		feedback = "Selected: option " + optNum
		b.answerCallback(cb.ID, feedback)
		if cb.Message != nil {
			b.editMessage(cb.Message.Chat.ID, cb.Message.MessageID, cb.Message.Text+"\n\n✅ <b>"+feedback+"</b>")
		}
		return
	default:
		key = action
		feedback = "Selected: " + action
	}

	log.Printf("[telegram] callback: action=%s target=%s key=%q", action, target, key)
	sendRawKeys(target, key)

	// Answer callback + edit message
	b.answerCallback(cb.ID, feedback)
	if cb.Message != nil {
		b.editMessage(cb.Message.Chat.ID, cb.Message.MessageID, cb.Message.Text+"\n\n✅ <b>"+feedback+"</b>")
	}
}

// ── Message Handling (Replies & Commands) ──

func (b *telegramBot) handleMessage(msg *tgMessage) {
	text := strings.TrimSpace(msg.Text)

	// Command: /agents — list active panes
	if text == "/agents" || text == "/start" {
		b.sendAgentList(msg.Chat.ID)
		return
	}

	// Command: /send Target message
	if strings.HasPrefix(text, "/send ") {
		rest := strings.TrimPrefix(text, "/send ")
		parts := strings.SplitN(rest, " ", 2)
		if len(parts) == 2 {
			target := resolveTarget(parts[0])
			if target != "" {
				sendTextToPane(target, parts[1])
				b.sendText(msg.Chat.ID, fmt.Sprintf("✅ Sent to <b>%s</b>", target))
			} else {
				b.sendText(msg.Chat.ID, "Unknown agent: "+parts[0])
			}
		}
		return
	}

	// Reply to a bot message → forward text to the pane that message belongs to
	if msg.ReplyTo != nil {
		b.msgToPaneMu.RLock()
		target, ok := b.msgToPane[msg.ReplyTo.MessageID]
		b.msgToPaneMu.RUnlock()
		if ok && target != "" {
			sendTextToPane(target, text)
			b.sendText(msg.Chat.ID, fmt.Sprintf("✅ Sent to <b>%s</b>", target))
			return
		}
	}

	// Check if user picked a session via /agents and is now typing a message
	b.pickerMu.Lock()
	target, hasPicker := b.pickerPane[msg.Chat.ID]
	if hasPicker {
		delete(b.pickerPane, msg.Chat.ID)
	}
	b.pickerMu.Unlock()

	if hasPicker && target != "" {
		sendTextToPane(target, text)
		b.sendText(msg.Chat.ID, fmt.Sprintf("✅ Sent to <b>%s</b>", target))
		return
	}

	// Unknown message — show help
	b.sendText(msg.Chat.ID, "Commands:\n/agents — list & select agents\n/send Agent message — send to agent\n\nOr reply to any notification to send text to that agent.")
}

// ── Agent List (Session Picker) ──

func (b *telegramBot) sendAgentList(chatID int64) {
	panes, err := findClaudePanes()
	if err != nil || len(panes) == 0 {
		b.sendText(chatID, "No active agents found.")
		return
	}

	var rows [][]map[string]string
	for _, p := range panes {
		label := fmt.Sprintf("%s · %s", p.Session, p.DirName)
		rows = append(rows, []map[string]string{
			{"text": label, "callback_data": "pick|" + p.Target},
		})
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id":      chatID,
		"text":         "🖥 <b>Select an agent</b>\n\nPick an agent, then type your message:",
		"parse_mode":   "HTML",
		"reply_markup": map[string]interface{}{"inline_keyboard": rows},
	})

	b.mu.RLock()
	token := b.token
	b.mu.RUnlock()

	http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token), "application/json", strings.NewReader(string(payload)))
}

// Handle pick callback from agent list
func (b *telegramBot) handlePickCallback(cb *tgCallback, target string) {
	b.pickerMu.Lock()
	b.pickerPane[cb.Message.Chat.ID] = target
	b.pickerMu.Unlock()

	// Resolve display name
	name := target
	if idx := strings.Index(target, ":"); idx >= 0 {
		name = target[:idx]
	}

	b.answerCallback(cb.ID, "Selected: "+name)
	b.editMessage(cb.Message.Chat.ID, cb.Message.MessageID,
		fmt.Sprintf("🖥 Selected: <b>%s</b>\n\nType your message now — it will be sent to this agent.", name))
}

// ── Hook Event → Interactive Telegram Message ──

func (b *telegramBot) handleHookEvent(event hookEvent, whEvent string, whMsg string) {
	b.mu.RLock()
	running := b.running
	b.mu.RUnlock()
	if !running {
		return
	}

	// Resolve pane target
	target := event.PaneTarget
	if target == "" {
		return
	}

	log.Printf("[telegram] hookEvent: event=%s target=%s whEvent=%s tool=%s", event.EventName, target, whEvent, event.ToolName)

	switch whEvent {
	case "permission":
		// Skip Notification events (permission_prompt, elicitation_dialog) —
		// the PermissionRequest event handles these with proper buttons/options.
		// Sending both would cause duplicate messages.
		if event.EventName == "Notification" {
			return
		}

		// Check if this is a question with options (AskUserQuestion or similar)
		q := parseAskUserQuestion(event.ToolInput)
		if q != nil {
			msg := fmt.Sprintf("❓ <b>Agent is asking:</b>\n%s\n\n<b>%s</b>", formatAgentBlock(resolveAgentContext(event)), q.Question)
			if q.MultiSelect {
				msg += "\n\n<i>Multi-select: reply with comma-separated choices</i>"
				b.sendNotification(msg, target)
			} else {
				// Show options as buttons — tapping one approves + selects
				var rows [][]map[string]string
				for i, opt := range q.Options {
					rows = append(rows, []map[string]string{
						{"text": opt, "callback_data": fmt.Sprintf("answer:%d|%s", i+1, target)},
					})
				}
				rows = append(rows, []map[string]string{
					{"text": "✗ Deny", "callback_data": "no|" + target},
				})
				msgID := b.sendWithKeyboard(msg, rows)
				if msgID > 0 {
					b.msgToPaneMu.Lock()
					b.msgToPane[msgID] = target
					b.msgToPaneMu.Unlock()
				}
			}
			return
		}

		// Standard permission — send with Yes/Always/No buttons
		b.sendPermissionMessage(whMsg, target)

	case "error", "stopped", "task_completed":
		// Non-interactive — send as reply-able notification
		b.sendNotification(whMsg, target)
	}
}

// parseAskUserQuestion extracts the question text and option labels from AskUserQuestion tool_input
type parsedQuestion struct {
	Question    string
	Options     []string
	MultiSelect bool
}

func parseAskUserQuestion(toolInput json.RawMessage) *parsedQuestion {
	var input struct {
		Questions []struct {
			Question    string `json:"question"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(toolInput, &input) != nil || len(input.Questions) == 0 {
		return nil
	}
	q := input.Questions[0]
	if len(q.Options) == 0 {
		return nil
	}
	var labels []string
	for _, o := range q.Options {
		labels = append(labels, o.Label)
	}
	return &parsedQuestion{Question: q.Question, Options: labels, MultiSelect: q.MultiSelect}
}

// ── Sending Messages with Inline Keyboards ──

// sendPermissionMessage sends a Telegram message with Yes/Always/No buttons.
// Returns the message ID for reply tracking.
func (b *telegramBot) sendPermissionMessage(text string, target string) {
	keyboard := [][]map[string]string{
		{
			{"text": "✓ Yes", "callback_data": "yes|" + target},
			{"text": "✓ Always", "callback_data": "always|" + target},
			{"text": "✗ No", "callback_data": "no|" + target},
		},
	}
	msgID := b.sendWithKeyboard(text, keyboard)
	if msgID > 0 {
		b.msgToPaneMu.Lock()
		b.msgToPane[msgID] = target
		b.msgToPaneMu.Unlock()
	}
}

// sendElicitationMessage sends a message with custom option buttons.
func (b *telegramBot) sendElicitationMessage(text string, target string, options []string) {
	var rows [][]map[string]string
	for _, opt := range options {
		rows = append(rows, []map[string]string{
			{"text": opt, "callback_data": opt + "|" + target},
		})
	}
	msgID := b.sendWithKeyboard(text, rows)
	if msgID > 0 {
		b.msgToPaneMu.Lock()
		b.msgToPane[msgID] = target
		b.msgToPaneMu.Unlock()
	}
}

// sendNotification sends a plain message and tracks it for reply-to.
func (b *telegramBot) sendNotification(text string, target string) {
	msgID := b.sendTextReturn(text)
	if msgID > 0 && target != "" {
		b.msgToPaneMu.Lock()
		b.msgToPane[msgID] = target
		b.msgToPaneMu.Unlock()
	}
}

// sendRawKeys sends keystrokes to a tmux pane without appending Enter.
// Claude Code's permission UI processes single keypresses immediately.
func sendRawKeys(target, key string) {
	hexes := make([]string, 0, len(key))
	for _, b := range []byte(key) {
		hexes = append(hexes, fmt.Sprintf("%02x", b))
	}
	args := append([]string{"send-keys", "-t", target, "-H"}, hexes...)
	exec.Command("tmux", args...).Run()
}

// ── Telegram API Helpers ──

func (b *telegramBot) sendText(chatID int64, text string) {
	b.mu.RLock()
	token := b.token
	chatIDStr := b.chatID
	b.mu.RUnlock()

	cid := fmt.Sprintf("%d", chatID)
	if cid == "0" {
		cid = chatIDStr
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id":    cid,
		"text":       text,
		"parse_mode": "HTML",
	})
	http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token), "application/json", strings.NewReader(string(payload)))
}

func (b *telegramBot) sendTextReturn(text string) int {
	b.mu.RLock()
	token := b.token
	chatID := b.chatID
	b.mu.RUnlock()

	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	})
	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token), "application/json", strings.NewReader(string(payload)))
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	body, _ := io.ReadAll(resp.Body)
	json.Unmarshal(body, &result)
	return result.Result.MessageID
}

func (b *telegramBot) sendWithKeyboard(text string, keyboard [][]map[string]string) int {
	b.mu.RLock()
	token := b.token
	chatID := b.chatID
	b.mu.RUnlock()

	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id":      chatID,
		"text":         text,
		"parse_mode":   "HTML",
		"reply_markup": map[string]interface{}{"inline_keyboard": keyboard},
	})
	resp, err := http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token), "application/json", strings.NewReader(string(payload)))
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	body, _ := io.ReadAll(resp.Body)
	json.Unmarshal(body, &result)
	return result.Result.MessageID
}

func (b *telegramBot) answerCallback(callbackID string, text string) {
	b.mu.RLock()
	token := b.token
	b.mu.RUnlock()

	payload, _ := json.Marshal(map[string]string{
		"callback_query_id": callbackID,
		"text":              text,
	})
	http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", token), "application/json", strings.NewReader(string(payload)))
}

func (b *telegramBot) editMessage(chatID int64, messageID int, text string) {
	b.mu.RLock()
	token := b.token
	b.mu.RUnlock()

	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
		"parse_mode": "HTML",
	})
	http.Post(fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", token), "application/json", strings.NewReader(string(payload)))
}
