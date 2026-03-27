package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// tmuxControl manages a persistent tmux -C connection.
// All pane output is received via %output notifications (push, not poll).
type tmuxControl struct {
	mu    sync.Mutex
	cmd   *exec.Cmd
	stdin *bufio.Writer

	// Pending command responses: FIFO queue (tmux returns responses in order)
	pending   []chan string
	pendingMu sync.Mutex

	// Pane output callbacks
	onOutput func(paneID string, data string)

	// Pane ID → target mapping (e.g. %5 → Agent:1.2)
	paneMap   map[string]string // paneID → target
	targetMap map[string]string // target → paneID
	mapMu     sync.RWMutex
}

func newTmuxControl() *tmuxControl {
	return &tmuxControl{
		paneMap:   make(map[string]string),
		targetMap: make(map[string]string),
	}
}

func (tc *tmuxControl) start() error {
	// Build env without TMUX (allows control mode from within tmux)
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "TMUX=") {
			env = append(env, e)
		}
	}

	tc.cmd = exec.Command("tmux", "-C", "attach")
	tc.cmd.Env = env

	stdinPipe, err := tc.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	tc.stdin = bufio.NewWriter(stdinPipe)

	stdoutPipe, err := tc.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := tc.cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	// Parse output in background
	go tc.readLoop(bufio.NewScanner(stdoutPipe))

	// Give readLoop time to start and drain initial tmux notifications
	time.Sleep(1 * time.Second)

	// Drain: send a known marker and wait for it
	drainCh := make(chan string, 1)
	tc.pendingMu.Lock()
	tc.pending = append(tc.pending, drainCh)
	tc.pendingMu.Unlock()
	tc.sendRaw("display-message -p DRAIN_MARKER")

	// Wait and discard anything before our marker
	for {
		select {
		case resp := <-drainCh:
			if strings.TrimSpace(resp) == "DRAIN_MARKER" {
				goto drained
			}
			log.Printf("[tmuxctl] draining: %q", resp)
			// Re-queue a receiver for the next block
			tc.pendingMu.Lock()
			tc.pending = append(tc.pending, drainCh)
			tc.pendingMu.Unlock()
		case <-time.After(3 * time.Second):
			goto drained
		}
	}
drained:

	// Test with a simple command
	resp, err := tc.sendCommand("display-message -p cw-ok")
	if err != nil {
		tc.cmd.Process.Kill()
		return fmt.Errorf("control mode test failed: %w", err)
	}
	_ = resp

	// Build initial pane map
	tc.refreshPaneMap()

	// Test capture-pane on the first available pane
	tc.mapMu.RLock()
	var testTarget string
	for _, t := range tc.paneMap {
		testTarget = t
		break
	}
	tc.mapMu.RUnlock()
	if testTarget != "" {
		out, err := tc.capturePaneByTarget(testTarget)
		if err != nil {
			log.Printf("[tmuxctl] capture test FAILED for %s: %v", testTarget, err)
		} else {
			lines := strings.Split(out, "\n")
			_ = lines
		}
	}

	return nil
}

func (tc *tmuxControl) stop() {
	if tc.cmd != nil && tc.cmd.Process != nil {
		tc.sendRaw("detach")
		time.Sleep(200 * time.Millisecond)
		tc.cmd.Process.Kill()
	}
}

// sendCommand sends a tmux command and returns the response.
func (tc *tmuxControl) sendCommand(command string) (string, error) {
	ch := make(chan string, 1)

	tc.pendingMu.Lock()
	tc.pending = append(tc.pending, ch)
	tc.pendingMu.Unlock()

	tc.sendRaw(command)

	select {
	case result := <-ch:
		return result, nil
	case <-time.After(5 * time.Second):
		return "", fmt.Errorf("timeout waiting for response to: %s", command)
	}
}

func (tc *tmuxControl) sendRaw(command string) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.stdin.WriteString(command + "\n")
	tc.stdin.Flush()
}

// readLoop parses the tmux control mode protocol.
func (tc *tmuxControl) readLoop(scanner *bufio.Scanner) {
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024) // 4MB buffer

	var (
		inBlock  bool
		blockBuf strings.Builder
	)

	for scanner.Scan() {
		line := scanner.Text()

		// %begin [timestamp] [cmdnum] [flags]
		if strings.HasPrefix(line, "%begin ") {
			inBlock = true
			blockBuf.Reset()
			continue
		}

		// %end / %error — deliver response to next waiting caller (FIFO)
		if strings.HasPrefix(line, "%end ") || strings.HasPrefix(line, "%error ") {
			inBlock = false
			tc.pendingMu.Lock()
			if len(tc.pending) > 0 {
				ch := tc.pending[0]
				tc.pending = tc.pending[1:]
				ch <- blockBuf.String()
			} else {
				// No one waiting — unsolicited response (tmux sends initial block on attach)
				content := blockBuf.String()
				if len(content) > 60 {
					content = content[:60] + "..."
				}
				_ = content
			}
			tc.pendingMu.Unlock()
			continue
		}

		// %output PANE_ID DATA — can arrive even inside %begin/%end blocks
		if strings.HasPrefix(line, "%output ") {
			rest := line[8:] // after "%output "
			spaceIdx := strings.IndexByte(rest, ' ')
			if spaceIdx > 0 {
				paneID := rest[:spaceIdx]
				data := unescapeControlMode(rest[spaceIdx+1:])
				if tc.onOutput != nil {
					tc.onOutput(paneID, data)
				}
			}
			continue
		}

		// Inside a command response block — collect content (BEFORE checking other % notifications)
		if inBlock {
			if blockBuf.Len() > 0 {
				blockBuf.WriteByte('\n')
			}
			blockBuf.WriteString(unescapeControlMode(line))
			continue
		}
	}
}

// unescapeControlMode reverses tmux's octal escaping of control characters.
// Characters < ASCII 32 and backslash are escaped as \NNN (3-digit octal).
func unescapeControlMode(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s // fast path
	}

	var buf strings.Builder
	buf.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == '\\' && i+3 < len(s) {
			// Try to parse 3 octal digits
			o1, o2, o3 := s[i+1]-'0', s[i+2]-'0', s[i+3]-'0'
			if o1 < 8 && o2 < 8 && o3 < 8 {
				buf.WriteByte(o1*64 + o2*8 + o3)
				i += 4
				continue
			}
			// Not octal — literal backslash
			if i+1 < len(s) && s[i+1] == '\\' {
				buf.WriteByte('\\')
				i += 2
				continue
			}
		}
		buf.WriteByte(s[i])
		i++
	}
	return buf.String()
}

// refreshPaneMap queries tmux for all panes and builds the paneID ↔ target mapping.
func (tc *tmuxControl) refreshPaneMap() {
	resp, err := tc.sendCommand(`list-panes -a -F "#{pane_id},#{session_name}:#{window_index}.#{pane_index}"`)
	if err != nil {
		log.Printf("[tmuxctl] refreshPaneMap failed: %v", err)
		return
	}

	tc.mapMu.Lock()
	defer tc.mapMu.Unlock()

	tc.paneMap = make(map[string]string)
	tc.targetMap = make(map[string]string)

	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		parts := strings.SplitN(line, ",", 2)
		if len(parts) == 2 {
			paneID := parts[0]
			target := parts[1]
			tc.paneMap[paneID] = target
			tc.targetMap[target] = paneID
		}
	}
	log.Printf("[tmuxctl] pane map: %d panes", len(tc.paneMap))
}

// capturePaneByTarget sends capture-pane for a target via control mode (no subprocess).
func (tc *tmuxControl) capturePaneByTarget(target string) (string, error) {
	return tc.sendCommand(fmt.Sprintf("capture-pane -t '%s' -e -p", target))
}

// targetForPaneID resolves %5 → Agent:1.2
func (tc *tmuxControl) targetForPaneID(paneID string) string {
	tc.mapMu.RLock()
	defer tc.mapMu.RUnlock()
	return tc.paneMap[paneID]
}
