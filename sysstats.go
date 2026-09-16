package main

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
)

var (
	reCPUidle = regexp.MustCompile(`([\d.]+)%\s+idle`)
	reMemFree = regexp.MustCompile(`free percentage:\s*(\d+)%`)
	reGPUutil = regexp.MustCompile(`"Device Utilization %"=(\d+)`)
)

// startSysSampler launches the background metrics loop (idempotent enough for a
// single call from the server bootstrap).
func startSysSampler() {
	go func() {
		for {
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

	// CPU: the SECOND `top` sample is the accurate one (the first is since-boot).
	if out, err := exec.Command("top", "-l", "2", "-n", "0").Output(); err == nil {
		if m := reCPUidle.FindAllStringSubmatch(string(out), -1); len(m) > 0 {
			if idle, err := strconv.ParseFloat(m[len(m)-1][1], 64); err == nil {
				s.CPU = clampPct(100 - idle)
			}
		}
	}

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
	sysMu.RLock()
	s := sysCur
	sysMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(s)
}
