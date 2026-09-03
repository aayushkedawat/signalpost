// Command simulate POSTs synthetic normalized events to the Traffic Light
// server, so the whole pipeline — event -> state machine -> aggregation ->
// clients — can be exercised without a live Claude Code or Copilot
// session (PRD.md §13, "fake event simulator").
//
// This is dev/test tooling, not a shipped feature. It posts already-
// normalized events and so deliberately bypasses the vendor adapters
// that phases 2 and 3 will add.
//
//	simulate task_started --tool claude --session s1
//	simulate --scenario permission --tool claude --session s1
//	simulate --list
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// events is protocol.md §1. The simulator validates locally as well so a
// typo is a clear message rather than a 400 from the server.
var events = []string{
	"session_started",
	"task_started",
	"permission_requested",
	"permission_granted",
	"input_received",
	"task_completed",
	"task_failed",
	"session_ended",
}

// scenarios are the sequences worth replaying by hand while watching the
// CLI. Each is just a list of §1 event names.
var scenarios = map[string][]string{
	"normal":     {"session_started", "task_started", "task_completed"},
	"permission": {"session_started", "task_started", "permission_requested", "permission_granted", "task_completed"},
	"input":      {"session_started", "task_started", "permission_requested", "input_received", "task_completed"},
	"error":      {"session_started", "task_started", "task_failed"},
	"crash":      {"session_started", "task_started", "task_failed", "session_ended"},
	"abandoned":  {"session_started", "task_started", "permission_requested", "session_ended"},
	// Deliberately inconsistent, to watch a session land in UNKNOWN.
	"inconsistent": {"session_started", "task_completed"},
	"full":         {"session_started", "task_started", "permission_requested", "permission_granted", "task_completed", "session_ended"},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "simulate:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("simulate", flag.ContinueOnError)
	server := fs.String("server", "http://127.0.0.1:8787", "traffic light server base URL")
	tool := fs.String("tool", "claude", "value for the event's `source` field")
	session := fs.String("session", "sim1", "value for the event's `sessionId` field")
	tokenFile := fs.String("token-file", defaultTokenPath(), "bearer token file")
	token := fs.String("token", "", "bearer token; overrides --token-file")
	scenario := fs.String("scenario", "", "replay a named event sequence instead of a single event")
	delay := fs.Duration("delay", 800*time.Millisecond, "pause between events in a scenario")
	list := fs.Bool("list", false, "list event names and scenarios, then exit")
	timeout := fs.Duration("timeout", 2*time.Second, "per-request HTTP timeout")
	fs.Usage = usage(fs)

	// Accept both "simulate <event>" and the "traffic-light simulate
	// <event>" form used in the docs.
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "simulate" {
		args = args[1:]
	}
	// Allow the event name before or after the flags.
	var positional []string
	var flags []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || len(flags) > 0 {
			flags = append(flags, a)
		} else {
			positional = append(positional, a)
		}
	}
	if err := fs.Parse(flags); err != nil {
		return err
	}
	positional = append(positional, fs.Args()...)

	if *list {
		printCatalog()
		return nil
	}

	var sequence []string
	switch {
	case *scenario != "" && len(positional) > 0:
		return errors.New("give either an event name or --scenario, not both")
	case *scenario != "":
		seq, ok := scenarios[*scenario]
		if !ok {
			return fmt.Errorf("unknown scenario %q (try --list)", *scenario)
		}
		sequence = seq
	case len(positional) == 1:
		if !valid(positional[0]) {
			return fmt.Errorf("unknown event %q (try --list)", positional[0])
		}
		sequence = positional
	case len(positional) == 0:
		fs.Usage()
		return errors.New("no event or scenario given")
	default:
		return fmt.Errorf("expected one event name, got %d", len(positional))
	}

	bearer, err := resolveToken(*token, *tokenFile)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: *timeout}
	for i, event := range sequence {
		if i > 0 && *delay > 0 {
			time.Sleep(*delay)
		}
		if err := send(client, *server, bearer, *tool, *session, event); err != nil {
			return err
		}
		fmt.Printf("→ %-22s tool=%s session=%s\n", event, *tool, *session)
	}
	return nil
}

func send(client *http.Client, server, token, tool, session, event string) error {
	body := fmt.Sprintf(
		`{"eventId":%q,"source":%q,"sessionId":%q,"event":%q,"timestamp":%q}`,
		newEventID(), tool, session, event, time.Now().UTC().Format(time.RFC3339))

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(server, "/")+"/events", bytes.NewReader([]byte(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w (is the server running?)", event, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		var detail bytes.Buffer
		_, _ = detail.ReadFrom(resp.Body)
		return fmt.Errorf("post %s: server returned %s: %s",
			event, resp.Status, strings.TrimSpace(detail.String()))
	}
	return nil
}

// newEventID mimics a hook's UUID/ULID. It is dedup identity only, so
// any unique value will do.
func newEventID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("evt_%d", time.Now().UnixNano())
	}
	return "evt_" + hex.EncodeToString(raw)
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

func valid(event string) bool {
	for _, e := range events {
		if e == event {
			return true
		}
	}
	return false
}

func printCatalog() {
	fmt.Println("events:")
	for _, e := range events {
		fmt.Printf("  %s\n", e)
	}
	fmt.Println("\nscenarios:")
	for _, name := range []string{"normal", "permission", "input", "error", "crash", "abandoned", "inconsistent", "full"} {
		fmt.Printf("  %-13s %s\n", name, strings.Join(scenarios[name], " → "))
	}
}

func usage(fs *flag.FlagSet) func() {
	return func() {
		fmt.Fprintf(fs.Output(), `simulate — POST synthetic normalized events to the traffic light server

usage:
  simulate <event> [flags]
  simulate --scenario <name> [flags]
  simulate --list

flags:
`)
		fs.PrintDefaults()
	}
}
