package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"trafficlight/internal/types"
)

func writeToken(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func withStdin(t *testing.T, payload string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig })

	go func() {
		w.WriteString(payload)
		w.Close()
	}()
	fn()
	r.Close()
}

// TestPostsNormalizedEnvelope is the happy path: a real hook payload in,
// exactly the five-field envelope out, with the bearer token attached.
func TestPostsNormalizedEnvelope(t *testing.T) {
	var (
		mu     sync.Mutex
		gotEv  types.NormalizedEvent
		gotTok string
		gotCT  string
		hits   int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hits++
		gotTok = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&gotEv)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	tok := writeToken(t, "secret-token")
	withStdin(t, `{"hook_event_name":"Stop","session_id":"s-42","prompt":"SENSITIVE"}`, func() {
		if err := run(srv.URL, tok); err != nil {
			t.Fatalf("run: %v", err)
		}
	})

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("server got %d requests, want 1", hits)
	}
	if gotEv.Event != types.EventTaskCompleted {
		t.Errorf("event = %s, want task_completed", gotEv.Event)
	}
	if gotEv.SessionID != "s-42" {
		t.Errorf("sessionId = %q", gotEv.SessionID)
	}
	if gotEv.Source != "claude" {
		t.Errorf("source = %q", gotEv.Source)
	}
	// The trailing newline in the token file must not reach the header.
	if gotTok != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want %q", gotTok, "Bearer secret-token")
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
}

// TestIgnoredHookPostsNothing: PreToolUse must not generate traffic.
func TestIgnoredHookPostsNothing(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	tok := writeToken(t, "t")
	withStdin(t, `{"hook_event_name":"PreToolUse","session_id":"s-1","tool_name":"Bash"}`, func() {
		if err := run(srv.URL, tok); err != nil {
			t.Errorf("an ignored hook should not error: %v", err)
		}
	})
	if hits != 0 {
		t.Errorf("ignored hook produced %d requests, want 0", hits)
	}
}

// TestFailOpen is the test that matters most. Every one of these is a
// real failure mode, and in every case the hook must return rather than
// hang, crash, or write to stdout — because Claude Code is waiting on it.
func TestFailOpen(t *testing.T) {
	// A handler that never responds. It waits on an explicit release
	// channel as well as the request context: a cancelled client request
	// does not reliably cancel the server-side context here, and waiting
	// on the context alone leaves the handler parked so httptest's Close
	// blocks forever.
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() { close(release); slow.Close() }()

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()

	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer unauthorized.Close()

	goodToken := writeToken(t, "t")

	cases := []struct {
		name    string
		server  string
		token   string
		payload string
	}{
		{"server not running", "http://127.0.0.1:1", goodToken, `{"hook_event_name":"Stop","session_id":"s"}`},
		{"server hangs", slow.URL, goodToken, `{"hook_event_name":"Stop","session_id":"s"}`},
		{"server errors", broken.URL, goodToken, `{"hook_event_name":"Stop","session_id":"s"}`},
		{"bad token", unauthorized.URL, goodToken, `{"hook_event_name":"Stop","session_id":"s"}`},
		{"token file missing", broken.URL, "/nonexistent/token", `{"hook_event_name":"Stop","session_id":"s"}`},
		{"malformed payload", broken.URL, goodToken, `not json at all`},
		{"empty payload", broken.URL, goodToken, ``},
		{"null payload", broken.URL, goodToken, `null`},
		{"no session id", broken.URL, goodToken, `{"hook_event_name":"Stop"}`},
		{"unknown hook", broken.URL, goodToken, `{"hook_event_name":"Whatever","session_id":"s"}`},
		{"garbage url", "://not-a-url", goodToken, `{"hook_event_name":"Stop","session_id":"s"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("panicked, which would break fail-open at the process level: %v", r)
					}
				}()
				withStdin(t, tc.payload, func() {
					// The error is expected and irrelevant; what matters
					// is that it returns at all.
					_ = run(tc.server, tc.token)
				})
			}()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("hook did not return — it would have blocked the agent")
			}
		})
	}
}

// TestTimeoutIsActuallyBounded pins the fail-open budget: a hung server
// must not hold the hook longer than requestTimeout allows.
func TestTimeoutIsActuallyBounded(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() { close(release); slow.Close() }()

	tok := writeToken(t, "t")
	start := time.Now()
	withStdin(t, `{"hook_event_name":"Stop","session_id":"s"}`, func() {
		_ = run(slow.URL, tok)
	})
	elapsed := time.Since(start)

	// Generous ceiling to stay reliable on a loaded CI box, while still
	// failing loudly if the timeout stops working.
	if elapsed > 3*time.Second {
		t.Errorf("took %v against a hung server; the timeout is not bounding it", elapsed)
	}
}

// TestNeverWritesToStdout: Claude Code parses hook stdout, so anything
// written there could alter agent behavior.
func TestNeverWritesToStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	tok := writeToken(t, "t")
	withStdin(t, `{"hook_event_name":"Stop","session_id":"s"}`, func() {
		_ = run("http://127.0.0.1:1", tok) // guaranteed to fail
	})

	w.Close()
	os.Stdout = orig
	var buf [512]byte
	n, _ := r.Read(buf[:])
	if n > 0 {
		t.Errorf("wrote %d bytes to stdout, which Claude Code would interpret: %q", n, buf[:n])
	}
}
