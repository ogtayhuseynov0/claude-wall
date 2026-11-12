package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
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
	Offset    int64     `json:"offset,omitempty"` // transcript bytes already counted
}

type financeStore struct {
	mu       sync.RWMutex
	procMu   sync.Mutex // one processTranscript at a time, so offsets stay right
	saveMu   sync.Mutex
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

	f.procMu.Lock()
	defer f.procMu.Unlock()

	file, err := os.Open(event.TranscriptPath)
	if err != nil {
		return
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return
	}

	// Continue from where the last Stop stopped. Transcripts only grow, and
	// they can pass 1 GB, so a full read on every Stop is too slow.
	var prev sessionCost
	f.mu.RLock()
	if sc := f.sessions[event.SessionID]; sc != nil {
		prev = *sc
	}
	f.mu.RUnlock()
	if prev.Offset == 0 || prev.Offset > st.Size() {
		prev = sessionCost{} // no offset yet, or the file was rewritten: count from 0
	}
	if _, err := file.Seek(prev.Offset, io.SeekStart); err != nil {
		return
	}

	// Sum usage across all assistant messages in the transcript
	inputTok, outputTok, cacheRead, cacheCreate := prev.InputTok, prev.OutputTok, prev.CacheRead, prev.CacheCreate
	turns := prev.Turns
	offset := prev.Offset
	assistant := []byte(`"type":"assistant"`)
	rd := bufio.NewReaderSize(file, 256*1024)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			break // EOF; a partial last line waits for the next Stop
		}
		offset += int64(len(line))
		if !bytes.Contains(line, assistant) {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message *struct {
				Usage *struct {
					Input       int64 `json:"input_tokens"`
					Output      int64 `json:"output_tokens"`
					CacheRead   int64 `json:"cache_read_input_tokens"`
					CacheCreate int64 `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &entry) != nil || entry.Type != "assistant" {
			continue
		}
		turns++
		if entry.Message == nil || entry.Message.Usage == nil {
			continue
		}
		u := entry.Message.Usage
		inputTok += u.Input
		outputTok += u.Output
		cacheRead += u.CacheRead
		cacheCreate += u.CacheCreate
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
		Offset:      offset,
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
	// saveMu keeps an older snapshot from landing after a newer one.
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	// Marshal under the lock: iterating the map while a Stop writes to it
	// crashed the server ("concurrent map iteration and map write").
	f.mu.RLock()
	out, err := json.MarshalIndent(financeData{
		Sessions:        f.sessions,
		MastermindCost:  f.mastermindCost,
		MastermindCalls: f.mastermindCalls,
	}, "", "  ")
	f.mu.RUnlock()
	if err != nil {
		return
	}
	writeFileAtomic(financeFile(), out, 0644)
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
