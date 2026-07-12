package main

import (
	"encoding/json"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// SysMetrics is the payload pushed to the header over /ws/system.
type SysMetrics struct {
	CPU       float64 `json:"cpu"`       // 0..100 percent
	Mem       float64 `json:"mem"`       // 0..100 percent
	Disk      float64 `json:"disk"`      // 0..100 percent
	NetDown   float64 `json:"netDown"`   // bytes/sec received
	NetUp     float64 `json:"netUp"`     // bytes/sec sent
	MemUsed   uint64  `json:"memUsed"`   // bytes
	MemTotal  uint64  `json:"memTotal"`  // bytes
	DiskUsed  uint64  `json:"diskUsed"`  // bytes
	DiskTotal uint64  `json:"diskTotal"` // bytes
	NCPU      int     `json:"ncpu"`
}

// metricsHub fans out the latest sample to all connected header sockets.
type metricsHub struct {
	mu      sync.RWMutex
	subs    map[chan []byte]struct{}
	latest  []byte
	started bool
}

var sysMetrics = &metricsHub{subs: map[chan []byte]struct{}{}}

func (h *metricsHub) register() chan []byte {
	ch := make(chan []byte, 4)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	snapshot := h.latest
	h.mu.Unlock()
	if snapshot != nil {
		ch <- snapshot // populate header immediately on connect
	}
	return ch
}

func (h *metricsHub) unregister(ch chan []byte) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *metricsHub) broadcast(b []byte) {
	h.mu.Lock()
	h.latest = b
	for ch := range h.subs {
		select {
		case ch <- b:
		default: // slow client — drop frame
		}
	}
	h.mu.Unlock()
}

// startMetrics samples system stats every 2s and broadcasts. Cheap: native
// syscalls where possible, one tiny subprocess on darwin for memory/net.
func startMetrics() {
	h := sysMetrics
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return
	}
	h.started = true
	h.mu.Unlock()

	initPlatformMetrics()

	const interval = 2 * time.Second
	var prevRx, prevTx uint64
	var prevNet time.Time
	first := true

	tick := func() {
		var m SysMetrics
		m.NCPU = runtime.NumCPU()
		m.CPU = readCPUPercent()
		used, total := readMem()
		m.MemUsed, m.MemTotal = used, total
		if total > 0 {
			m.Mem = float64(used) / float64(total) * 100
		}
		du, dt := readDisk()
		m.DiskUsed, m.DiskTotal = du, dt
		if dt > 0 {
			m.Disk = float64(du) / float64(dt) * 100
		}
		rx, tx := readNetBytes()
		now := time.Now()
		if !first {
			dtSec := now.Sub(prevNet).Seconds()
			if dtSec > 0 {
				if rx >= prevRx {
					m.NetDown = float64(rx-prevRx) / dtSec
				}
				if tx >= prevTx {
					m.NetUp = float64(tx-prevTx) / dtSec
				}
			}
		}
		prevRx, prevTx, prevNet, first = rx, tx, now, false

		if b, err := json.Marshal(m); err == nil {
			h.broadcast(b)
		}
	}

	tick() // prime net counters so the next tick has a delta
	for range time.NewTicker(interval).C {
		tick()
	}
}

func handleSystemWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ch := sysMetrics.register()
	defer sysMetrics.unregister(ch)

	// Drain client pings/closes so the read pump notices disconnects.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				conn.Close()
				return
			}
		}
	}()

	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case b, ok := <-ch:
			if !ok {
				return
			}
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
				return
			}
		case <-ping.C:
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
