package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// buildVer is a hash of the shipped front-end files, computed at startup. The
// PWA polls /api/version and reloads once when it changes, so a deploy doesn't
// leave the phone on a stale cached page (iOS/PWA caches are sticky).
var buildVer string

func computeBuildVer() {
	h := sha1.New()
	for _, f := range []string{"static/m.html", "static/pip.html", "static/sw.js"} {
		if b, err := staticFiles.ReadFile(f); err == nil {
			h.Write(b)
		}
	}
	buildVer = hex.EncodeToString(h.Sum(nil))[:12]
}

func handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"version": buildVer})
}
