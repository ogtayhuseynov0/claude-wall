package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDetectAgent(t *testing.T) {
	cases := []struct {
		cmd, title, want string
	}{
		{"2.1.330", "✳ Fix the login form", agentClaude},
		{"claude", "", agentClaude},
		{"node", "π - my-app", agentPi},
		{"pi", "", agentPi},
		{"node", "π", agentPi},
		{"opencode", "", agentOpenCode},
		{"opencode-v2", "OpenCode", agentOpenCode},
		{"bun", "opencode - my-app", agentOpenCode},
		{"opencode", "OC | Greeting", agentOpenCode},
		{"node", "OC | Fix the build", agentOpenCode},
		{"codex", "", agentCodex},
		{"node", "Codex", agentCodex},
		// A Claude session named after another agent stays Claude.
		{"2.1.330", "✳ Add opencode and pi support", agentClaude},
		// A shell keeps the last program's title.
		{"zsh", "π - my-app", ""},
		{"zsh", "Claude Code", ""},
		// Other programs that only mention an agent.
		{"ruby", "brew install some-tap/opencode-v2", ""},
		{"nvim", "nvim opencode.json", ""},
		{"node", "pnpm dev", ""},
		{"node", "pi-calculator", ""},
	}
	for _, c := range cases {
		if got := detectAgent(c.cmd, c.title); got != c.want {
			t.Errorf("detectAgent(%q, %q) = %q, want %q", c.cmd, c.title, got, c.want)
		}
	}
}

func TestPaneLabel(t *testing.T) {
	cases := map[[2]string]string{
		{"node", "π - app"}:      "pi",
		{"opencode", ""}:         "opencode",
		{"nvim", "nvim main.go"}: "nvim",
		{"zsh", "π - app"}:       "shell",
		{"node", "pnpm dev"}:     "node",
	}
	for in, want := range cases {
		if got := paneLabel(in[0], in[1]); got != want {
			t.Errorf("paneLabel(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// A plugin agent's Stop right after a tool call must end the working state:
// its events come in order, so the Claude Code guard against a late
// PostToolUse does not apply.
func TestOrderedAgentStopIsFinal(t *testing.T) {
	for _, agent := range []string{agentPi, agentOpenCode} {
		s := newHookStore()
		ev := func(name string) hookEvent {
			return hookEvent{SessionID: agent + "-1", CWD: "/tmp/x", EventName: name, Agent: agent, PaneTarget: "S:1.1"}
		}
		s.processEvent(ev("UserPromptSubmit"))
		if got := s.getStateForPane("S:1.1", "").Activity; got != "Thinking…" {
			t.Errorf("%s: activity after prompt = %q", agent, got)
		}
		pre := ev("PreToolUse")
		pre.ToolName = "Bash"
		pre.ToolInput = json.RawMessage(`{"command":"echo hi"}`)
		s.processEvent(pre)
		if got := s.getStateForPane("S:1.1", "").Activity; got != "$ echo hi" {
			t.Errorf("%s: activity after tool = %q", agent, got)
		}
		s.processEvent(ev("PostToolUse"))
		s.processEvent(ev("Stop"))
		if got := s.getStateForPane("S:1.1", "").Status; got != "idle" {
			t.Errorf("%s: status after Stop = %q, want idle", agent, got)
		}
	}

	// Claude Code keeps the guard: a Stop right after a tool stays working.
	s := newHookStore()
	s.processEvent(hookEvent{SessionID: "c1", EventName: "PreToolUse", ToolName: "Bash", PaneTarget: "S:1.2"})
	s.processEvent(hookEvent{SessionID: "c1", EventName: "Stop", PaneTarget: "S:1.2"})
	if got := s.getStateForPane("S:1.2", "").Status; got != "working" {
		t.Errorf("claude: status right after tool + Stop = %q, want working", got)
	}
}

func TestHeartbeatKeepsState(t *testing.T) {
	s := newHookStore()
	s.processEvent(hookEvent{SessionID: "pi-1", EventName: "PreToolUse", ToolName: "Read", Agent: agentPi, PaneTarget: "S:1.1"})
	s.sessions["pi-1"].UpdatedAt = time.Now().Add(-time.Minute)
	s.processEvent(hookEvent{SessionID: "pi-1", EventName: "Heartbeat", Agent: agentPi, PaneTarget: "S:1.1"})
	st := s.getStateForPane("S:1.1", "")
	if st.Status != "working" || st.Activity != "Read" || time.Since(st.UpdatedAt) > time.Second {
		t.Errorf("after heartbeat: %+v", st)
	}
}

func TestPiLoaderStatus(t *testing.T) {
	rule := "────────────────────────────────────────"
	busy := "out\n── ⠼ Working " + rule + "\n" + rule + "\n~/app (main)\n0.1%/1.0M (auto)   model • medium\n"
	if st, act := parseTerminalStatus(busy); st != "working" || act != "Working…" {
		t.Errorf("busy pi screen: %q %q", st, act)
	}
	idle := "done\n" + rule + "\n" + rule + "\n~/app (main)\n0.1%/1.0M (auto)   model • medium\n"
	if st, _ := parseTerminalStatus(idle); st != "idle" {
		t.Errorf("idle pi screen: %q", st)
	}
	// A braille spinner that a command printed is not Pi's loader.
	printed := "  ⎿  ⠋ Installing dependencies\n" + rule + "\n"
	if st, _ := parseTerminalStatus(printed); st != "idle" {
		t.Errorf("printed spinner: %q", st)
	}
}

// A stale "working" hook state must not be pushed: the capture loop reads the
// terminal for that pane, and pushing the stale state on every other agent's
// hook event made the status flip between working and idle.
func TestPushHookStatusSkipsStaleState(t *testing.T) {
	h := newCaptureHub()
	s := newHookStore()
	saved := hooks
	hooks = s // resolveStatus reads the global store
	defer func() { hooks = saved }()
	setPaneDir("T:1.1", "/tmp/x")
	ch := h.subscribe("T:1.1")
	h.latest["T:1.1"] = paneUpdate{Status: "idle"}

	s.processEvent(hookEvent{SessionID: "pi-9", EventName: "PreCompact", Agent: agentPi, PaneTarget: "T:1.1"})
	s.sessions["pi-9"].UpdatedAt = time.Now().Add(-3 * time.Minute)
	h.pushHookStatus(s)
	select {
	case u := <-ch:
		t.Fatalf("stale state pushed: %q", u.Status)
	default:
	}
	if st, _, _ := h.resolveStatus("T:1.1", "idle screen\n"); st != "idle" {
		t.Fatalf("capture path with stale state = %q, want idle", st)
	}

	s.processEvent(hookEvent{SessionID: "pi-9", EventName: "UserPromptSubmit", Agent: agentPi, PaneTarget: "T:1.1"})
	h.pushHookStatus(s)
	select {
	case u := <-ch:
		if u.Status != "working" {
			t.Fatalf("fresh state pushed %q", u.Status)
		}
	default:
		t.Fatal("fresh working state not pushed")
	}
}
