package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: claude-wall-pane <pane-target>")
		os.Exit(1)
	}

	target := os.Args[1]
	session := target
	if idx := strings.Index(target, ":"); idx >= 0 {
		session = target[:idx]
	}

	fd := int(os.Stdin.Fd())

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to set raw mode: %v\n", err)
		os.Exit(1)
	}

	cleanup := func() {
		term.Restore(fd, oldState)
		os.Stdout.Write([]byte("\033[?7h\033[?25h"))
	}
	defer cleanup()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(0)
	}()

	// Disable line wrap, hide cursor
	os.Stdout.Write([]byte("\033[?7l\033[?25l"))

	// Input goroutine
	inputCh := make(chan []byte, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				inputCh <- data
			}
			if err != nil {
				close(inputCh)
				return
			}
		}
	}()

	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	var prevFrame string

	for {
		select {
		case data, ok := <-inputCh:
			if !ok {
				return
			}
			// Forward all input as hex bytes to source pane
			hexes := make([]string, len(data))
			for i, b := range data {
				hexes[i] = fmt.Sprintf("%02x", b)
			}
			args := append([]string{"send-keys", "-t", target, "-H"}, hexes...)
			exec.Command("tmux", args...).Run()

		case <-ticker.C:
			if err := exec.Command("tmux", "has-session", "-t", session).Run(); err != nil {
				return
			}

			out, err := exec.Command("tmux", "capture-pane", "-t", target, "-e", "-p").Output()
			if err != nil {
				continue
			}

			rows := 20
			if _, h, err := term.GetSize(fd); err == nil {
				rows = h
			}

			lines := strings.Split(string(out), "\n")
			if len(lines) > rows {
				lines = lines[len(lines)-rows:]
			}

			var frame strings.Builder
			for i, line := range lines {
				frame.WriteString(line)
				frame.WriteString("\033[K")
				if i < len(lines)-1 {
					frame.WriteString("\r\n")
				}
			}
			frame.WriteString("\033[J")

			frameStr := frame.String()
			if frameStr != prevFrame {
				os.Stdout.Write([]byte("\033[H"))
				os.Stdout.Write([]byte(frameStr))
				prevFrame = frameStr
			}
		}
	}
}
