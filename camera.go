package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Pictures from this Mac's camera, taken from the phone: a look at who is in
// front of the screen while it is controlled over VNC. They live in
// ~/.claude/cw-camera, not the shared drive (agents read that), and the phone
// lists, views and deletes them.

// cameraDevice is the built-in camera's name as `ffmpeg -f avfoundation
// -list_devices true -i ""` prints it; the other entries are virtual cameras.
const cameraDevice = "FaceTime HD Camera"

func cameraDir() string { return filepath.Join(os.Getenv("HOME"), ".claude", "cw-camera") }

// Photo names are their capture time, so a client name can't leave the folder.
var cameraName = regexp.MustCompile(`^\d{8}-\d{6}(-\d+)?\.jpg$`)

// cameraMu runs one capture at a time.
var cameraMu sync.Mutex

type cameraPhoto struct {
	Name string `json:"name"`
	Time int64  `json:"time"` // unix secs
}

func cameraPhotos() []cameraPhoto {
	ents, _ := os.ReadDir(cameraDir())
	list := []cameraPhoto{}
	for _, e := range ents {
		if e.IsDir() || !cameraName.MatchString(e.Name()) {
			continue
		}
		if info, err := e.Info(); err == nil {
			list = append(list, cameraPhoto{e.Name(), info.ModTime().Unix()})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name > list[j].Name })
	return list
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func ffmpegBin() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return "/opt/homebrew/bin/ffmpeg"
}

func handleCameraList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"photos": cameraPhotos()})
}

func handleCameraSnap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	cameraMu.Lock()
	defer cameraMu.Unlock()
	dir := cameraDir()
	if err := os.MkdirAll(filepath.Join(dir, "thumbs"), 0700); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stamp := time.Now().Format("20060102-150405")
	name := stamp + ".jpg"
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			break
		}
		name = fmt.Sprintf("%s-%d.jpg", stamp, n)
	}
	// Written under a dot name, which the list skips, until it is complete.
	tmp := filepath.Join(dir, "."+name)
	defer os.Remove(tmp)

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	// Frame 45 is 1.5s in: the first frames are dark while the camera sets its
	// exposure.
	out, err := exec.CommandContext(ctx, ffmpegBin(), "-hide_banner", "-loglevel", "error",
		"-f", "avfoundation", "-framerate", "30", "-video_size", "1920x1080", "-i", cameraDevice+":none",
		"-vf", `select=gte(n\,45)`, "-frames:v", "1", "-q:v", "3", "-update", "1", "-y", tmp).CombinedOutput()
	if info, statErr := os.Stat(tmp); err != nil || statErr != nil || info.Size() == 0 {
		var msg []string
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l != "" && !strings.Contains(l, "NSCameraUseContinuityCameraDeviceType") {
				msg = append(msg, l)
			}
		}
		if len(msg) == 0 && err != nil {
			msg = append(msg, err.Error())
		}
		http.Error(w, "the camera gave no picture: "+strings.Join(msg, "; "), http.StatusInternalServerError)
		return
	}
	final := filepath.Join(dir, name)
	if err := os.Rename(tmp, final); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	os.Chmod(final, 0600)
	// A small copy for the history grid; the full picture is served if it fails.
	thumb := filepath.Join(dir, "thumbs", name)
	if exec.Command("/usr/bin/sips", "-Z", "480", final, "--out", thumb).Run() == nil {
		os.Chmod(thumb, 0600)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "name": name})
}

func handleCameraPhoto(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !cameraName.MatchString(name) {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	path := filepath.Join(cameraDir(), name)
	if r.URL.Query().Get("thumb") == "1" {
		if t := filepath.Join(cameraDir(), "thumbs", name); fileExists(t) {
			path = t
		}
	}
	if !fileExists(path) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// A name is never reused for another picture.
	w.Header().Set("Cache-Control", "private, max-age=604800")
	http.ServeFile(w, r, path)
}

func handleCameraDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var names []string
	if r.URL.Query().Get("all") == "1" {
		for _, p := range cameraPhotos() {
			names = append(names, p.Name)
		}
	} else if name := r.URL.Query().Get("name"); cameraName.MatchString(name) {
		names = []string{name}
	} else {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	for _, n := range names {
		os.Remove(filepath.Join(cameraDir(), n))
		os.Remove(filepath.Join(cameraDir(), "thumbs", n))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "deleted": len(names)})
}
