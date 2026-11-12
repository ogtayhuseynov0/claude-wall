package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSchedulerSave(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".claude"), 0o755)

	// A file that does not parse is moved aside, not overwritten later.
	os.WriteFile(schedulerFile(), []byte("{broken"), 0o644)
	s := &scheduler{tasks: map[string]*scheduledTask{}}
	s.load()
	if _, err := os.Stat(schedulerFile()); !os.IsNotExist(err) {
		t.Fatalf("broken file still in place: %v", err)
	}
	if m, _ := filepath.Glob(schedulerFile() + ".bad-*"); len(m) != 1 {
		t.Fatalf("moved-aside files: %v", m)
	}

	// Concurrent saves: the file is always valid JSON, and the last state wins.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.mu.Lock()
			s.tasks["t"] = &scheduledTask{ID: "t", Attempts: i}
			s.mu.Unlock()
			s.save()
		}(i)
	}
	wg.Wait()
	s.mu.Lock()
	s.tasks["t"] = &scheduledTask{ID: "t", Status: "stopped"}
	s.mu.Unlock()
	s.save()

	s2 := &scheduler{tasks: map[string]*scheduledTask{}}
	s2.load()
	if s2.tasks["t"] == nil || s2.tasks["t"].Status != "stopped" {
		t.Fatalf("reloaded: %+v", s2.tasks["t"])
	}
	if st, _ := os.Stat(schedulerFile()); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}
