package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
)

// Phone fit. A tmux session that no terminal is attached to has no client to
// size its window, so it keeps the size it was made at: a session launched
// from the phone is 80x24, split so Claude Code gets 10 rows, and the phone
// shows those 10 rows with the rest of the screen empty. While a phone views a
// pane of such a session, the pane gets taller to fill the phone's screen.
// Only rows are added: the width is never changed (a narrower Claude Code
// wraps its status line). The last phone to leave puts the window back as it
// was. A session with a terminal attached is never touched: the terminal owns
// its size.

type fitWindow struct {
	views  int
	w, h   int    // window size before the first phone view
	layout string // pane layout before the first phone view
}

var (
	fitMu   sync.Mutex
	fitWins = map[string]*fitWindow{} // by tmux window id (@N)
)

// phoneView is one phone connection's hold on a window.
type phoneView struct {
	wid  string
	once sync.Once
}

// tmuxFields reads tmux formats for target, one per field.
func tmuxFields(target string, formats ...string) []string {
	out, err := tmuxOutput("display-message", "-p", "-t", target, strings.Join(formats, "\t"))
	if err != nil {
		return nil
	}
	f := strings.Split(strings.TrimRight(out, "\n"), "\t")
	if len(f) != len(formats) {
		return nil
	}
	return f
}

// terminalsOn counts the terminals attached to a session. The hub's own
// control-mode client is attached too but never sizes a window, so it is left out.
func terminalsOn(session string) int {
	out, err := tmuxOutput("list-clients", "-t", session, "-F", "#{client_control_mode}")
	if err != nil {
		return 1 // unsure: leave the window alone
	}
	n := 0
	for _, l := range strings.Fields(out) {
		if l == "0" {
			n++
		}
	}
	return n
}

func atoi(s string) int {
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

// fitPhone makes target at least rows tall for a phone. Pass the connection's
// view from the previous call (nil the first time) and keep the one returned;
// release it when the connection closes.
func fitPhone(view *phoneView, target string, rows int) *phoneView {
	if rows < 5 || rows > 200 {
		return view
	}
	fitMu.Lock()
	defer fitMu.Unlock()
	f := tmuxFields(target, "#{session_id}", "#{window_id}", "#{window_width}",
		"#{window_height}", "#{window_layout}", "#{pane_height}", "#{pane_id}", "#{window_zoomed_flag}")
	if f == nil || terminalsOn(f[0]) > 0 {
		return view
	}
	wid, ww, wh, ph, pane := f[1], atoi(f[2]), atoi(f[3]), atoi(f[5]), f[6]
	if ph >= rows || ph <= 0 {
		return view // already fills the screen
	}
	if view == nil || view.wid != wid {
		if view != nil {
			releaseLocked(view.wid)
		}
		st := fitWins[wid]
		if st == nil {
			st = &fitWindow{w: ww, h: wh, layout: f[4]}
			fitWins[wid] = st
		}
		st.views++
		view = &phoneView{wid: wid}
		log.Printf("[phonefit] %s %s: %d rows for a phone (%d viewing, was %dx%d)", wid, target, rows, st.views, ww, wh)
	}
	resizeWindow(wid, ww, wh+rows-ph)
	if f[7] != "1" {
		tmuxExec("resize-pane", "-t", pane, "-y", fmt.Sprint(rows)) // the new rows go to this pane
	}
	return view
}

// resizeWindow sets the window size, then drops the "manual" window-size that
// resize-window leaves behind, so a terminal attaching later still sizes it.
func resizeWindow(wid string, cols, rows int) {
	tmuxExec("resize-window", "-t", wid, "-x", fmt.Sprint(cols), "-y", fmt.Sprint(rows))
	tmuxExec("set-option", "-w", "-u", "-t", wid, "window-size")
}

func (v *phoneView) release() {
	if v == nil {
		return
	}
	v.once.Do(func() {
		fitMu.Lock()
		defer fitMu.Unlock()
		releaseLocked(v.wid)
	})
}

func releaseLocked(wid string) {
	st := fitWins[wid]
	if st == nil {
		return
	}
	if st.views--; st.views > 0 {
		return
	}
	delete(fitWins, wid)
	f := tmuxFields(wid, "#{session_id}")
	if f == nil || terminalsOn(f[0]) > 0 {
		return // window is gone, or a terminal now sizes it
	}
	resizeWindow(wid, st.w, st.h)
	tmuxExec("select-layout", "-t", wid, st.layout)
	log.Printf("[phonefit] %s: last phone left, back to %dx%d", wid, st.w, st.h)
}
