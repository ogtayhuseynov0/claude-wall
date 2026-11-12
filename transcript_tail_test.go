package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	tUser      = `{"type":"user","userType":"external","message":{"content":"%s"}}` + "\n"
	tAssistant = `{"type":"assistant","message":{"content":[{"type":"text","text":"%s"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":1}}}` + "\n"
)

func line(format, text string) string { return strings.Replace(format, "%s", text, 1) }

func TestTranscriptTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	var b strings.Builder
	b.WriteString(line(tUser, "old prompt"))
	// 3 MB of assistant lines: the prompt is outside the first 2 MB window.
	filler := line(tAssistant, strings.Repeat("x", 1000))
	for b.Len() < 3<<20 {
		b.WriteString(filler)
	}
	b.WriteString(line(tAssistant, "the reply"))
	os.WriteFile(path, []byte(b.String()), 0o644)

	if got := lastUserPrompt(path); got != "old prompt" {
		t.Fatalf("lastUserPrompt = %q", got)
	}
	if got := lastAssistantText(path); got != "the reply" {
		t.Fatalf("lastAssistantText = %q", got)
	}

	// Small file, no prompt at all: the whole file is read once, no error.
	small := filepath.Join(t.TempDir(), "small.jsonl")
	os.WriteFile(small, []byte(line(tAssistant, "hi")), 0o644)
	if got := lastUserPrompt(small); got != "" {
		t.Fatalf("lastUserPrompt(small) = %q", got)
	}
	if got := lastUserPrompt(filepath.Join(t.TempDir(), "missing.jsonl")); got != "" {
		t.Fatalf("lastUserPrompt(missing) = %q", got)
	}
}

func TestFinanceIncremental(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // save() writes under $HOME/.claude
	f := &financeStore{sessions: map[string]*sessionCost{}}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	ev := hookEvent{SessionID: "s1", TranscriptPath: path, EventName: "Stop"}
	turns := func() int { return f.sessions["s1"].Turns }

	os.WriteFile(path, []byte(line(tUser, "go")+line(tAssistant, "a")+line(tAssistant, "b")), 0o644)
	f.processTranscript(ev)
	if turns() != 2 || f.sessions["s1"].InputTok != 20 {
		t.Fatalf("first read: %+v", f.sessions["s1"])
	}

	// One new line and a partial one: count the full line only.
	partial := line(tAssistant, "d")
	fh, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fh.WriteString(line(tAssistant, "c") + partial[:20])
	fh.Close()
	f.processTranscript(ev)
	if turns() != 3 {
		t.Fatalf("second read: turns = %d", turns())
	}

	fh, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fh.WriteString(partial[20:])
	fh.Close()
	f.processTranscript(ev)
	sc := f.sessions["s1"]
	if sc.Turns != 4 || sc.InputTok != 40 || sc.OutputTok != 20 || sc.CacheRead != 400 || sc.CacheCreate != 4 {
		t.Fatalf("third read: %+v", sc)
	}

	// The file got shorter (rewritten): count from 0 again.
	os.WriteFile(path, []byte(line(tAssistant, "new")), 0o644)
	f.processTranscript(ev)
	if turns() != 1 {
		t.Fatalf("after rewrite: turns = %d", turns())
	}
}

func TestDailyIncremental(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.jsonl")
	msg := func(uuid string) string {
		return `{"type":"assistant","uuid":"` + uuid + `","timestamp":"2026-11-12T10:00:00Z","cwd":"/p/app","message":{"model":"claude-opus-5-5","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}` + "\n"
	}
	os.WriteFile(path, []byte(line(tUser, "go")+msg("a")+msg("b")), 0o644)
	st := parseTranscript(path, nil)

	// Append a new message, a duplicate uuid and half a line.
	tail := msg("d")
	fh, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fh.WriteString(msg("c") + msg("a") + tail[:30])
	fh.Close()
	st = parseTranscript(path, st)
	fh, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fh.WriteString(tail[30:])
	fh.Close()
	st = parseTranscript(path, st)

	full := parseTranscript(path, nil)
	for name, s := range map[string]*fileStats{"incremental": st, "full": full} {
		if b := s.days["2026-11-12"]; b.Msgs != 4 || b.Input != 40 {
			t.Errorf("%s: %+v", name, b)
		}
	}
}
