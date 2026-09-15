package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
)

// Content-hash dedup for mobile uploads: the client sends the SHA-256 of the
// file it is about to upload; if we already have a file for that hash (and it
// still exists on disk), we hand back the existing path so the same photo/file
// isn't uploaded or stored twice. The hash is the ORIGINAL file's content, so
// dedup is stable even though images are re-encoded before upload.
var uploadHashes = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

func rememberUpload(hash, path string) {
	if hash == "" || path == "" {
		return
	}
	uploadHashes.Lock()
	uploadHashes.m[hash] = path
	uploadHashes.Unlock()
}

// lookupUpload returns the stored path for a hash, or "" if unknown or the file
// is gone (temp files can be reaped — drop the stale entry when that happens).
func lookupUpload(hash string) string {
	if hash == "" {
		return ""
	}
	uploadHashes.Lock()
	defer uploadHashes.Unlock()
	p := uploadHashes.m[hash]
	if p == "" {
		return ""
	}
	if _, err := os.Stat(p); err != nil {
		delete(uploadHashes.m, hash)
		return ""
	}
	return p
}

func handleUploadLookup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"path": lookupUpload(r.URL.Query().Get("hash"))})
}
