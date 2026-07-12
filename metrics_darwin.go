//go:build darwin

package main

import (
	"bufio"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// cpuPctBits holds the latest CPU busy% as float64 bits, fed by a persistent
// `iostat` stream. Exact CPU time on darwin needs mach host_processor_info
// (cgo, unavailable under CGO_ENABLED=0), so we read kernel-computed us+sy
// from iostat instead of approximating with load average.
var cpuPctBits uint64

func readCPUPercent() float64 {
	return math.Float64frombits(atomic.LoadUint64(&cpuPctBits))
}

// initPlatformMetrics starts one long-lived `iostat -w 2` (no process-table
// walk, unlike top) and parses the per-interval CPU columns. Restarts on exit.
func initPlatformMetrics() {
	go func() {
		for {
			runIostat()
			time.Sleep(2 * time.Second) // iostat died — back off, retry
		}
	}()
}

func runIostat() {
	cmd := exec.Command("iostat", "-w", "2")
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return
	}
	defer cmd.Wait()
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		// Data row tail: ... us sy id 1m 5m 15m  → anchor from the right so a
		// varying number of disk columns can't shift the CPU fields.
		f := strings.Fields(sc.Text())
		if len(f) < 6 {
			continue
		}
		us, e1 := strconv.ParseFloat(f[len(f)-6], 64)
		sy, e2 := strconv.ParseFloat(f[len(f)-5], 64)
		id, e3 := strconv.ParseFloat(f[len(f)-4], 64)
		if e1 != nil || e2 != nil || e3 != nil {
			continue // header line
		}
		busy := us + sy
		if busy < 0 {
			busy = 0
		}
		if busy > 100 {
			busy = 100
		}
		_ = id
		atomic.StoreUint64(&cpuPctBits, math.Float64bits(busy))
	}
}

var vmPageRe = regexp.MustCompile(`page size of (\d+) bytes`)

// readMem parses vm_stat (a tiny, fast binary — no process-table walk) for
// memory pressure. used ≈ active + wired + compressor-occupied, matching
// Activity Monitor's "Memory Used".
func readMem() (used, total uint64) {
	if t, err := unix.SysctlUint64("hw.memsize"); err == nil {
		total = t
	}
	out, err := exec.Command("/usr/bin/vm_stat").Output()
	if err != nil {
		return 0, total
	}
	text := string(out)
	pageSize := uint64(4096)
	if m := vmPageRe.FindStringSubmatch(text); m != nil {
		if ps, e := strconv.ParseUint(m[1], 10, 64); e == nil {
			pageSize = ps
		}
	}
	pages := func(label string) uint64 {
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, label) {
				v := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line[len(label):]), "."))
				n, _ := strconv.ParseUint(v, 10, 64)
				return n
			}
		}
		return 0
	}
	active := pages("Pages active:")
	wired := pages("Pages wired down:")
	compressed := pages("Pages occupied by compressor:")
	used = (active + wired + compressed) * pageSize
	if total > 0 && used > total {
		used = total
	}
	return used, total
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

// readNetBytes sums cumulative rx/tx bytes across interfaces via `netstat -ibn`.
// Columns are anchored from the right so an empty Address column can't shift
// the byte fields. Loopback excluded; each interface counted once.
func readNetBytes() (rx, tx uint64) {
	out, err := exec.Command("/usr/sbin/netstat", "-ibn").Output()
	if err != nil {
		return 0, 0
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) == 0 {
		return 0, 0
	}
	// Header → count columns trailing after Obytes (Coll, maybe Drop).
	header := strings.Fields(lines[0])
	obytesCol := -1
	for i, h := range header {
		if h == "Obytes" {
			obytesCol = i
		}
	}
	if obytesCol < 0 {
		return 0, 0
	}
	trailing := len(header) - 1 - obytesCol // columns after Obytes

	seen := map[string]bool{}
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) < trailing+4 {
			continue
		}
		name := f[0]
		if name == "lo0" || seen[name] {
			continue
		}
		oIdx := len(f) - 1 - trailing // Obytes
		iIdx := oIdx - 3              // Ibytes (Opkts, Oerrs sit between)
		if iIdx < 1 {
			continue
		}
		ib, e1 := strconv.ParseUint(f[iIdx], 10, 64)
		ob, e2 := strconv.ParseUint(f[oIdx], 10, 64)
		if e1 != nil || e2 != nil {
			continue // skipped header-ish or shifted row
		}
		seen[name] = true
		rx += ib
		tx += ob
	}
	return rx, tx
}
