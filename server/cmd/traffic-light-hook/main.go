// Command traffic-light-hook is what Claude Code actually invokes. It
// reads a raw hook payload on stdin, normalizes it (internal/adapters/
// claude), and POSTs the resulting envelope to the state server.
//
// It lives in the server module rather than beside the other clients
// because it shares the adapter, and duplicating vendor knowledge into a
// second module is precisely what docs/protocol.md §8 forbids. It is not
// a "client" in the sense apps/cli and the Flutter app are: those render
// state and are vendor-agnostic, while this produces events and is
// vendor-specific by nature.
//
// # Fail-open
//
// This binary runs inside Claude Code's critical path. PRD §7 and
// CLAUDE.md both make it non-negotiable that it can never block or break
// an agent. Therefore:
//
//   - it always exits 0, whatever happens;
//   - it writes nothing to stdout (Claude Code interprets hook stdout)
//     and nothing to stderr unless debugging is explicitly requested;
//   - every network call is bounded by a short timeout;
//   - it never panics: a panic would violate fail-open at the process
//     level, not merely the logical one, so main recovers.
//
// A dropped event costs a briefly wrong light. A blocked hook costs the
// user their agent. The asymmetry decides every trade-off here.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"trafficlight/internal/adapters/claude"
)

// Bounded so a wedged or missing server cannot stall a hook. Chosen to be
// comfortably longer than a loopback POST and far shorter than a human
// notices.
const (
	requestTimeout = 500 * time.Millisecond
	maxPayloadSize = 1 << 20 // 1 MiB; hook payloads carry transcripts, so cap the read
)

// debugf writes only when TRAFFIC_LIGHT_HOOK_DEBUG is set. Silence is the
// default because anything on stderr risks surfacing in the agent's UI.
func debugf(format string, args ...any) {
	if os.Getenv("TRAFFIC_LIGHT_HOOK_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "traffic-light-hook: "+format+"\n", args...)
}

func main() {
	// Fail-open at the process level: whatever happens below, including a
	// panic, this process exits 0 and stays quiet.
	defer func() {
		if r := recover(); r != nil {
			debugf("recovered from panic: %v", r)
		}
		os.Exit(0)
	}()

	var (
		hookName  = flag.String("hook", "", "hook event name (optional; the payload's hook_event_name is authoritative)")
		serverURL = flag.String("server", "http://127.0.0.1:8787", "state server base URL")
		tokenFile = flag.String("token-file", defaultTokenFile(), "file holding the bearer token")
	)
	flag.Parse()

	if err := run(*serverURL, *tokenFile); err != nil {
		// Deliberately swallowed. The hook's job is to be invisible when
		// it fails.
		debugf("hook %q: %v", *hookName, err)
	}
}

func run(serverURL, tokenFile string) error {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, maxPayloadSize))
	if err != nil {
		return fmt.Errorf("reading payload: %w", err)
	}

	ev, ok, err := claude.Normalize(raw)
	if err != nil {
		return err
	}
	if !ok {
		// An unmapped hook (PreToolUse, and anything Claude Code adds
		// later). Nothing to do, and not a failure.
		return nil
	}

	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encoding event: %w", err)
	}

	// Read the token lazily and tolerate its absence: a missing token
	// means the server has not run yet, which is not worth a diagnostic
	// in the agent's face.
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return fmt.Errorf("reading token: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+"/events", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(token)))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("posting event: %w", err)
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused, but cap it: we do not care
	// about the body's contents.
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("server returned %s for %s", resp.Status, ev.Event)
	}
	debugf("posted %s for session %s", ev.Event, ev.SessionID)
	return nil
}

func defaultTokenFile() string {
	if v := os.Getenv("TRAFFIC_LIGHT_TOKEN_FILE"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".traffic-light-token"
	}
	return filepath.Join(home, ".traffic-light", "token")
}
