package main

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Daily usage stats — GitHub-style history built by scanning every Claude Code
// transcript under ~/.claude/projects/**/*.jsonl. Independent of the live Stop
// hook, so it covers full history including Codex-less / hook-less sessions.

// modelRates returns per-MTok pricing (input, output, cacheRead, cacheWrite)
// for a model id. Claude Code runs Opus by default, which the live-session
// estimate in finance.go under-prices with flat Sonnet rates — here we price
// each message by its actual model.
func modelRates(model string) (in, out, cacheR, cacheW float64) {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "opus"):
		return 5, 25, 0.5, 6.25
	case strings.Contains(m, "haiku"):
		return 1, 5, 0.1, 1.25
	case strings.Contains(m, "fable"), strings.Contains(m, "mythos"):
		return 10, 50, 1.0, 12.5
	case strings.Contains(m, "sonnet"):
		return 3, 15, 0.3, 3.75
	default:
		return 3, 15, 0.3, 3.75
	}
}

type usageBucket struct {
	Cost        float64 `json:"cost"`
	Input       int64   `json:"input"`
	Output      int64   `json:"output"`
	CacheRead   int64   `json:"cacheRead"`
	CacheCreate int64   `json:"cacheCreate"`
	Msgs        int     `json:"msgs"`
}

func (b *usageBucket) add(o usageBucket) {
	b.Cost += o.Cost
	b.Input += o.Input
	b.Output += o.Output
	b.CacheRead += o.CacheRead
	b.CacheCreate += o.CacheCreate
	b.Msgs += o.Msgs
}

func addTo(m map[string]usageBucket, k string, b usageBucket) {
	cur := m[k]
	cur.add(b)
	m[k] = cur
}

func addNested(m map[string]map[string]usageBucket, day, k string, b usageBucket) {
	if m[day] == nil {
		m[day] = map[string]usageBucket{}
	}
	addTo(m[day], k, b)
}

// fileStats holds one transcript's contribution, keyed to its size+mtime so an
// unchanged file is never re-read.
type fileStats struct {
	size     int64
	mtime    int64
	days     map[string]usageBucket
	models   map[string]usageBucket
	projects map[string]usageBucket
	// per-day breakdowns for the day-detail view: day -> key -> bucket
	dayModels   map[string]map[string]usageBucket
	dayProjects map[string]map[string]usageBucket
}

type dailyStore struct {
	mu    sync.Mutex
	files map[string]*fileStats
}

var dailyUsage = &dailyStore{files: map[string]*fileStats{}}

func projectsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

func dayKey(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return t.Local().Format("2006-01-02")
}

func projectName(cwd string) string {
	if cwd == "" {
		return "unknown"
	}
	return filepath.Base(cwd)
}

// parseTranscript reads one JSONL and buckets its assistant-message usage by
// day / model / project. Dedups by message uuid within the file.
func parseTranscript(path string) *fileStats {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	fsData := &fileStats{
		days:        map[string]usageBucket{},
		models:      map[string]usageBucket{},
		projects:    map[string]usageBucket{},
		dayModels:   map[string]map[string]usageBucket{},
		dayProjects: map[string]map[string]usageBucket{},
	}
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)

	for sc.Scan() {
		var e struct {
			Type      string `json:"type"`
			UUID      string `json:"uuid"`
			Timestamp string `json:"timestamp"`
			Cwd       string `json:"cwd"`
			Message   struct {
				Model string `json:"model"`
				Usage struct {
					Input       int64 `json:"input_tokens"`
					Output      int64 `json:"output_tokens"`
					CacheRead   int64 `json:"cache_read_input_tokens"`
					CacheCreate int64 `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Type != "assistant" || e.Message.Model == "" || e.Message.Model == "<synthetic>" {
			continue
		}
		u := e.Message.Usage
		if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheCreate == 0 {
			continue
		}
		if e.UUID != "" {
			if seen[e.UUID] {
				continue
			}
			seen[e.UUID] = true
		}
		day := dayKey(e.Timestamp)
		if day == "" {
			continue
		}
		inR, outR, crR, cwR := modelRates(e.Message.Model)
		cost := float64(u.Input)*inR/1e6 + float64(u.Output)*outR/1e6 +
			float64(u.CacheRead)*crR/1e6 + float64(u.CacheCreate)*cwR/1e6
		b := usageBucket{
			Cost: cost, Input: u.Input, Output: u.Output,
			CacheRead: u.CacheRead, CacheCreate: u.CacheCreate, Msgs: 1,
		}
		proj := projectName(e.Cwd)
		addTo(fsData.days, day, b)
		addTo(fsData.models, e.Message.Model, b)
		addTo(fsData.projects, proj, b)
		addNested(fsData.dayModels, day, e.Message.Model, b)
		addNested(fsData.dayProjects, day, proj, b)
	}
	return fsData
}

type namedBucket struct {
	Name string `json:"name"`
	usageBucket
}

type dayBucket struct {
	Date string `json:"date"`
	usageBucket
}

// scan refreshes the per-file cache (re-parsing only changed transcripts) and
// evicts deleted files. Caller must hold d.mu.
func (d *dailyStore) scan() {
	dir := projectsDir()
	present := map[string]bool{}
	filepath.WalkDir(dir, func(path string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, e := de.Info()
		if e != nil {
			return nil
		}
		present[path] = true
		mtime := info.ModTime().UnixNano()
		if cur := d.files[path]; cur != nil && cur.size == info.Size() && cur.mtime == mtime {
			return nil // unchanged — reuse cached contribution
		}
		fsData := parseTranscript(path)
		if fsData == nil {
			return nil
		}
		fsData.size = info.Size()
		fsData.mtime = mtime
		d.files[path] = fsData
		return nil
	})
	for p := range d.files {
		if !present[p] {
			delete(d.files, p)
		}
	}
}

func (d *dailyStore) compute() map[string]interface{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scan()

	days := map[string]usageBucket{}
	models := map[string]usageBucket{}
	projects := map[string]usageBucket{}
	for _, fsData := range d.files {
		for k, v := range fsData.days {
			addTo(days, k, v)
		}
		for k, v := range fsData.models {
			addTo(models, k, v)
		}
		for k, v := range fsData.projects {
			addTo(projects, k, v)
		}
	}

	dayList := make([]dayBucket, 0, len(days))
	for k, v := range days {
		dayList = append(dayList, dayBucket{Date: k, usageBucket: v})
	}
	sort.Slice(dayList, func(i, j int) bool { return dayList[i].Date < dayList[j].Date })

	modelList := sortedBuckets(models)
	projectList := sortedBuckets(projects)

	now := time.Now()
	todayKey := now.Format("2006-01-02")
	monthPrefix := now.Format("2006-01")
	cut30 := now.AddDate(0, 0, -29).Format("2006-01-02")

	var totals usageBucket
	var todayCost, cost30, monthCost float64
	for _, dl := range dayList {
		totals.add(dl.usageBucket)
		if dl.Date == todayKey {
			todayCost = dl.Cost
		}
		if dl.Date >= cut30 {
			cost30 += dl.Cost
		}
		if strings.HasPrefix(dl.Date, monthPrefix) {
			monthCost += dl.Cost
		}
	}

	return map[string]interface{}{
		"days":       dayList,
		"byModel":    modelList,
		"byProject":  projectList,
		"totals":     totals,
		"todayCost":  todayCost,
		"cost30d":    cost30,
		"monthCost":  monthCost,
		"today":      todayKey,
		"generated":  now.Format(time.RFC3339),
		"numDays":    len(dayList),
	}
}

func sortedBuckets(m map[string]usageBucket) []namedBucket {
	out := make([]namedBucket, 0, len(m))
	for k, v := range m {
		out = append(out, namedBucket{Name: k, usageBucket: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cost > out[j].Cost })
	return out
}

func handleFinanceDaily(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dailyUsage.compute())
}

// computeDay aggregates one date's model/project breakdown across all cached
// transcripts.
func (d *dailyStore) computeDay(date string) map[string]interface{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scan()

	var total usageBucket
	models := map[string]usageBucket{}
	projects := map[string]usageBucket{}
	for _, f := range d.files {
		if b, ok := f.days[date]; ok {
			total.add(b)
		}
		for k, v := range f.dayModels[date] {
			addTo(models, k, v)
		}
		for k, v := range f.dayProjects[date] {
			addTo(projects, k, v)
		}
	}
	return map[string]interface{}{
		"date":      date,
		"total":     total,
		"byModel":   sortedBuckets(models),
		"byProject": sortedBuckets(projects),
	}
}

func handleFinanceDay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	date := r.URL.Query().Get("date")
	if len(date) != 10 {
		http.Error(w, "bad date", 400)
		return
	}
	json.NewEncoder(w).Encode(dailyUsage.computeDay(date))
}
