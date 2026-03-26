package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func pidFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude-wall.pid")
}

func logFile() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".claude")
	os.MkdirAll(dir, 0755)
	return filepath.Join(dir, "claude-wall.log")
}

func daemonStart(port int, public bool) {
	// Check if already running
	if pid := readPid(); pid > 0 {
		if processAlive(pid) {
			fmt.Printf("▸ Already running (PID %d), restarting...\n", pid)
			daemonStop()
		}
		// Stale PID file
		os.Remove(pidFile())
	}

	// Build args for the background process
	args := []string{"--serve"}
	if port != 7685 {
		args = append(args, fmt.Sprintf("%d", port))
	}
	if public {
		args = append(args, "--public")
	}

	exe, err := os.Executable()
	if err != nil {
		fatal("cannot find executable: %v", err)
	}

	log, err := os.OpenFile(logFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fatal("cannot open log file: %v", err)
	}

	cmd := exec.Command(exe, args...)
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		fatal("cannot start daemon: %v", err)
	}

	// Write PID
	os.WriteFile(pidFile(), []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0644)

	fmt.Printf("▸ Started (PID %d)\n", cmd.Process.Pid)
	fmt.Printf("  Dashboard at http://%s:%d\n", bindHost(public), port)
	fmt.Printf("  Logs: %s\n", logFile())
}

func daemonStop() {
	pid := readPid()
	if pid <= 0 {
		fmt.Println("▸ Not running")
		return
	}

	if !processAlive(pid) {
		os.Remove(pidFile())
		fmt.Println("▸ Not running (cleaned stale PID)")
		return
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		fatal("cannot find process: %v", err)
	}

	proc.Signal(syscall.SIGTERM)
	fmt.Printf("▸ Stopped (PID %d)\n", pid)
	os.Remove(pidFile())
}

func daemonRestart(port int, public bool) {
	daemonStop()
	daemonStart(port, public)
}

func daemonStatus() {
	pid := readPid()
	if pid <= 0 || !processAlive(pid) {
		fmt.Println("▸ Not running")
		os.Remove(pidFile())
		return
	}
	fmt.Printf("▸ Running (PID %d)\n", pid)
	fmt.Printf("  Logs: %s\n", logFile())
}

func readPid() int {
	data, err := os.ReadFile(pidFile())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func bindHost(public bool) string {
	if public {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}
