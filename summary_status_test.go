package main

import (
	"testing"
	"time"
)

func TestSummaryStatusRecent(t *testing.T) {
	now := time.Date(2026, 11, 24, 23, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	cases := []struct {
		name   string
		status string
		worked bool
		since  time.Time
		want   string
	}{
		{"just stopped", "idle", true, ago(time.Minute), "recent"},
		{"stopped 29m59s ago", "idle", true, ago(30*time.Minute - time.Second), "recent"},
		{"stopped exactly 30m ago", "idle", true, ago(30 * time.Minute), "idle"},
		{"stopped 2 days ago", "idle", true, ago(48 * time.Hour), "idle"},
		{"never worked", "idle", false, ago(time.Minute), "idle"},
		{"no stop time yet", "idle", true, time.Time{}, "idle"},
		{"working", "working", true, ago(time.Minute), "working"},
		{"waiting", "permission", true, ago(time.Minute), "permission"},
		{"error", "error", true, ago(time.Minute), "error"},
	}
	for _, c := range cases {
		if got := summaryStatus(c.status, c.worked, c.since, now); got != c.want {
			t.Errorf("%s: summaryStatus(%q) = %q, want %q", c.name, c.status, got, c.want)
		}
	}
}

// The trackers survive a restart: save, wipe memory, load again.
func TestStopStateRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stopped := time.Now().Add(-5 * time.Minute).Truncate(time.Second)

	idleMu.Lock()
	everWorked = map[string]bool{"a:1.1": true}
	idleSince = map[string]time.Time{"a:1.1": stopped, "b:1.1": stopped}
	snap := snapshotStopState()
	idleMu.Unlock()
	lastStopSave = ""
	saveStopState(snap)

	idleMu.Lock() // a restart: memory is empty
	everWorked = map[string]bool{}
	idleSince = map[string]time.Time{}
	idleMu.Unlock()
	loadStopState()

	idleMu.Lock()
	defer idleMu.Unlock()
	if !everWorked["a:1.1"] || everWorked["b:1.1"] {
		t.Fatalf("everWorked after load = %v", everWorked)
	}
	if !idleSince["a:1.1"].Equal(stopped) {
		t.Fatalf("idleSince[a] = %v, want %v", idleSince["a:1.1"], stopped)
	}
	if got := summaryStatus("idle", everWorked["a:1.1"], idleSince["a:1.1"], time.Now()); got != "recent" {
		t.Fatalf("after restart, a stopped 5 min ago = %q, want recent", got)
	}
}
