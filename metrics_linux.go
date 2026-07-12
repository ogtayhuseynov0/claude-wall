//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var (
	cpuMu       sync.Mutex
	prevTotal   uint64
	prevIdle    uint64
	havePrevCPU bool
)

// /proc/stat delta gives accurate CPU% directly — no helper sampler needed.
func initPlatformMetrics() {}

// readCPUPercent computes busy% from the delta of /proc/stat aggregate jiffies.
func readCPUPercent() float64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	line := data
	if i := strings.IndexByte(string(data), '\n'); i > 0 {
		line = data[:i]
	}
	fields := strings.Fields(string(line))
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0
	}
	var total, idle uint64
	for i := 1; i < len(fields); i++ {
		v, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			continue
		}
		total += v
		if i == 4 || i == 5 { // idle + iowait
			idle += v
		}
	}
	cpuMu.Lock()
	defer cpuMu.Unlock()
	if !havePrevCPU {
		prevTotal, prevIdle, havePrevCPU = total, idle, true
		return 0
	}
	dt := total - prevTotal
	di := idle - prevIdle
	prevTotal, prevIdle = total, idle
	if dt == 0 {
		return 0
	}
	return float64(dt-di) / float64(dt) * 100
}

func readMem() (used, total uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var memTotal, memAvail uint64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64) // kB
		switch fields[0] {
		case "MemTotal:":
			memTotal = v * 1024
		case "MemAvailable:":
			memAvail = v * 1024
		}
	}
	if memTotal == 0 {
		return 0, 0
	}
	if memAvail > memTotal {
		memAvail = memTotal
	}
	return memTotal - memAvail, memTotal
}

func readDisk() (used, total uint64) {
	var st unix.Statfs_t
	if err := unix.Statfs("/", &st); err != nil {
		return 0, 0
	}
	bs := uint64(st.Bsize)
	total = st.Blocks * bs
	used = (st.Blocks - st.Bfree) * bs
	return used, total
}

// readNetBytes sums rx/tx across non-loopback interfaces from /proc/net/dev.
func readNetBytes() (rx, tx uint64) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		if name == "lo" || name == "" {
			continue
		}
		fields := strings.Fields(line[idx+1:])
		if len(fields) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(fields[0], 10, 64) // rx bytes
		t, _ := strconv.ParseUint(fields[8], 10, 64) // tx bytes
		rx += r
		tx += t
	}
	return rx, tx
}
