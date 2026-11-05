package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os/exec"
	"strings"
)

// lockJXA calls SACLockScreenImmediate from the private login framework: the
// same lock as Control-Command-Q or Apple menu > Lock Screen. It needs no
// Accessibility or Automation permission, unlike sending the keystroke.
const lockJXA = `ObjC.import('Foundation');
$.NSBundle.bundleWithPath('/System/Library/PrivateFrameworks/login.framework').load;
ObjC.bindFunction('SACLockScreenImmediate', ['int', []]);
$.SACLockScreenImmediate();`

// handleLock locks this Mac from the phone, e.g. after unlocking it over VNC.
// Sessions keep running behind the lock screen.
func handleLock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	out, err := exec.Command("/usr/bin/osascript", "-l", "JavaScript", "-e", lockJXA).CombinedOutput()
	if err != nil {
		// Sleeping the display locks too while a password is required
		// right after sleep.
		log.Printf("[lock] login framework failed (%v: %s), sleeping the display", err, strings.TrimSpace(string(out)))
		if out, err := exec.Command("/usr/bin/pmset", "displaysleepnow").CombinedOutput(); err != nil {
			http.Error(w, "could not lock: "+strings.TrimSpace(string(out)), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
