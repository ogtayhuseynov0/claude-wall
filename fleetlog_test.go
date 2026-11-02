package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendRule(t *testing.T) {
	p := filepath.Join(t.TempDir(), "CLAUDE.md")

	// Missing file: created with the section.
	if err := appendRule(p, "Verify UI in real Chrome, never headless"); err != nil {
		t.Fatal(err)
	}
	// Existing section: appended under it, before later sections.
	os.WriteFile(p, []byte("# Proj\n\n"+flRulesHead+"\n\n- one\n\n## Other\n- keep\n"), 0644)
	if err := appendRule(p, "two"); err != nil {
		t.Fatal(err)
	}
	// Duplicate: no-op.
	if err := appendRule(p, "two"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	want := "# Proj\n\n" + flRulesHead + "\n\n- one\n- two\n\n## Other\n- keep\n"
	if string(b) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b, want)
	}

	// File without the section: section added at the end.
	q := filepath.Join(t.TempDir(), "CLAUDE.md")
	os.WriteFile(q, []byte("# Rules\n- a"), 0644)
	appendRule(q, "b")
	b, _ = os.ReadFile(q)
	if string(b) != "# Rules\n- a\n\n"+flRulesHead+"\n\n- b\n" {
		t.Fatalf("got:\n%q", b)
	}
}

func TestDigestTranscript(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	lines := []string{
		`{"type":"user","message":{"content":"add a copy button to each drive row"}}`,
		`{"type":"user","isMeta":true,"message":{"content":"meta noise"}}`,
		`{"type":"user","message":{"content":"<command-name>/model</command-name>"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Adding it."},{"type":"tool_use","name":"Edit","input":{"file_path":"/x/m.html"}},{"type":"tool_use","name":"Edit","input":{"file_path":"/x/m.html"}},{"type":"tool_use","name":"Bash","input":{"command":"go build ./..."}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"exit 1: undefined: fmtTime"}]}}`,
		`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"subagent chatter"}]}}`,
	}
	body := strings.Join(lines, "\n") + "\n"
	partial := `{"type":"assistant","message":{"content":[{"type":"text","text":"half wr`
	os.WriteFile(p, []byte(body+partial), 0644)

	d, end, err := digestTranscript(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"USER: add a copy button", "AGENT: Adding it.", "EDITED: /x/m.html", "RAN: go build ./...", "ERROR: exit 1: undefined: fmtTime"} {
		if !strings.Contains(d, want) {
			t.Errorf("digest missing %q:\n%s", want, d)
		}
	}
	for _, bad := range []string{"meta noise", "/model", "subagent chatter", "half wr"} {
		if strings.Contains(d, bad) {
			t.Errorf("digest should not contain %q", bad)
		}
	}
	if strings.Count(d, "EDITED:") != 1 {
		t.Errorf("repeated edits of one file should be listed once:\n%s", d)
	}
	if end != int64(len(body)) {
		t.Fatalf("cursor should stop before the partial line: got %d want %d", end, len(body))
	}

	// Next read starts at the cursor and picks up the now-complete line only.
	os.WriteFile(p, []byte(body+`{"type":"assistant","message":{"content":[{"type":"text","text":"Done, copy button live."}]}}`+"\n"), 0644)
	d2, _, _ := digestTranscript(p, end)
	if strings.TrimSpace(d2) != "AGENT: Done, copy button live." {
		t.Fatalf("incremental digest: %q", d2)
	}
}

func TestIsDupAndApply(t *testing.T) {
	f := &fleetLogStore{data: fleetLogData{Projects: map[string]logProject{}, Cursors: map[string]int64{}}}
	x := &logExtraction{Done: []string{"Copy button on each drive row"}, Todo: []string{"Upload progress bar"}}
	if n := f.apply("claude-wall", "s1", "", x); n != 2 {
		t.Fatalf("first apply added %d, want 2", n)
	}
	// Same items again (different punctuation/case) are dropped.
	x2 := &logExtraction{Done: []string{"copy button on each drive row."}, Todo: []string{"Upload progress bar"}}
	if n := f.apply("claude-wall", "s1", "", x2); n != 0 {
		t.Fatalf("dup apply added %d, want 0", n)
	}
	// Closing by id.
	todo := f.openItems("claude-wall", 10)
	if len(todo) != 1 {
		t.Fatalf("open items: %d", len(todo))
	}
	f.apply("claude-wall", "s1", "", &logExtraction{Closed: []string{todo[0].ID}})
	if len(f.openItems("claude-wall", 10)) != 0 {
		t.Fatal("todo should be closed")
	}
}
