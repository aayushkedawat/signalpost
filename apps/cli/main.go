// Command traffic-light-cli polls GET /state and prints the current
// state per tool.
//
// This is the phase-1 terminal output: intentionally basic, just enough
// to watch state changes happen live while testing the pipeline. The
// polished tmux TUI panel described in PRD.md §10 is a later phase, and
// so is the Dart implementation that will eventually share
// traffic_light_core with the desktop app.
//
// It is pure display, and — per PRD.md §10 — always labels state in text
// rather than by colour alone, since colour is unreliable across terminal
// themes and unavailable to colour-blind users.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// state mirrors GET /state (protocol.md §10). The client understands
// only this shape — never vendor hook semantics, never the state machine.
type state struct {
	Version   int    `json:"version"`
	UpdatedAt string `json:"updatedAt"`
	Overall   struct {
		State string  `json:"state"`
		Tool  *string `json:"tool"`
	} `json:"overall"`
	Tools map[string]struct {
		State          string `json:"state"`
		Since          string `json:"since"`
		ActiveSessions int    `json:"activeSessions"`
		WaitingTooLong bool   `json:"waitingTooLong"`
	} `json:"tools"`
}

// The traffic-light metaphor from PRD.md §2. The label is what carries
// the meaning; the colour is decoration on top of it.
var display = map[string]struct {
	label string
	color string
}{
	"waiting":   {"WAITING  needs you", "\033[31m"},
	"error":     {"ERROR    failed", "\033[35m"},
	"unknown":   {"UNKNOWN  not sure", "\033[36m"},
	"executing": {"EXECUTING working", "\033[33m"},
	"done":      {"DONE     finished", "\033[32m"},
	"idle":      {"IDLE     nothing running", "\033[90m"},
}

const reset = "\033[0m"

func main() {
	server := flag.String("server", "http://127.0.0.1:8787", "traffic light server base URL")
	tokenFile := flag.String("token-file", defaultTokenPath(), "bearer token file")
	token := flag.String("token", "", "bearer token; overrides --token-file")
	interval := flag.Duration("interval", time.Second, "poll interval")
	once := flag.Bool("once", false, "print the current state once and exit")
	all := flag.Bool("all", false, "print every poll rather than only changes")
	noColor := flag.Bool("no-color", false, "disable ANSI colour")
	flag.Parse()

	bearer, err := resolveToken(*token, *tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "traffic-light-cli:", err)
		os.Exit(1)
	}

	colorize := !*noColor && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	client := &http.Client{Timeout: 2 * time.Second}
	base := strings.TrimRight(*server, "/")

	if *once {
		st, err := fetch(client, base, bearer)
		if err != nil {
			fmt.Fprintln(os.Stderr, "traffic-light-cli:", err)
			os.Exit(1)
		}
		fmt.Print(stamped(render(st, colorize)))
		return
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	var last string
	var lastErr string
	poll := func() {
		st, err := fetch(client, base, bearer)
		if err != nil {
			// The server being unreachable is a normal condition, not a
			// crash: report it once and keep polling.
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				last = ""
				fmt.Printf("%s  unreachable: %s\n", time.Now().Format("15:04:05"), msg)
			}
			return
		}
		lastErr = ""
		out := render(st, colorize)
		if out == last && !*all {
			return
		}
		last = out
		fmt.Print(stamped(out))
	}

	poll()
	for {
		select {
		case <-ticker.C:
			poll()
		case <-sig:
			return
		}
	}
}

// render returns the headline plus one line per tool, deliberately
// WITHOUT a timestamp: the caller stamps the output, so that comparing
// two renders detects a real state change rather than the clock moving.
// Continuation lines are indented to line up under the stamp the caller
// prepends.
func render(st state, colorize bool) string {
	var b strings.Builder

	headline := display[st.Overall.State].label
	if headline == "" {
		headline = strings.ToUpper(st.Overall.State)
	}
	if st.Overall.Tool != nil {
		headline = fmt.Sprintf("%s (%s)", headline, *st.Overall.Tool)
	}
	fmt.Fprintf(&b, "%s\n", paint(st.Overall.State, headline, colorize))

	names := make([]string, 0, len(st.Tools))
	for name := range st.Tools {
		names = append(names, name)
	}
	sort.Strings(names)

	if len(names) == 0 {
		fmt.Fprintf(&b, "          no active sessions\n")
		return b.String()
	}

	for _, name := range names {
		tool := st.Tools[name]
		label := display[tool.State].label
		if label == "" {
			label = strings.ToUpper(tool.State)
		}
		line := fmt.Sprintf("%-9s %s", name, label)
		if tool.WaitingTooLong {
			line += "  (waiting a while)"
		}
		fmt.Fprintf(&b, "          %s  %s\n",
			paint(tool.State, line, colorize), sessionCount(tool.ActiveSessions))
	}
	return b.String()
}

// stamped prefixes a rendered block with the current wall-clock time.
// The indent in render() lines the remaining rows up underneath it.
func stamped(block string) string {
	return time.Now().Format("15:04:05") + "  " + block
}

func sessionCount(n int) string {
	if n == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", n)
}

func paint(state, text string, colorize bool) string {
	if !colorize {
		return text
	}
	color := display[state].color
	if color == "" {
		return text
	}
	return color + text + reset
}

func fetch(client *http.Client, base, token string) (state, error) {
	var st state

	req, err := http.NewRequest(http.MethodGet, base+"/state", nil)
	if err != nil {
		return st, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return st, fmt.Errorf("unauthorized — token does not match the server's")
	}
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("server returned %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return st, fmt.Errorf("decode state: %w", err)
	}
	return st, nil
}

func resolveToken(token, path string) (string, error) {
	if token != "" {
		return token, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w (start the server once to generate it, or pass --token)", path, err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return trimmed, nil
}

func defaultTokenPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".traffic-light", "token")
	}
	return filepath.Join(home, ".traffic-light", "token")
}
