package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

	// Liveness. The tmux connection can die at any time (tmux server restart,
	// `tmux kill-server`, the socket going away). When it does, readLoop ends
	// and every later sendCommand would otherwise block for the full 5s
	// timeout — with N subscribed panes captured per tick, that wedges the hub
	// while HTTP keeps answering 200, which is exactly how this looked like a
	// "crash" from outside. dead is closed once, so the hub can watch it and
	// reconnect.
	dead     chan struct{}
	deadOnce sync.Once
	stopping atomic.Bool // set by stop(): a deliberate shutdown, not a failure
}

func newTmuxControl() *tmuxControl {
	return &tmuxControl{
		paneMap:   make(map[string]string),
		targetMap: make(map[string]string),
		dead:      make(chan struct{}),
	}
}

func (tc *tmuxControl) start() error {
	sweepOrphanedControlClients()

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
		// Tear the half-open connection down properly, or this failed attempt
		// leaves a client on the tmux server exactly like a missed stop() does.
		tc.stop()
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

// stop detaches the control-mode client and reaps the process.
//
// This must be called on every exit path. A `tmux -C attach` that is merely
// orphaned stays registered as a client on the tmux server for as long as the
// server lives, so a process that skips this leaves a client behind on every
// restart — they accumulate until several of them are fighting over the same
// event stream.
func (tc *tmuxControl) stop() {
	if tc == nil {
		return
	}
	tc.stopping.Store(true)
	if tc.cmd == nil || tc.cmd.Process == nil {
		tc.markDead()
		return
	}
	tc.sendRaw("detach")

	// Give tmux a moment to drop the client cleanly, then make sure.
	done := make(chan struct{})
	go func() { tc.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(700 * time.Millisecond):
		tc.cmd.Process.Kill()
		<-done
	}
	tc.markDead()
}

// markDead closes the dead channel once and unblocks anyone waiting on a
// response, so callers fail immediately instead of each burning the 5s timeout.
func (tc *tmuxControl) markDead() {
	tc.deadOnce.Do(func() {
		close(tc.dead)
		tc.pendingMu.Lock()
		for _, ch := range tc.pending {
			close(ch)
		}
		tc.pending = nil
		tc.pendingMu.Unlock()
	})
}

// alive reports whether the control connection is still usable.
func (tc *tmuxControl) alive() bool {
	if tc == nil {
		return false
	}
	select {
	case <-tc.dead:
		return false
	default:
		return true
	}
}

// sendCommand sends a tmux command and returns the response.
func (tc *tmuxControl) sendCommand(command string) (string, error) {
	if !tc.alive() {
		return "", errControlDead
	}

	ch := make(chan string, 1)

	tc.pendingMu.Lock()
	tc.pending = append(tc.pending, ch)
	tc.pendingMu.Unlock()

	if err := tc.sendRaw(command); err != nil {
		tc.markDead()
		return "", err
	}

	select {
	case result, ok := <-ch:
		if !ok {
			return "", errControlDead
		}
		return result, nil
	case <-tc.dead:
		return "", errControlDead
	case <-time.After(5 * time.Second):
		// A timeout means the protocol went out of step: the response we were
		// queued for is never coming, and every later caller would inherit the
		// mismatch. Treat the connection as lost so it gets rebuilt.
		tc.markDead()
		return "", fmt.Errorf("timeout waiting for response to: %s", command)
	}
}

// errControlDead is returned once the control connection is gone, so callers
// can fall back immediately rather than waiting on a pipe nobody is reading.
var errControlDead = errors.New("tmux control connection closed")

func (tc *tmuxControl) sendRaw(command string) error {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if _, err := tc.stdin.WriteString(command + "\n"); err != nil {
		return err
	}
	// A write to a pipe whose reader is gone fails here. Surfacing it is the
	// difference between one logged reconnect and a silently frozen dashboard.
	return tc.stdin.Flush()
}

// readLoop parses the tmux control mode protocol.
func (tc *tmuxControl) readLoop(scanner *bufio.Scanner) {
	// However this loop ends — tmux server gone, socket closed, scanner error —
	// the connection is finished. Say so, and wake anyone waiting.
	defer func() {
		if err := scanner.Err(); err != nil {
			log.Printf("[tmuxctl] control stream ended: %v", err)
		} else if !tc.stopping.Load() {
			log.Println("[tmuxctl] control stream closed by tmux")
		}
		tc.markDead()
	}()

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

// sweepOrphanedControlClients detaches control-mode clients left behind by
// dead servers.
//
// A `tmux -C attach` outlives the process that spawned it: when the parent goes
// away without detaching, the client keeps running, reparented to launchd, and
// stays registered on the tmux server consuming the same event stream. Several
// of them at once is how the wall ended up frozen on a stale frame. Shutdown
// now detaches properly, but clients leaked by earlier builds survive until the
// tmux server itself restarts, so clear them on the way in.
//
// The test is ownership, not age: our own client is a child of this process, so
// anything whose parent is PID 1 belongs to a server that no longer exists.
func sweepOrphanedControlClients() {
	out, err := tmuxOutput("list-clients", "-F", "#{client_name}\t#{client_pid}\t#{client_control_mode}")
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 || parts[2] != "1" {
			continue
		}
		pid, err := strconv.Atoi(parts[1])
		if err != nil || pid <= 1 {
			continue
		}
		if ppidOf(pid) != 1 {
			continue // owned by a live process — leave it alone
		}
		log.Printf("[tmuxctl] detaching orphaned control client %s (pid %d)", parts[0], pid)
		exec.Command("tmux", "detach-client", "-t", parts[0]).Run()
		syscall.Kill(pid, syscall.SIGTERM)
	}
}

// ppidOf returns a process's parent PID, or 0 if it cannot be read.
func ppidOf(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return ppid
}
