package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeHome makes an empty HOME with no work account and no projects override.
func fakeHome(t *testing.T) string {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_WALL_PROJECTS_DIRS", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_WALL_WORK_CONFIG_DIR", "off")
	workDirCache.Lock()
	workDirCache.at = workDirCache.at.AddDate(-1, 0, 0) // drop any cached lookup
	workDirCache.Unlock()
	return home
}

func TestListProjectsFromClaudeHistory(t *testing.T) {
	home := fakeHome(t)
	at := func(rel string) string { return filepath.Join(home, rel) }
	os.MkdirAll(at("work/app/.git/worktrees/fix"), 0755)
	os.MkdirAll(at("work/app/src"), 0755)
	os.MkdirAll(at("work/fresh-clone/.git"), 0755) // never opened with Claude, beside app
	os.MkdirAll(at("work/app-fix"), 0755)
	os.WriteFile(at("work/app-fix/.git"), []byte("gitdir: "+at("work/app/.git/worktrees/fix")+"\n"), 0644)
	os.MkdirAll(at("work/app-rel"), 0755)
	os.WriteFile(at("work/app-rel/.git"), []byte("gitdir: ../app/.git/worktrees/rel\n"), 0644) // --relative-paths
	os.MkdirAll(at("work/orphan"), 0755)
	os.WriteFile(at("work/orphan/.git"), []byte("gitdir: "+at("gone/.git/worktrees/x")+"\n"), 0644)
	os.MkdirAll(at("clients/acme/ios/.git"), 0755)
	os.MkdirAll(at("clients/other/ios/.git"), 0755)
	os.MkdirAll(at("notes"), 0755)
	os.MkdirAll(at("dotfiles/.git"), 0755) // beside nothing recorded: not offered
	os.WriteFile(at(".claude.json"), []byte(`{"projects": {
		"`+at("work/app/src")+`": {},
		"`+at("work/app-fix")+`": {},
		"`+at("work/app-rel")+`": {},
		"`+at("work/orphan")+`": {},
		"`+at("clients/acme/ios")+`": {},
		"`+at("clients/other/ios")+`": {},
		"`+at("notes")+`": {},
		"`+home+`": {},
		"/usr/share": {},
		"/gone/elsewhere": {}
	}}`), 0644)

	got := listProjects()
	want := []projectEntry{
		{Name: "acme/ios", Path: at("clients/acme/ios")},
		{Name: "app", Path: at("work/app")},
		{Name: "fresh-clone", Path: at("work/fresh-clone")},
		{Name: "other/ios", Path: at("clients/other/ios")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestListProjectsFromProjectsDirs(t *testing.T) {
	fakeHome(t)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "app", ".git"), 0755)
	os.MkdirAll(filepath.Join(root, "group", "nested", ".git"), 0755)
	os.MkdirAll(filepath.Join(root, "app-worktree"), 0755)
	os.WriteFile(filepath.Join(root, "app-worktree", ".git"), []byte("gitdir: x"), 0644)

	t.Setenv("CLAUDE_WALL_PROJECTS_DIRS", root+":"+root)
	got := listProjects()
	want := []projectEntry{{Name: "app", Path: filepath.Join(root, "app")}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestSourceRepo(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "macapp"), 0755)
	os.WriteFile(filepath.Join(repo, refreshScript), []byte("#!/bin/sh\n"), 0755)

	// Not a git checkout (like the module cache): skipped.
	t.Setenv("CLAUDE_WALL_REPO", repo)
	wd, _ := os.Getwd()
	if got := sourceRepo(); got != wd {
		t.Fatalf("without .git: got %q, want the build checkout %q", got, wd)
	}

	os.Mkdir(filepath.Join(repo, ".git"), 0755)
	if got := sourceRepo(); got != repo {
		t.Fatalf("with CLAUDE_WALL_REPO: got %q, want %q", got, repo)
	}
}

func TestShellQuote(t *testing.T) {
	got := shellQuote(`/a b/$(x)/it's`)
	if want := `'/a b/$(x)/it'\''s'`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestWorkConfigDir(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("CLAUDE_WALL_WORK_CONFIG_DIR", "")
	write := func(dir, body string) {
		os.MkdirAll(filepath.Join(home, dir), 0755)
		os.WriteFile(filepath.Join(home, dir, ".claude.json"), []byte(body), 0644)
	}
	write(".claude-empty", `{"projects": {}}`)
	write(".claude-acme", `{"oauthAccount": {"emailAddress": "me@acme.test"}}`)
	if got, want := findWorkConfigDir(), filepath.Join(home, ".claude-acme"); got != want {
		t.Fatalf("detected: got %q, want %q", got, want)
	}
	if !isWorkConfigDir(filepath.Join(home, ".claude-acme") + "/") {
		t.Fatal("trailing slash should still match the work dir")
	}
	if cmd := workClaudeCommand("--continue"); !strings.HasPrefix(cmd, "CLAUDE_CONFIG_DIR='"+home) || !strings.HasSuffix(cmd, "' claude --continue") {
		t.Fatalf("command: %s", cmd)
	}

	// Several signed in: the first by name, however recently each was used.
	write(".claude-zeta", `{"oauthAccount": {"emailAddress": "me@zeta.test"}}`)
	if got, want := findWorkConfigDir(), filepath.Join(home, ".claude-acme"); got != want {
		t.Fatalf("several: got %q, want %q", got, want)
	}

	// A projects/ symlinked to the personal one is not the work account's own.
	os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0755)
	os.Symlink(filepath.Join(home, ".claude", "projects"), filepath.Join(home, ".claude-acme", "projects"))
	os.WriteFile(filepath.Join(home, ".claude-acme", "CLAUDE.md"), []byte("# own\n"), 0644)
	t.Setenv("CLAUDE_WALL_WORK_CONFIG_DIR", "~/.claude-acme")
	workDirCache.Lock()
	workDirCache.at = workDirCache.at.AddDate(-1, 0, 0)
	workDirCache.Unlock()
	if got := workOwnPath("projects"); got != "" {
		t.Fatalf("shared projects: got %q", got)
	}
	if got, want := workOwnPath("CLAUDE.md"), filepath.Join(home, ".claude-acme", "CLAUDE.md"); got != want {
		t.Fatalf("own CLAUDE.md: got %q, want %q", got, want)
	}

	t.Setenv("CLAUDE_WALL_WORK_CONFIG_DIR", "~/.claude-empty")
	if got, want := findWorkConfigDir(), filepath.Join(home, ".claude-empty"); got != want {
		t.Fatalf("override: got %q, want %q", got, want)
	}
	t.Setenv("CLAUDE_WALL_WORK_CONFIG_DIR", "off")
	if got := findWorkConfigDir(); got != "" {
		t.Fatalf("off: got %q", got)
	}
}
