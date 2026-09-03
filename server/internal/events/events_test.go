package events

import (
	"strings"
	"testing"

	"trafficlight/internal/types"
)

const validBody = `{
  "eventId": "evt_9f2a1c",
  "source": "claude",
  "sessionId": "abc123",
  "event": "permission_requested",
  "timestamp": "2026-09-03T10:42:31Z"
}`

// TestParseAcceptsTheDocumentedEnvelope uses the exact example from
// protocol.md §4.
func TestParseAcceptsTheDocumentedEnvelope(t *testing.T) {
	ev, err := Parse(strings.NewReader(validBody))
	if err != nil {
		t.Fatalf("documented envelope rejected: %v", err)
	}
	want := types.NormalizedEvent{
		EventID:   "evt_9f2a1c",
		Source:    "claude",
		SessionID: "abc123",
		Event:     types.EventPermissionRequested,
		Timestamp: "2026-09-03T10:42:31Z",
	}
	if ev != want {
		t.Fatalf("got %+v, want %+v", ev, want)
	}
}

// TestAllNormalizedEventNamesAccepted covers protocol.md §1.
func TestAllNormalizedEventNamesAccepted(t *testing.T) {
	names := []string{
		"session_started", "task_started", "permission_requested",
		"permission_granted", "input_received", "task_completed",
		"task_failed", "session_ended",
	}
	if len(names) != len(known) {
		t.Fatalf("known set has %d entries, want %d", len(known), len(names))
	}
	for _, name := range names {
		body := `{"eventId":"e","source":"claude","sessionId":"s","event":"` + name + `","timestamp":"2026-09-03T10:42:31Z"}`
		if _, err := Parse(strings.NewReader(body)); err != nil {
			t.Errorf("event %q rejected: %v", name, err)
		}
	}
}

// TestUnknownFieldsAreRejected is the enforcement point for PRD.md §9:
// content must not be able to cross the hook -> server boundary, and
// rejecting is required rather than silently stripping.
func TestUnknownFieldsAreRejected(t *testing.T) {
	leaky := []struct {
		name string
		body string
	}{
		{"prompt", `{"eventId":"e","source":"claude","sessionId":"s","event":"task_started","timestamp":"2026-09-03T10:42:31Z","prompt":"my secret code"}`},
		{"tool output", `{"eventId":"e","source":"claude","sessionId":"s","event":"task_started","timestamp":"2026-09-03T10:42:31Z","toolOutput":"..."}`},
		{"file contents", `{"eventId":"e","source":"claude","sessionId":"s","event":"task_started","timestamp":"2026-09-03T10:42:31Z","file":"/etc/passwd"}`},
		{"nested object", `{"eventId":"e","source":"claude","sessionId":"s","event":"task_started","timestamp":"2026-09-03T10:42:31Z","meta":{"cwd":"/home/me"}}`},
		{"state injection", `{"eventId":"e","source":"claude","sessionId":"s","event":"task_started","timestamp":"2026-09-03T10:42:31Z","state":"idle"}`},
	}
	for _, tc := range leaky {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(tc.body)); err == nil {
				t.Fatal("unexpected field accepted; content must never cross the boundary")
			}
		})
	}
}

// TestTrailingContentRejected: a second object in the body must not slip
// past field validation.
func TestTrailingContentRejected(t *testing.T) {
	body := `{"eventId":"e","source":"claude","sessionId":"s","event":"task_started","timestamp":"2026-09-03T10:42:31Z"}{"prompt":"secret"}`
	if _, err := Parse(strings.NewReader(body)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestValidationRejectsBadEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		ev   types.NormalizedEvent
	}{
		{"missing eventId", types.NormalizedEvent{Source: "claude", SessionID: "s", Event: types.EventTaskStarted, Timestamp: "2026-09-03T10:42:31Z"}},
		{"missing source", types.NormalizedEvent{EventID: "e", SessionID: "s", Event: types.EventTaskStarted, Timestamp: "2026-09-03T10:42:31Z"}},
		{"missing sessionId", types.NormalizedEvent{EventID: "e", Source: "claude", Event: types.EventTaskStarted, Timestamp: "2026-09-03T10:42:31Z"}},
		{"missing event", types.NormalizedEvent{EventID: "e", Source: "claude", SessionID: "s", Timestamp: "2026-09-03T10:42:31Z"}},
		{"missing timestamp", types.NormalizedEvent{EventID: "e", Source: "claude", SessionID: "s", Event: types.EventTaskStarted}},
		{"unknown event name", types.NormalizedEvent{EventID: "e", Source: "claude", SessionID: "s", Event: "PreToolUse", Timestamp: "2026-09-03T10:42:31Z"}},
		{"vendor hook name", types.NormalizedEvent{EventID: "e", Source: "claude", SessionID: "s", Event: "SessionStart", Timestamp: "2026-09-03T10:42:31Z"}},
		{"state as event", types.NormalizedEvent{EventID: "e", Source: "claude", SessionID: "s", Event: "waiting", Timestamp: "2026-09-03T10:42:31Z"}},
		{"non-RFC3339 timestamp", types.NormalizedEvent{EventID: "e", Source: "claude", SessionID: "s", Event: types.EventTaskStarted, Timestamp: "3 Sep 2026"}},
		{"oversized sessionId", types.NormalizedEvent{EventID: "e", Source: "claude", SessionID: strings.Repeat("x", MaxFieldLen+1), Event: types.EventTaskStarted, Timestamp: "2026-09-03T10:42:31Z"}},
		{"control chars in source", types.NormalizedEvent{EventID: "e", Source: "cla\nude", SessionID: "s", Event: types.EventTaskStarted, Timestamp: "2026-09-03T10:42:31Z"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.ev); err == nil {
				t.Fatal("invalid envelope accepted")
			}
		})
	}
}

// TestSourceIsNotAClosedSet: protocol.md §4 says source is an extensible
// string, so a third agent must not need a server change.
func TestSourceIsNotAClosedSet(t *testing.T) {
	body := `{"eventId":"e","source":"codex","sessionId":"s","event":"task_started","timestamp":"2026-09-03T10:42:31Z"}`
	ev, err := Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("a new source was rejected: %v", err)
	}
	if ev.Source != "codex" {
		t.Fatalf("source = %q, want codex", ev.Source)
	}
}

// TestMalformedInputErrorsRatherThanPanics: a garbage payload must return
// an error, never take the process down (fail-open at process level).
func TestMalformedInputErrorsRatherThanPanics(t *testing.T) {
	bodies := []string{
		"", "not json", "{", "[]", "null", `{"eventId":123}`,
		`{"eventId":"e","source":"claude","sessionId":"s","event":["task_started"],"timestamp":"2026-09-03T10:42:31Z"}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Parse panicked on %q: %v", body, r)
				}
			}()
			if _, err := Parse(strings.NewReader(body)); err == nil {
				t.Fatalf("Parse(%q) accepted malformed input", body)
			}
		})
	}
}

// TestRFC3339Variants: fractional seconds and offsets are valid RFC 3339
// and should not be rejected just because the docs show a Z example.
func TestRFC3339Variants(t *testing.T) {
	for _, ts := range []string{
		"2026-09-03T10:42:31Z",
		"2026-09-03T10:42:31.123Z",
		"2026-09-03T10:42:31+05:30",
	} {
		ev := types.NormalizedEvent{EventID: "e", Source: "claude", SessionID: "s", Event: types.EventTaskStarted, Timestamp: ts}
		if err := Validate(ev); err != nil {
			t.Errorf("timestamp %q rejected: %v", ts, err)
		}
	}
}
