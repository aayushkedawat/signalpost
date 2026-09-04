package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"trafficlight/internal/config"
	"trafficlight/internal/sessions"
	"trafficlight/internal/types"
)

const testToken = "test-token"

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	t      *testing.T
	server *httptest.Server
	clock  *clock
	seq    int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	c := &clock{now: time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)}
	cfg := config.Default()
	mgr := sessions.NewWithClock(cfg.Sessions, c.Now)
	srv := httptest.NewServer(NewWithClock(cfg, mgr, testToken, nil, c.Now).Handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, server: srv, clock: c}
}

// post sends one event the way a hook would.
func (h *harness) post(tool, session string, event types.NormalizedEventType) *http.Response {
	h.t.Helper()
	h.seq++
	body := fmt.Sprintf(
		`{"eventId":"evt_%d","source":%q,"sessionId":%q,"event":%q,"timestamp":"2026-09-03T10:00:00Z"}`,
		h.seq, tool, session, event)
	return h.postRaw(body, testToken)
}

func (h *harness) postRaw(body, token string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/events", strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *harness) mustPost(tool, session string, event types.NormalizedEventType) {
	h.t.Helper()
	resp := h.post(tool, session, event)
	if resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("POST /events %s = %d, want 202", event, resp.StatusCode)
	}
}

func (h *harness) state() types.StateResponse {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/state", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET /state = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		h.t.Fatalf("GET /state content type = %q", ct)
	}
	var out types.StateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// TestFullPipeline walks the whole flow the way the simulator will:
// event -> normalizer -> session manager -> state machine -> aggregation
// -> GET /state.
func TestFullPipeline(t *testing.T) {
	h := newHarness(t)

	if got := h.state().Overall.State; got != types.StateIdle {
		t.Fatalf("fresh server overall = %s, want idle", got)
	}

	h.mustPost("claude", "s1", types.EventSessionStarted)
	if got := h.state().Tools["claude"].State; got != types.StateIdle {
		t.Fatalf("after session_started = %s, want idle", got)
	}

	h.mustPost("claude", "s1", types.EventTaskStarted)
	if got := h.state().Overall.State; got != types.StateExecuting {
		t.Fatalf("after task_started = %s, want executing", got)
	}

	h.mustPost("claude", "s1", types.EventPermissionRequested)
	st := h.state()
	if st.Overall.State != types.StateWaiting {
		t.Fatalf("after permission_requested overall = %s, want waiting", st.Overall.State)
	}
	if st.Overall.Tool == nil || *st.Overall.Tool != "claude" {
		t.Fatalf("overall.tool = %v, want claude", st.Overall.Tool)
	}
	if st.Tools["claude"].WaitingTooLong {
		t.Fatal("flagged urgent immediately on entering WAITING")
	}

	// Past the urgency threshold the flag flips, but the state does not:
	// waitingTooLong is a display hint (protocol.md §5), and the clients
	// render it as a distinct colour rather than as a seventh state.
	// Derived from config rather than hard-coded, so tuning the threshold
	// does not break a test that is not about the threshold.
	h.clock.advance(config.Default().WaitingTooLong + time.Second)
	st = h.state()
	if !st.Tools["claude"].WaitingTooLong {
		t.Fatal("still not urgent after 31s in WAITING")
	}
	if st.Tools["claude"].State != types.StateWaiting {
		t.Fatalf("urgency changed the state to %s", st.Tools["claude"].State)
	}

	h.mustPost("claude", "s1", types.EventPermissionGranted)
	if got := h.state().Tools["claude"].State; got != types.StateExecuting {
		t.Fatalf("after permission_granted = %s, want executing", got)
	}

	h.mustPost("claude", "s1", types.EventTaskCompleted)
	if got := h.state().Tools["claude"].State; got != types.StateDone {
		t.Fatalf("after task_completed = %s, want done", got)
	}

	h.clock.advance(6 * time.Second)
	if got := h.state().Tools["claude"].State; got != types.StateIdle {
		t.Fatalf("DONE did not expire to idle, got %s", got)
	}

	h.mustPost("claude", "s1", types.EventSessionEnded)
	if len(h.state().Tools) != 0 {
		t.Fatal("session_ended while IDLE left the tool in /state")
	}
}

// TestCrossToolPipeline: two tools disagreeing produces one headline and
// no blended state.
func TestCrossToolPipeline(t *testing.T) {
	h := newHarness(t)
	for _, tool := range []string{"claude", "copilot"} {
		h.mustPost(tool, "s1", types.EventSessionStarted)
		h.mustPost(tool, "s1", types.EventTaskStarted)
	}
	h.mustPost("copilot", "s1", types.EventPermissionRequested)

	st := h.state()
	if st.Overall.State != types.StateWaiting {
		t.Fatalf("overall = %s, want waiting", st.Overall.State)
	}
	if st.Overall.Tool == nil || *st.Overall.Tool != "copilot" {
		t.Fatalf("overall.tool = %v, want copilot", st.Overall.Tool)
	}
	if st.Tools["claude"].State != types.StateExecuting {
		t.Fatalf("claude = %s, want executing", st.Tools["claude"].State)
	}
}

// TestMultipleSessionsAggregateOverHTTP: tmux panes.
func TestMultipleSessionsAggregateOverHTTP(t *testing.T) {
	h := newHarness(t)
	for _, s := range []string{"pane1", "pane2", "pane3"} {
		h.mustPost("claude", s, types.EventSessionStarted)
		h.mustPost("claude", s, types.EventTaskStarted)
	}
	h.mustPost("claude", "pane2", types.EventPermissionRequested)

	st := h.state()
	if st.Tools["claude"].ActiveSessions != 3 {
		t.Fatalf("activeSessions = %d, want 3", st.Tools["claude"].ActiveSessions)
	}
	if st.Tools["claude"].State != types.StateWaiting {
		t.Fatalf("claude = %s, want waiting", st.Tools["claude"].State)
	}
}

// TestDuplicateEventOverHTTP: a fail-open hook retry is accepted but
// changes nothing.
func TestDuplicateEventOverHTTP(t *testing.T) {
	h := newHarness(t)
	h.mustPost("claude", "s1", types.EventSessionStarted)
	h.mustPost("claude", "s1", types.EventTaskStarted)
	h.mustPost("claude", "s1", types.EventTaskCompleted)

	body := `{"eventId":"evt_retry","source":"claude","sessionId":"s1","event":"task_started","timestamp":"2026-09-03T10:00:00Z"}`
	if resp := h.postRaw(body, testToken); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first delivery = %d, want 202", resp.StatusCode)
	}
	if got := h.state().Tools["claude"].State; got != types.StateExecuting {
		t.Fatalf("state = %s, want executing", got)
	}

	// The retry must be accepted (a hook must never see an error it might
	// act on) and must not re-apply the transition.
	h.mustPost("claude", "s1", types.EventTaskCompleted)
	if resp := h.postRaw(body, testToken); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("retry = %d, want 202", resp.StatusCode)
	}
	if got := h.state().Tools["claude"].State; got != types.StateDone {
		t.Fatalf("retry re-applied the transition: state = %s, want done", got)
	}
}

// TestAuthRequiredOnEventsAndState, and not on health (protocol.md §10).
func TestAuthRequiredOnEventsAndState(t *testing.T) {
	h := newHarness(t)

	t.Run("events without a token", func(t *testing.T) {
		body := `{"eventId":"e","source":"claude","sessionId":"s","event":"task_started","timestamp":"2026-09-03T10:00:00Z"}`
		if resp := h.postRaw(body, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if len(h.state().Tools) != 0 {
			t.Fatal("an unauthenticated event mutated state")
		}
	})

	t.Run("state without a token", func(t *testing.T) {
		resp, err := h.server.Client().Get(h.server.URL + "/state")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("health without a token", func(t *testing.T) {
		resp, err := h.server.Client().Get(h.server.URL + "/health")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})
}

// TestNoResponseLeaksTheToken checks every endpoint, since the token is a
// shared secret sitting in a file that hooks and three clients all read.
func TestNoResponseLeaksTheToken(t *testing.T) {
	h := newHarness(t)
	h.mustPost("claude", "s1", types.EventSessionStarted)

	for _, path := range []string{"/state", "/health"} {
		req, err := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := h.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var buf strings.Builder
		for k, vs := range resp.Header {
			buf.WriteString(k)
			for _, v := range vs {
				buf.WriteString(v)
			}
		}
		var body [4096]byte
		n, _ := resp.Body.Read(body[:])
		resp.Body.Close()
		buf.Write(body[:n])
		if strings.Contains(buf.String(), testToken) {
			t.Fatalf("%s leaked the token", path)
		}
	}
}

// TestHealthShape matches protocol.md §10 and deliberately reports
// nothing beyond liveness.
func TestHealthShape(t *testing.T) {
	h := newHarness(t)
	h.clock.advance(90 * time.Second)

	resp, err := h.server.Client().Get(h.server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 3 {
		t.Fatalf("health has %d fields (%v), want exactly status/version/uptimeSeconds", len(raw), raw)
	}
	if raw["status"] != "ok" {
		t.Errorf("status = %v, want ok", raw["status"])
	}
	if raw["version"] != config.Version {
		t.Errorf("version = %v, want %s", raw["version"], config.Version)
	}
	if raw["uptimeSeconds"] != float64(90) {
		t.Errorf("uptimeSeconds = %v, want 90", raw["uptimeSeconds"])
	}
}

// TestMalformedEventsRejected: bad input is a 4xx, never a 5xx and never
// a crash.
func TestMalformedEventsRejected(t *testing.T) {
	h := newHarness(t)
	bodies := []string{
		``,
		`not json`,
		`{}`,
		`{"eventId":"e","source":"claude","sessionId":"s","event":"PreToolUse","timestamp":"2026-09-03T10:00:00Z"}`,
		`{"eventId":"e","source":"claude","sessionId":"s","event":"task_started","timestamp":"2026-09-03T10:00:00Z","prompt":"secret"}`,
	}
	for _, body := range bodies {
		resp := h.postRaw(body, testToken)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %q = %d, want 400", body, resp.StatusCode)
		}
	}
	if len(h.state().Tools) != 0 {
		t.Fatal("a rejected event mutated state")
	}

	// And the server is still serving.
	h.mustPost("claude", "s1", types.EventSessionStarted)
}

// TestOversizedBodyRejected keeps the hot path from reading unbounded
// input.
func TestOversizedBodyRejected(t *testing.T) {
	h := newHarness(t)
	huge := fmt.Sprintf(
		`{"eventId":%q,"source":"claude","sessionId":"s","event":"task_started","timestamp":"2026-09-03T10:00:00Z"}`,
		strings.Repeat("x", maxBodyBytes*2))
	resp := h.postRaw(huge, testToken)
	if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 413 or 400", resp.StatusCode)
	}
}

// TestMethodRouting: clients are read-only, so state is not settable.
func TestMethodRouting(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/events"},
		{http.MethodPost, "/state"},
		{http.MethodPost, "/health"},
		{http.MethodPut, "/state"},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(tc.method, h.server.URL+tc.path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := h.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// TestUnknownPath
func TestUnknownPath(t *testing.T) {
	h := newHarness(t)
	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestErrorSurvivesSessionEndOverHTTP: the case the ephemeral window
// exists for — an agent errors out and its process exits.
func TestErrorSurvivesSessionEndOverHTTP(t *testing.T) {
	h := newHarness(t)
	h.mustPost("claude", "s1", types.EventSessionStarted)
	h.mustPost("claude", "s1", types.EventTaskStarted)
	h.mustPost("claude", "s1", types.EventTaskFailed)
	h.mustPost("claude", "s1", types.EventSessionEnded)

	st := h.state()
	if st.Overall.State != types.StateError {
		t.Fatalf("overall = %s, want error to remain visible", st.Overall.State)
	}

	h.clock.advance(6 * time.Second)
	if len(h.state().Tools) != 0 {
		t.Fatal("session lingered past the ephemeral window")
	}
}

// TestConcurrentHooksAndClients approximates real use: several hooks
// posting while clients poll at ~1s.
func TestConcurrentHooksAndClients(t *testing.T) {
	h := newHarness(t)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			session := fmt.Sprintf("s%d", n)
			for j := 0; j < 25; j++ {
				body := fmt.Sprintf(
					`{"eventId":"evt_%d_%d","source":"claude","sessionId":%q,"event":"task_started","timestamp":"2026-09-03T10:00:00Z"}`,
					n, j, session)
				resp := h.postRaw(body, testToken)
				if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusBadRequest {
					t.Errorf("unexpected status %d", resp.StatusCode)
				}
			}
		}(i)
	}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				h.state()
			}
		}()
	}
	wg.Wait()

	if got := h.state().Tools["claude"].ActiveSessions; got != 4 {
		t.Fatalf("activeSessions = %d, want 4", got)
	}
}

// TestEventsResponseHasNoBody: clients must read state from GET /state,
// not infer it from the POST response.
func TestEventsResponseHasNoBody(t *testing.T) {
	h := newHarness(t)
	resp := h.post("claude", "s1", types.EventSessionStarted)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var buf [64]byte
	if n, _ := resp.Body.Read(buf[:]); n != 0 {
		t.Fatalf("POST /events returned a body: %q", buf[:n])
	}
}
