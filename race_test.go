package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// hub.resolveStatus writes h.termWorkingUntil without h.mu while
// unsubscribe deletes from it under h.mu.
func TestRaceHubTermWorkingUntil(t *testing.T) {
	hooks = nil
	h := newCaptureHub()
	working := "✻ Architecting… (3m 17s · ↓ 10.9k tokens)"
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.resolveStatus("A:1.1", working)
			h.resolveStatus("A:1.1", "idle")
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			ch := h.subscribe("A:1.1")
			h.unsubscribe("A:1.1", ch)
		}
	}()
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// financeStore.save marshals f.sessions after RUnlock.
func TestRaceFinanceSave(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := &financeStore{sessions: map[string]*sessionCost{}}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			f.addMastermindCost(0.01)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			f.mu.Lock()
			f.sessions[string(rune('a'+i%26))+string(rune(i))] = &sessionCost{CostUSD: 1}
			f.mu.Unlock()
		}
	}()
	wg.Wait()
}

// getStateForPane hands out the live *hookState; callers read fields unlocked.
func TestRaceHookStateEscape(t *testing.T) {
	hs := newHookStore()
	hs.processEvent(hookEvent{SessionID: "s", EventName: "SessionStart", PaneTarget: "A:1.1"})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			ev := "PreToolUse"
			if i%2 == 0 {
				ev = "Stop"
			}
			hs.processEvent(hookEvent{SessionID: "s", EventName: ev, ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`), PaneTarget: "A:1.1"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			if st := hs.getStateForPane("A:1.1", "/x"); st != nil {
				_ = st.Status + st.Activity
			}
		}
	}()
	wg.Wait()
}
