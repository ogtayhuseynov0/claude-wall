package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A shared "drive" folder any agent can drop files into, browsable + up/download
// from the phone. Lives at ~/claude-wall-drive. All paths are confined to it
// (no traversal out).

func driveDir() string {
	return filepath.Join(os.Getenv("HOME"), "claude-wall-drive")
}

// drivePath resolves a client-supplied relative path to an absolute path that is
// guaranteed to stay inside the drive dir. Returns ("", false) on traversal.
func drivePath(rel string) (string, bool) {
	root := driveDir()
	clean := filepath.Clean("/" + strings.TrimPrefix(rel, "/")) // force-absolute then clean, kills ../
	abs := filepath.Join(root, clean)
	if abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", false
	}
	return abs, true
}

type driveEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix secs
}

func handleDriveList(w http.ResponseWriter, r *http.Request) {
	os.MkdirAll(driveDir(), 0755)
	abs, ok := drivePath(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	list := make([]driveEntry, 0, len(ents))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		list = append(list, driveEntry{e.Name(), e.IsDir(), info.Size(), info.ModTime().Unix()})
	}
	// Folders first, then by most-recently-modified.
	sort.Slice(list, func(i, j int) bool {
		if list[i].IsDir != list[j].IsDir {
			return list[i].IsDir
		}
		return list[i].MTime > list[j].MTime
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	// `dir` is this folder's absolute path so the phone can copy a real path to
	// hand to an agent (so it writes files here).
	json.NewEncoder(w).Encode(map[string]any{"dir": abs, "entries": list})
}

func handleDriveDownload(w http.ResponseWriter, r *http.Request) {
	abs, ok := drivePath(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	name := filepath.Base(abs)
	// ?dl=1 forces a download; otherwise let the browser preview (images, pdf…).
	if r.URL.Query().Get("dl") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "")+`"`)
	}
	http.ServeFile(w, r, abs)
}

func handleDriveUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	dir, ok := drivePath(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.ParseMultipartForm(1024 << 20) // 1GB; local only
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	name := filepath.Base(header.Filename)
	if name == "" || name == "." || name == "/" {
		name = "upload-" + time.Now().Format("20060102-150405")
	}
	dst, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer dst.Close()
	if _, err := io.Copy(dst, file); err != nil {
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "name": name})
}

func handleDriveDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	abs, ok := drivePath(r.URL.Query().Get("path"))
	if !ok || abs == driveDir() {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	// Remove a file, or an empty directory (os.Remove refuses a non-empty dir).
	if err := os.Remove(abs); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
