package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fleet Log: one central logbook of what every agent did and what is left,
// per project. Entries arrive two ways:
//
//   - automatically: when an agent's turn ends (Stop hook) and the session has
//     been quiet for a moment, a cheap model reads what happened since the last
//     look and files done / todo / waiting / decision / lesson / rule entries;
//   - directly: agents (or you) post with `cw log <kind> "<text>"`.
//
// Rule entries are "teach once" suggestions: corrections you gave an agent,
// which you can save into the project's or the global CLAUDE.md from the page.
// Stored in ~/.claude/claude-wall/fleetlog.json.

type logEntry struct {
	ID       string `json:"id"`
	Project  string `json:"project"`
	Kind     string `json:"kind"`             // todo, done, waiting, decision, lesson, rule, note
	Text     string `json:"text"`             // lesson: the problem as it shows up
	Detail   string `json:"detail,omitempty"` // lesson: the fix · rule: scope (project|global)
	Status   string `json:"status,omitempty"` // todo/waiting: open|closed · rule: pending|saved|dismissed
	Source   string `json:"source"`           // auto, agent, you
	Session  string `json:"session,omitempty"`
	Pane     string `json:"pane,omitempty"`
	At       int64  `json:"at"` // unix ms
	ClosedAt int64  `json:"closedAt,omitempty"`
}

type logProject struct {
	Name string `json:"name"`
	Root string `json:"root"`
}

type fleetLogData struct {
	Entries  []logEntry            `json:"entries"`
	Projects map[string]logProject `json:"projects"` // name → project
	Cursors  map[string]int64      `json:"cursors"`  // transcript path → bytes already read
}

type fleetLogStore struct {
	mu   sync.Mutex
	data fleetLogData

	saveMu sync.Mutex // serialises file writes; never held together with mu

	pending map[string]hookEvent // session → latest Stop not yet read
	stopAt  map[string]time.Time // session → when that Stop arrived
	lastRun map[string]time.Time // session → last extraction
}

const (
	flQuiet      = 2 * time.Minute  // let a burst of back-and-forth settle before reading
	flMinGap     = 15 * time.Minute // per session at most one read per gap, so steps fold into one line
	flMinDigest  = 1200             // smaller stretches wait for more (small exchanges rarely matter)
	flMaxEntries = 6000
	flDigestMax  = 14000 // chars of transcript digest sent to the model
	flRulesHead  = "## Rules (from Fleet Log)"
)

var fleetLog *fleetLogStore

func fleetLogDir() string       { return filepath.Join(os.Getenv("HOME"), ".claude", "claude-wall") }
func fleetLogFile() string      { return filepath.Join(fleetLogDir(), "fleetlog.json") }
func fleetLogWorkerDir() string { return filepath.Join(fleetLogDir(), "fleetlog-worker") }

func initFleetLog() {
	fleetLog = &fleetLogStore{
		pending: map[string]hookEvent{},
		stopAt:  map[string]time.Time{},
		lastRun: map[string]time.Time{},
	}
	fleetLog.load()
	os.MkdirAll(fleetLogWorkerDir(), 0755)
	go fleetLog.worker()
}

func (f *fleetLogStore) load() {
	// `touch ~/.claude/claude-wall/fleetlog.reset` + restart starts a fresh log;
	// the old one is kept as fleetlog-<time>.json.
	marker := filepath.Join(fleetLogDir(), "fleetlog.reset")
	if _, err := os.Stat(marker); err == nil {
		os.Rename(fleetLogFile(), filepath.Join(fleetLogDir(), "fleetlog-"+time.Now().Format("20060102-150405")+".json"))
		os.Remove(marker)
		log.Printf("[fleetlog] reset: started a fresh log")
	}
	if b, err := os.ReadFile(fleetLogFile()); err == nil {
		json.Unmarshal(b, &f.data)
	}
	if f.data.Projects == nil {
		f.data.Projects = map[string]logProject{}
	}
	if f.data.Cursors == nil {
		f.data.Cursors = map[string]int64{}
	}
}

// save writes a snapshot to disk. Caller must NOT hold f.mu.
func (f *fleetLogStore) save() {
	f.mu.Lock()
	b, err := json.Marshal(f.data)
	f.mu.Unlock()
	if err != nil {
		return
	}
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	os.MkdirAll(fleetLogDir(), 0755)
	tmp := fleetLogFile() + ".tmp"
	if os.WriteFile(tmp, b, 0644) == nil {
		os.Rename(tmp, fleetLogFile())
	}
}

func newLogID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ── projects ────────────────────────────────────────────────────────────────

var projCache sync.Map // cwd → logProject

// projectFor maps a working directory to its project: the main repo's folder
// (so wt worktrees and subdirectories land on the same project), else the
// directory itself.
func projectFor(cwd string) logProject {
	if cwd == "" {
		return logProject{Name: "unknown"}
	}
	if v, ok := projCache.Load(cwd); ok {
		return v.(logProject)
	}
	p := logProject{Name: filepath.Base(cwd), Root: cwd}
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--path-format=absolute", "--git-common-dir").Output(); err == nil {
		dir := strings.TrimSpace(string(out))
		if filepath.Base(dir) == ".git" {
			p.Root = filepath.Dir(dir)
			p.Name = filepath.Base(p.Root)
		}
	}
	if p.Root == os.Getenv("HOME") {
		p.Name = "home"
	}
	projCache.Store(cwd, p)
	return p
}

// ── capture after each agent run ───────────────────────────────────────────

// onStop queues a session for reading once it has been quiet for flQuiet.
// Cheap: called inline from the hook handler.
func (f *fleetLogStore) onStop(ev hookEvent) {
	if f == nil || ev.TranscriptPath == "" || ev.SessionID == "" {
		return
	}
	if strings.HasPrefix(ev.CWD, fleetLogWorkerDir()) {
		return // our own extraction runs
	}
	f.mu.Lock()
	f.pending[ev.SessionID] = ev
	f.stopAt[ev.SessionID] = time.Now()
	f.mu.Unlock()
}

func (f *fleetLogStore) worker() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		if ev, ok := f.nextDue(); ok {
			f.extract(ev)
		}
	}
}

func (f *fleetLogStore) nextDue() (hookEvent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	best := ""
	var bestAt time.Time
	for sid, at := range f.stopAt {
		if _, ok := f.pending[sid]; !ok {
			if now.Sub(at) > 6*time.Hour {
				delete(f.stopAt, sid)
				delete(f.lastRun, sid)
			}
			continue
		}
		if now.Sub(at) < flQuiet || now.Sub(f.lastRun[sid]) < flMinGap {
			continue
		}
		if best == "" || at.Before(bestAt) {
			best, bestAt = sid, at
		}
	}
	if best == "" {
		return hookEvent{}, false
	}
	ev := f.pending[best]
	delete(f.pending, best)
	f.lastRun[best] = now
	return ev, true
}

func (f *fleetLogStore) extract(ev hookEvent) {
	proj := projectFor(ev.CWD)
	f.mu.Lock()
	cursor := f.data.Cursors[ev.TranscriptPath]
	f.data.Projects[proj.Name] = proj
	f.mu.Unlock()

	digest, end, err := digestTranscript(ev.TranscriptPath, cursor)
	if err != nil {
		return
	}
	if len(digest) < flMinDigest {
		// Leave the cursor: this stretch is read together with whatever comes next.
		return
	}
	res, err := runLogExtraction(proj.Name, digest, f.openItems(proj.Name, 30))
	if err != nil {
		// Keep the cursor where it was: the next Stop retries this stretch.
		log.Printf("[fleetlog] extract %s: %v", proj.Name, err)
		return
	}
	f.mu.Lock()
	f.data.Cursors[ev.TranscriptPath] = end
	f.mu.Unlock()
	n := f.apply(proj.Name, ev.SessionID, ev.PaneTarget, res)
	log.Printf("[fleetlog] %s: +%d entries", proj.Name, n)
	f.save()
}

var (
	reSysReminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	reWS          = regexp.MustCompile(`\s+`)
)

func clip(s string, n int) string {
	s = strings.TrimSpace(reWS.ReplaceAllString(s, " "))
	if len([]rune(s)) > n {
		return string([]rune(s)[:n]) + "…"
	}
	return s
}

// digestTranscript turns the transcript from byte offset `from` to its last
// complete line into a compact text the model can read: USER / AGENT lines and
// the agent's actions. With no cursor yet it reads only the recent tail.
// Returns the digest and the offset the next read should start from.
func digestTranscript(path string, from int64) (string, int64, error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", from, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return "", from, err
	}
	size := st.Size()
	if from > size {
		from = 0 // rewritten / truncated
	}
	start := from
	if from == 0 && size > 1_500_000 {
		start = size - 1_500_000
	}
	if size-start > 4_000_000 {
		start = size - 4_000_000
	}
	if _, err := fh.Seek(start, io.SeekStart); err != nil {
		return "", from, err
	}
	rd := bufio.NewReaderSize(fh, 256*1024)
	pos := start
	if start != from { // landed mid-line: skip to the next one
		skip, err := rd.ReadBytes('\n')
		pos += int64(len(skip))
		if err != nil {
			return "", pos, nil
		}
	}

	var lines []string
	edited := map[string]bool{}
	for {
		b, err := rd.ReadBytes('\n')
		if err != nil {
			break // EOF; a partial last line is left for next time
		}
		pos += int64(len(b))
		var e struct {
			Type      string `json:"type"`
			IsMeta    bool   `json:"isMeta"`
			Sidechain bool   `json:"isSidechain"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(b, &e) != nil || e.IsMeta || e.Sidechain {
			continue
		}
		switch e.Type {
		case "user":
			var s string
			if json.Unmarshal(e.Message.Content, &s) == nil {
				if t := userText(s); t != "" {
					lines = append(lines, "USER: "+clip(t, 700))
				}
				continue
			}
			var blocks []map[string]any
			if json.Unmarshal(e.Message.Content, &blocks) != nil {
				continue
			}
			for _, bl := range blocks {
				switch bl["type"] {
				case "text":
					if t := userText(str(bl["text"])); t != "" {
						lines = append(lines, "USER: "+clip(t, 700))
					}
				case "tool_result":
					if isErr, _ := bl["is_error"].(bool); isErr {
						lines = append(lines, "ERROR: "+clip(resultText(bl["content"]), 220))
					}
				}
			}
		case "assistant":
			var blocks []map[string]any
			if json.Unmarshal(e.Message.Content, &blocks) != nil {
				continue
			}
			for _, bl := range blocks {
				switch bl["type"] {
				case "text":
					if t := strings.TrimSpace(str(bl["text"])); t != "" {
						lines = append(lines, "AGENT: "+clip(t, 900))
					}
				case "tool_use":
					in, _ := bl["input"].(map[string]any)
					name := str(bl["name"])
					switch name {
					case "Bash":
						lines = append(lines, "RAN: "+clip(str(in["command"]), 160))
					case "Edit", "Write", "MultiEdit", "NotebookEdit":
						p := str(in["file_path"])
						if p != "" && !edited[p] {
							edited[p] = true
							lines = append(lines, "EDITED: "+p)
						}
					default:
						if name != "" && name != "Read" && name != "Grep" && name != "Glob" && name != "TodoWrite" {
							lines = append(lines, "TOOL: "+name)
						}
					}
				}
			}
		}
	}
	d := strings.Join(lines, "\n")
	if len(d) > flDigestMax {
		d = "…\n" + d[len(d)-flDigestMax:]
	}
	return d, pos, nil
}

func str(v any) string { s, _ := v.(string); return s }

// userText strips harness wrappers; returns "" for non-prompts (slash-command
// echoes, local command output, interrupts).
func userText(s string) string {
	s = strings.TrimSpace(reSysReminder.ReplaceAllString(s, ""))
	if s == "" || strings.HasPrefix(s, "<command-") || strings.HasPrefix(s, "<local-command") ||
		strings.HasPrefix(s, "[Request interrupted") || strings.HasPrefix(s, "Caveat:") {
		return ""
	}
	return s
}

func resultText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if arr, ok := v.([]any); ok {
		var b strings.Builder
		for _, it := range arr {
			if m, ok := it.(map[string]any); ok {
				b.WriteString(str(m["text"]))
				b.WriteString(" ")
			}
		}
		return b.String()
	}
	return ""
}

// ── the model call ─────────────────────────────────────────────────────────

type logExtraction struct {
	Done      []string `json:"done"`
	Todo      []string `json:"todo"`
	Waiting   []string `json:"waiting"`
	Decisions []string `json:"decisions"`
	Lessons   []struct {
		Problem string `json:"problem"`
		Fix     string `json:"fix"`
	} `json:"lessons"`
	Rules []struct {
		Rule  string `json:"rule"`
		Scope string `json:"scope"`
	} `json:"rules"`
	Closed []string `json:"closed"`
}

const flSchema = `{"type":"object","properties":{` +
	`"done":{"type":"array","items":{"type":"string"}},` +
	`"todo":{"type":"array","items":{"type":"string"}},` +
	`"waiting":{"type":"array","items":{"type":"string"}},` +
	`"decisions":{"type":"array","items":{"type":"string"}},` +
	`"lessons":{"type":"array","items":{"type":"object","properties":{"problem":{"type":"string"},"fix":{"type":"string"}},"required":["problem","fix"]}},` +
	`"rules":{"type":"array","items":{"type":"object","properties":{"rule":{"type":"string"},"scope":{"type":"string","enum":["project","global"]}},"required":["rule","scope"]}},` +
	`"closed":{"type":"array","items":{"type":"string"}}` +
	`},"required":["done","todo","waiting","decisions","lessons","rules","closed"]}`

const flSystem = `You keep the logbook for a solo developer who runs many AI coding agents at once.
You get an excerpt of one agent session (USER = the developer, AGENT = the coding agent,
RAN / EDITED / ERROR / TOOL = the agent's actions) and the project's currently open items.
Return only what the developer will want to see later. Most excerpts produce few or no
items; empty arrays are normal and expected.

The developer reads this log at a glance across 20+ projects, so keep only headlines.
Write every item as one short plain sentence, under 80 characters, at the level of outcomes.
Never describe tool mechanics ("edited m.html", "ran tests", "rebuilt the binary"), copy
tweaks, investigation steps or intermediate results. Fold all steps of one piece of work
into a single line. When in doubt, leave it out.

done (at most 2): headline results only: a feature shipped, a reported bug fixed, a deploy,
  a release. What the developer would mention in a weekly update. Not sub-steps.
todo (at most 2): open work the developer would otherwise forget: asked for and not
  finished, or a clear follow-up the agent named. Not ideas, not nice-to-haves.
waiting (at most 2): actions only the developer can take before work continues: merge a
  PR, apply a migration, log in, add a key, answer a question, test on a device. Never
  background jobs or things the agent is still doing.
decisions (at most 1): only significant choices (architecture, product, pricing, vendor),
  written "X over Y: reason". Skip small implementation choices.
lessons (at most 1, usually none): a hard problem whose fix took real effort and would save
  time on a different project (environment, tooling, platform quirks, infra). problem = the
  symptom as it shows up (error text or behaviour); fix = what solved it. Skip anything
  specific to this codebase.
rules (at most 1): ONLY from USER lines. A standing preference the developer stated as a
  correction or as an instruction meant to outlast this task, e.g. "do not use headless,
  verify in real Chrome". Never derive rules from AGENT text or from findings. Write them as
  instructions to future agents. scope "global" when it is about how they like to work in
  general, "project" when it is specific to this codebase. Skip one-off task instructions.
closed: ids of the given open items that this excerpt clearly completed or made moot.
Do not repeat anything already in the open items.`

func claudeBin() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	for _, p := range []string{
		filepath.Join(os.Getenv("HOME"), ".local", "bin", "claude"),
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "claude"
}

func runLogExtraction(project, digest string, open []logEntry) (*logExtraction, error) {
	var b strings.Builder
	b.WriteString("Project: " + project + "\n\nOpen items:\n")
	if len(open) == 0 {
		b.WriteString("(none)\n")
	}
	for _, e := range open {
		b.WriteString("[" + e.ID + "] (" + e.Kind + ") " + e.Text + "\n")
	}
	b.WriteString("\nExcerpt:\n" + digest + "\n")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	// No user settings (so no hooks or plugins fire back at us), no tools, no
	// saved session, run from a folder our own hook handler ignores.
	cmd := exec.CommandContext(ctx, claudeBin(), "-p",
		"--model", "haiku",
		"--tools", "",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--no-session-persistence",
		"--output-format", "json",
		"--json-schema", flSchema,
		"--system-prompt", flSystem,
	)
	cmd.Dir = fleetLogWorkerDir()
	cmd.Stdin = strings.NewReader(b.String())
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var r struct {
		Structured *logExtraction `json:"structured_output"`
		Result     string         `json:"result"`
		IsError    bool           `json:"is_error"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, err
	}
	if r.Structured != nil {
		return r.Structured, nil
	}
	var x logExtraction
	if err := json.Unmarshal([]byte(r.Result), &x); err != nil {
		return nil, err
	}
	return &x, nil
}

// ── store operations ───────────────────────────────────────────────────────

func normText(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == ' ' || r > 127 {
			return r
		}
		return ' '
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// isDupLocked: the same text of the same kind already on this project in the
// last 30 days. Rules match across all statuses so a dismissed one stays gone.
// Caller holds f.mu.
func (f *fleetLogStore) isDupLocked(project, kind, text string) bool {
	n := normText(text)
	cutoff := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	for i := len(f.data.Entries) - 1; i >= 0; i-- {
		e := &f.data.Entries[i]
		if e.At < cutoff {
			break
		}
		if e.Kind == kind && (e.Project == project || kind == "rule" && e.Detail == "global") && normText(e.Text) == n {
			return true
		}
	}
	return false
}

func (f *fleetLogStore) openItems(project string, max int) []logEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []logEntry
	for i := len(f.data.Entries) - 1; i >= 0 && len(out) < max; i-- {
		e := f.data.Entries[i]
		if e.Project == project && e.Status == "open" {
			out = append(out, e)
		}
	}
	return out
}

func (f *fleetLogStore) apply(project, session, pane string, x *logExtraction) int {
	now := time.Now().UnixMilli()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range x.Closed {
		for i := range f.data.Entries {
			e := &f.data.Entries[i]
			if e.ID == id && e.Project == project && e.Status == "open" {
				e.Status, e.ClosedAt = "closed", now
			}
		}
	}
	added := 0
	add := func(kind, text, detail, status string) {
		text = clip(text, 240)
		if text == "" || f.isDupLocked(project, kind, text) {
			return
		}
		f.data.Entries = append(f.data.Entries, logEntry{
			ID: newLogID(), Project: project, Kind: kind, Text: text, Detail: clip(detail, 400),
			Status: status, Source: "auto", Session: session, Pane: pane, At: now,
		})
		added++
	}
	for _, t := range x.Done {
		add("done", t, "", "")
	}
	for _, t := range x.Todo {
		add("todo", t, "", "open")
	}
	for _, t := range x.Waiting {
		add("waiting", t, "", "open")
	}
	for _, t := range x.Decisions {
		add("decision", t, "", "")
	}
	for _, l := range x.Lessons {
		add("lesson", l.Problem, l.Fix, "")
	}
	for _, r := range x.Rules {
		scope := r.Scope
		if scope != "global" {
			scope = "project"
		}
		add("rule", r.Rule, scope, "pending")
	}
	f.trimLocked()
	return added
}

// trimLocked keeps the log bounded, dropping the oldest finished entries first.
func (f *fleetLogStore) trimLocked() {
	over := len(f.data.Entries) - flMaxEntries
	if over <= 0 {
		return
	}
	kept := f.data.Entries[:0]
	for _, e := range f.data.Entries {
		if over > 0 && e.Status != "open" && e.Status != "pending" {
			over--
			continue
		}
		kept = append(kept, e)
	}
	f.data.Entries = kept
}

// ── HTTP API ───────────────────────────────────────────────────────────────

type logProjectSummary struct {
	logProject
	Open    int     `json:"open"`
	Waiting int     `json:"waiting"`
	Done7   int     `json:"done7"`
	LastAt  int64   `json:"lastAt"`
	Spark   [14]int `json:"spark"` // entries per day, oldest first
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// GET /api/log            → overview: projects, waiting on you, rule suggestions, recent done, lessons
// GET /api/log?project=X  → that project's entries, newest first
func handleLogGet(w http.ResponseWriter, r *http.Request) {
	// Build under the lock, write after releasing it (a slow phone must not
	// hold up the hook handler).
	writeJSON(w, fleetLog.view(r.URL.Query().Get("project")))
}

func (f *fleetLogStore) view(name string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != "" {
		out := []logEntry{}
		for i := len(f.data.Entries) - 1; i >= 0; i-- {
			if f.data.Entries[i].Project == name {
				out = append(out, f.data.Entries[i])
			}
		}
		return map[string]any{"project": f.data.Projects[name], "entries": out}
	}

	now := time.Now()
	day0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -13)
	week := now.Add(-7 * 24 * time.Hour).UnixMilli()
	sums := map[string]*logProjectSummary{}
	waiting, rules, recent, lessons := []logEntry{}, []logEntry{}, []logEntry{}, []logEntry{}
	for i := len(f.data.Entries) - 1; i >= 0; i-- {
		e := f.data.Entries[i]
		s := sums[e.Project]
		if s == nil {
			s = &logProjectSummary{logProject: f.data.Projects[e.Project]}
			s.Name = e.Project
			sums[e.Project] = s
		}
		if e.At > s.LastAt {
			s.LastAt = e.At
		}
		if d := int(time.UnixMilli(e.At).Sub(day0).Hours() / 24); d >= 0 && d < 14 {
			s.Spark[d]++
		}
		switch {
		case e.Kind == "todo" && e.Status == "open":
			s.Open++
		case e.Kind == "waiting" && e.Status == "open":
			s.Waiting++
			waiting = append(waiting, e)
		case e.Kind == "rule" && e.Status == "pending":
			rules = append(rules, e)
		case e.Kind == "done":
			if e.At >= week {
				s.Done7++
			}
			if len(recent) < 40 {
				recent = append(recent, e)
			}
		case e.Kind == "lesson" && len(lessons) < 30:
			lessons = append(lessons, e)
		}
	}
	projects := make([]*logProjectSummary, 0, len(sums))
	for _, s := range sums {
		projects = append(projects, s)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].LastAt > projects[j].LastAt })
	return map[string]any{
		"projects": projects, "waiting": waiting, "rules": rules, "recent": recent, "lessons": lessons,
		"queued": len(f.pending),
	}
}

var logKinds = map[string]string{
	"todo": "todo", "done": "done", "wait": "waiting", "waiting": "waiting", "blocked": "waiting",
	"decision": "decision", "decided": "decision", "lesson": "lesson", "note": "note", "rule": "rule",
}

// POST /api/log/add {project?|cwd?, kind, text, detail?, pane?, source?}
func handleLogAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Project, CWD, Kind, Text, Detail, Pane, Source string
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	kind := logKinds[strings.ToLower(strings.TrimSpace(in.Kind))]
	text := clip(in.Text, 240)
	if kind == "" || text == "" {
		http.Error(w, "kind must be todo|done|wait|decision|lesson|note|rule and text is required", http.StatusBadRequest)
		return
	}
	var proj logProject
	if in.CWD != "" {
		proj = projectFor(in.CWD)
	} else if in.Project != "" {
		proj = logProject{Name: in.Project}
	} else {
		http.Error(w, "project or cwd required", http.StatusBadRequest)
		return
	}
	src := in.Source
	if src != "you" {
		src = "agent"
	}
	pane := in.Pane
	if strings.HasPrefix(pane, "%") {
		pane = resolveTmuxPane(pane)
	}
	status := ""
	switch kind {
	case "todo", "waiting":
		status = "open"
	case "rule":
		status = "pending"
		if in.Detail != "global" {
			in.Detail = "project"
		}
	}
	e := logEntry{ID: newLogID(), Project: proj.Name, Kind: kind, Text: text, Detail: clip(in.Detail, 400),
		Status: status, Source: src, Pane: pane, At: time.Now().UnixMilli()}
	f := fleetLog
	f.mu.Lock()
	if proj.Root != "" || f.data.Projects[proj.Name].Name == "" {
		f.data.Projects[proj.Name] = proj
	}
	f.data.Entries = append(f.data.Entries, e)
	f.trimLocked()
	f.mu.Unlock()
	f.save()
	writeJSON(w, e)
}

// POST /api/log/update {id, status?, text?}
func handleLogUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in struct{ ID, Status, Text string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f := fleetLog
	f.mu.Lock()
	var found *logEntry
	for i := range f.data.Entries {
		if f.data.Entries[i].ID == in.ID {
			found = &f.data.Entries[i]
			break
		}
	}
	if found == nil {
		f.mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if in.Status == "open" || in.Status == "closed" {
		found.Status = in.Status
		found.ClosedAt = 0
		if in.Status == "closed" {
			found.ClosedAt = time.Now().UnixMilli()
		}
	}
	if t := clip(in.Text, 240); t != "" {
		found.Text = t
	}
	e := *found
	f.mu.Unlock()
	f.save()
	writeJSON(w, e)
}

// POST /api/log/delete {id}
func handleLogDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in struct{ ID string }
	json.NewDecoder(r.Body).Decode(&in)
	f := fleetLog
	f.mu.Lock()
	kept := f.data.Entries[:0]
	for _, e := range f.data.Entries {
		if e.ID != in.ID {
			kept = append(kept, e)
		}
	}
	f.data.Entries = kept
	f.mu.Unlock()
	f.save()
	writeJSON(w, map[string]any{"ok": true})
}

// POST /api/log/rule {id, action: project|global|dismiss}
// Teach once: write the rule into the project's CLAUDE.md, or the global one
// (both Claude profiles), so no agent needs to be told again.
func handleLogRule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in struct{ ID, Action string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f := fleetLog
	f.mu.Lock()
	var e logEntry
	ok := false
	for _, x := range f.data.Entries {
		if x.ID == in.ID && x.Kind == "rule" {
			e, ok = x, true
			break
		}
	}
	root := f.data.Projects[e.Project].Root
	f.mu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var files []string
	status := "saved"
	switch in.Action {
	case "dismiss":
		status = "dismissed"
	case "project":
		if root == "" {
			http.Error(w, "this project's folder is unknown, so there is no CLAUDE.md to write to", http.StatusBadRequest)
			return
		}
		files = []string{filepath.Join(root, "CLAUDE.md")}
	case "global":
		home := os.Getenv("HOME")
		files = []string{filepath.Join(home, ".claude", "CLAUDE.md")}
		if _, err := os.Stat(filepath.Join(home, ".claude-ts", "CLAUDE.md")); err == nil {
			files = append(files, filepath.Join(home, ".claude-ts", "CLAUDE.md"))
		}
	default:
		http.Error(w, "action must be project, global or dismiss", http.StatusBadRequest)
		return
	}
	for _, p := range files {
		if err := appendRule(p, e.Text); err != nil {
			http.Error(w, "could not write "+p+": "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	f.mu.Lock()
	for i := range f.data.Entries {
		if f.data.Entries[i].ID == in.ID {
			f.data.Entries[i].Status = status
			if in.Action != "dismiss" {
				f.data.Entries[i].Detail = in.Action
			}
		}
	}
	f.mu.Unlock()
	f.save()
	writeJSON(w, map[string]any{"ok": true, "status": status, "files": files})
}

// appendRule adds "- rule" under the "## Rules (from Fleet Log)" section,
// creating the section (or the file) when missing. No-op if already present.
func appendRule(path, rule string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	mode := os.FileMode(0644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	s := string(b)
	line := "- " + rule
	if strings.Contains(s, line) {
		return nil
	}
	lines := strings.Split(s, "\n")
	head := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == flRulesHead {
			head = i
			break
		}
	}
	if head < 0 {
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		if s != "" {
			s += "\n"
		}
		s += flRulesHead + "\n\n" + line + "\n"
	} else {
		j := head + 1
		if j < len(lines) && strings.TrimSpace(lines[j]) == "" {
			j++
		}
		for j < len(lines) && strings.HasPrefix(lines[j], "- ") {
			j++
		}
		lines = append(lines[:j], append([]string{line}, lines[j:]...)...)
		s = strings.Join(lines, "\n")
	}
	return os.WriteFile(path, []byte(s), mode)
}

// POST /api/log/backfill?hours=24 — read the recent tail of sessions active in
// the last N hours that the log has never seen, so the page isn't empty on day one.
func handleLogBackfill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 || hours > 24*7 {
		hours = 24
	}
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	home := os.Getenv("HOME")
	type cand struct {
		path string
		mod  time.Time
	}
	var cands []cand
	for _, root := range []string{filepath.Join(home, ".claude", "projects"), filepath.Join(home, ".claude-ts", "projects")} {
		files, _ := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
		for _, p := range files {
			if st, err := os.Stat(p); err == nil && st.ModTime().After(cutoff) {
				cands = append(cands, cand{p, st.ModTime()})
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	if len(cands) > 30 {
		cands = cands[:30]
	}

	f := fleetLog
	queued := 0
	for _, c := range cands {
		f.mu.Lock()
		_, seen := f.data.Cursors[c.path]
		f.mu.Unlock()
		if seen {
			continue
		}
		cwd := transcriptCWD(c.path)
		if cwd == "" || strings.HasPrefix(cwd, fleetLogWorkerDir()) {
			continue
		}
		sid := strings.TrimSuffix(filepath.Base(c.path), ".jsonl")
		f.mu.Lock()
		f.pending[sid] = hookEvent{SessionID: sid, CWD: cwd, TranscriptPath: c.path, EventName: "Stop"}
		f.stopAt[sid] = time.Now().Add(-flQuiet)
		f.mu.Unlock()
		queued++
	}
	writeJSON(w, map[string]any{"queued": queued})
}

// transcriptCWD reads the working directory recorded in a transcript's first lines.
func transcriptCWD(path string) string {
	fh, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for i := 0; i < 60 && sc.Scan(); i++ {
		var e struct {
			CWD string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.CWD != "" {
			return e.CWD
		}
	}
	return ""
}
