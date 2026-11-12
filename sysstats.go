package main

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// System stats for the mobile header (CPU / RAM / disk / GPU). A background
// sampler runs the metrics on a ticker so the HTTP endpoint returns cached
// values instantly (some commands, e.g. `top -l2`, take a couple of seconds).

type sysStats struct {
	CPU  int `json:"cpu"`  // % busy (100 - idle)
	Mem  int `json:"mem"`  // % used (100 - free, via memory_pressure)
	Disk int `json:"disk"` // % used of the data volume
	GPU  int `json:"gpu"`  // % utilization, -1 if unavailable
}

var (
	sysMu  sync.RWMutex
	sysCur = sysStats{CPU: -1, Mem: -1, Disk: -1, GPU: -1}
	// sysAsked is the unix time of the last /api/sysstats call. The sampler
	// only runs while someone reads it (the phone header polls every 4 s).
	sysAsked atomic.Int64
)

var (
	reMemFree = regexp.MustCompile(`free percentage:\s*(\d+)%`)
	reGPUutil = regexp.MustCompile(`"Device Utilization %"=(\d+)`)
)

// startSysSampler launches the background metrics loop (idempotent enough for a
// single call from the server bootstrap).
func startSysSampler() {
	go func() {
		for {
			if time.Since(time.Unix(sysAsked.Load(), 0)) > 20*time.Second {
				time.Sleep(time.Second)
				continue
			}
			s := sampleSys()
			sysMu.Lock()
			sysCur = s
			sysMu.Unlock()
			time.Sleep(3 * time.Second)
		}
	}()
}

func sampleSys() sysStats {
	s := sysStats{CPU: -1, Mem: -1, Disk: -1, GPU: -1}

	// CPU: from the iostat stream the metrics already run (metrics_darwin.go).
	// `top -l 2` cost 1.5 CPU-s per sample.
	s.CPU = clampPct(readCPUPercent())

	// RAM: macOS "used" always looks ~full (it caches), so use memory pressure's
	// free percentage — a meaningful signal — and report the complement.
	if out, err := exec.Command("memory_pressure").Output(); err == nil {
		if m := reMemFree.FindStringSubmatch(string(out)); m != nil {
			if free, err := strconv.Atoi(m[1]); err == nil {
				s.Mem = clampPct(float64(100 - free))
			}
		}
	}

	// Disk: the data volume (/ is the tiny sealed system volume).
	if out, err := exec.Command("df", "-k", "/System/Volumes/Data").Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) >= 2 {
			f := strings.Fields(lines[len(lines)-1])
			for _, tok := range f {
				if strings.HasSuffix(tok, "%") {
					if v, err := strconv.Atoi(strings.TrimSuffix(tok, "%")); err == nil {
						s.Disk = clampPct(float64(v))
					}
					break
				}
			}
		}
	}

	// GPU: Apple Silicon exposes utilization via IOAccelerator (no sudo needed).
	if out, err := exec.Command("ioreg", "-r", "-d1", "-c", "IOAccelerator").Output(); err == nil {
		if m := reGPUutil.FindStringSubmatch(string(out)); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				s.GPU = clampPct(float64(v))
			}
		}
	}
	return s
}

func clampPct(f float64) int {
	v := int(f + 0.5)
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func handleSysStats(w http.ResponseWriter, r *http.Request) {
	sysAsked.Store(time.Now().Unix())
	sysMu.RLock()
	s := sysCur
	sysMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(s)
}
