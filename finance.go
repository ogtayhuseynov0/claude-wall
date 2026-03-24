package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type sessionCost struct {
	SessionID string    `json:"sessionId"`
	Target    string    `json:"target"`
	Session   string    `json:"session"`
	DirName   string    `json:"dirName"`
	Branch    string    `json:"branch"`
	CostUSD   float64   `json:"costUsd"`
	InputTok  int64     `json:"inputTokens"`
	OutputTok int64     `json:"outputTokens"`
	CacheRead int64     `json:"cacheReadTokens"`
	CacheCreate int64   `json:"cacheCreateTokens"`
	Turns     int       `json:"turns"`
	Duration  int64     `json:"durationMs"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type financeStore struct {
	mu       sync.RWMutex
	sessions map[string]*sessionCost // sessionID → cost
	mastermindCost float64
	mastermindCalls int
}

var finance *financeStore

func initFinance() {
	finance = &financeStore{
		sessions: make(map[string]*sessionCost),
	}
	finance.load()
}

func financeFile() string {
	home, _ := os.UserHomeDir()
	return home + "/.claude/claude-wall-finance.json"
}

// processTranscript reads a Claude Code transcript JSONL to extract cost data
func (f *financeStore) processTranscript(event hookEvent) {
	if f == nil || event.TranscriptPath == "" {
		return
	}

	file, err := os.Open(event.TranscriptPath)
	if err != nil {
		return
	}
	defer file.Close()

	// Sum usage across all assistant messages in the transcript
	var inputTok, outputTok, cacheRead, cacheCreate int64
	var turns int
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var entry map[string]interface{}
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		if entry["type"] == "assistant" {
			turns++
			msg, _ := entry["message"].(map[string]interface{})
			if msg == nil {
				continue
			}
			usage, _ := msg["usage"].(map[string]interface{})
			if usage == nil {
				continue
			}
			if v, ok := usage["input_tokens"].(float64); ok { inputTok += int64(v) }
			if v, ok := usage["output_tokens"].(float64); ok { outputTok += int64(v) }
			if v, ok := usage["cache_read_input_tokens"].(float64); ok { cacheRead += int64(v) }
			if v, ok := usage["cache_creation_input_tokens"].(float64); ok { cacheCreate += int64(v) }
		}
	}

	if turns == 0 {
		return
	}

	// Estimate cost (Claude pricing approximation)
	// Input: $3/MTok, Output: $15/MTok, Cache read: $0.30/MTok, Cache create: $3.75/MTok
	costUSD := float64(inputTok)*3.0/1e6 + float64(outputTok)*15.0/1e6 +
		float64(cacheRead)*0.30/1e6 + float64(cacheCreate)*3.75/1e6

	// Resolve pane info
	target := event.PaneTarget
	session := ""
	dirName := ""
	branch := ""
	if target != "" {
		if idx := strings.Index(target, ":"); idx >= 0 {
			session = target[:idx]
		}
		if d := getPaneDir(target); d != "" {
			dirName = baseName(d)
			branch = gitBranch(d)
		}
	}

	f.mu.Lock()
	f.sessions[event.SessionID] = &sessionCost{
		SessionID:   event.SessionID,
		Target:      target,
		Session:     session,
		DirName:     dirName,
		Branch:      branch,
		CostUSD:     costUSD,
		InputTok:    inputTok,
		OutputTok:   outputTok,
		CacheRead:   cacheRead,
		CacheCreate: cacheCreate,
		Turns:       turns,
		UpdatedAt:   time.Now(),
	}
	f.mu.Unlock()
	f.save()
}

func (f *financeStore) addMastermindCost(cost float64) {
	f.mu.Lock()
	f.mastermindCost += cost
	f.mastermindCalls++
	f.mu.Unlock()
	f.save()
}

func (f *financeStore) getSummary() map[string]interface{} {
	f.mu.RLock()
	defer f.mu.RUnlock()

	var totalCost float64
	var totalInput, totalOutput, totalCacheRead, totalCacheCreate int64
	var totalTurns int
	sessions := make([]sessionCost, 0, len(f.sessions))

	for _, s := range f.sessions {
		totalCost += s.CostUSD
		totalInput += s.InputTok
		totalOutput += s.OutputTok
		totalCacheRead += s.CacheRead
		totalCacheCreate += s.CacheCreate
		totalTurns += s.Turns
		sessions = append(sessions, *s)
	}

	return map[string]interface{}{
		"totalCostUsd":      totalCost + f.mastermindCost,
		"agentCostUsd":      totalCost,
		"mastermindCostUsd": f.mastermindCost,
		"mastermindCalls":   f.mastermindCalls,
		"totalInputTokens":  totalInput,
		"totalOutputTokens": totalOutput,
		"totalCacheRead":    totalCacheRead,
		"totalCacheCreate":  totalCacheCreate,
		"totalTurns":        totalTurns,
		"sessions":          sessions,
	}
}

type financeData struct {
	Sessions       map[string]*sessionCost `json:"sessions"`
	MastermindCost float64                 `json:"mastermindCost"`
	MastermindCalls int                    `json:"mastermindCalls"`
}

func (f *financeStore) save() {
	f.mu.RLock()
	data := financeData{
		Sessions:        f.sessions,
		MastermindCost:  f.mastermindCost,
		MastermindCalls: f.mastermindCalls,
	}
	f.mu.RUnlock()

	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(financeFile(), out, 0644)
}

func (f *financeStore) load() {
	data, err := os.ReadFile(financeFile())
	if err != nil {
		return
	}
	var fd financeData
	if json.Unmarshal(data, &fd) != nil {
		return
	}
	f.mu.Lock()
	if fd.Sessions != nil {
		f.sessions = fd.Sessions
	}
	f.mastermindCost = fd.MastermindCost
	f.mastermindCalls = fd.MastermindCalls
	f.mu.Unlock()
}

func handleFinanceAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if finance == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{})
		return
	}
	json.NewEncoder(w).Encode(finance.getSummary())
}
