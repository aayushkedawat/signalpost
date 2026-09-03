package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"trafficlight/internal/events"
	"trafficlight/internal/types"
)

// The fixtures in testdata/ are real Claude Code hook payloads with every
// content-bearing field removed (prompt, tool_input, tool_response,
// transcript_path, cwd, ...) and the session id replaced. They are kept
// because the point of this adapter is to match what Claude Code actually
// sends, which is not something a hand-written payload can attest to.

func loadFixture(t *testing.T, hook string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", hook+".json"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return raw
}

// TestRealPayloadsMapCorrectly is the test this package exists for.
func TestRealPayloadsMapCorrectly(t *testing.T) {
	want := map[string]types.NormalizedEventType{
		"SessionStart":       types.EventSessionStarted,
		"UserPromptSubmit":   types.EventTaskStarted,
		"PermissionRequest":  types.EventPermissionRequested,
		"PostToolUse":        types.EventPermissionGranted,
		"PostToolUseFailure": types.EventTaskFailed,
		"Stop":               types.EventTaskCompleted,
		"SessionEnd":         types.EventSessionEnded,
	}

	for hook, wantEvent := range want {
		t.Run(hook, func(t *testing.T) {
			ev, ok, err := Normalize(loadFixture(t, hook))
			if err != nil {
				t.Fatalf("Normalize(%s) errored: %v", hook, err)
			}
			if !ok {
				t.Fatalf("Normalize(%s) reported no mapping, want %s", hook, wantEvent)
			}
			if ev.Event != wantEvent {
				t.Errorf("got %s, want %s", ev.Event, wantEvent)
			}
			if ev.Source != Source {
				t.Errorf("source = %q, want %q", ev.Source, Source)
			}
			if ev.SessionID == "" {
				t.Error("sessionId is empty")
			}
			if ev.EventID == "" {
				t.Error("eventId is empty")
			}
		})
	}
}

// TestPreToolUseIsIgnored guards the decision that keeps every ordinary
// tool call out of UNKNOWN. If someone maps PreToolUse to task_started,
// it arrives immediately after UserPromptSubmit's task_started, and
// EXECUTING+task_started is not a listed transition.
func TestPreToolUseIsIgnored(t *testing.T) {
	ev, ok, err := Normalize(loadFixture(t, "PreToolUse"))
	if err != nil {
		t.Fatalf("PreToolUse should be ignored, not an error: %v", err)
	}
	if ok {
		t.Fatalf("PreToolUse mapped to %s; it must be ignored (see protocol.md §8)", ev.Event)
	}
}

// TestEnvelopeCarriesNothingElse is the privacy guard. PRD §9 limits what
// crosses the hook→server boundary to five fields; this asserts that a
// payload rich in prompts and file contents produces an event carrying
// none of it.
func TestEnvelopeCarriesNothingElse(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "UserPromptSubmit",
		"session_id": "s-1",
		"prompt": "SENSITIVE-PROMPT-TEXT",
		"cwd": "/Users/someone/secret-project",
		"transcript_path": "/Users/someone/.claude/transcript.jsonl",
		"tool_input": {"command": "cat ~/.ssh/id_rsa", "file_path": "/etc/passwd"},
		"tool_response": {"output": "SENSITIVE-OUTPUT"},
		"last_assistant_message": "SENSITIVE-REPLY"
	}`)

	ev, ok, err := Normalize(raw)
	if err != nil || !ok {
		t.Fatalf("Normalize failed: ok=%v err=%v", ok, err)
	}

	encoded, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"SENSITIVE-PROMPT-TEXT", "SENSITIVE-OUTPUT", "SENSITIVE-REPLY",
		"secret-project", "id_rsa", "/etc/passwd", "transcript",
	} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("normalized event leaked %q: %s", secret, encoded)
		}
	}

	// Exactly the five fields of the envelope, nothing more.
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{"eventId", "source", "sessionId", "event", "timestamp"}
	if len(fields) != len(want) {
		t.Errorf("envelope has %d fields, want %d: %v", len(fields), len(want), fields)
	}
	for _, k := range want {
		if _, present := fields[k]; !present {
			t.Errorf("envelope missing %q", k)
		}
	}
}

func TestUnmappedHookIsIgnoredNotAnError(t *testing.T) {
	// Hooks Claude Code has that we deliberately do not map, plus one
	// that does not exist, to cover future additions.
	for _, hook := range []string{"PreCompact", "PostCompact", "SubagentStart", "SomeFutureHook"} {
		raw := []byte(`{"hook_event_name":"` + hook + `","session_id":"s-1"}`)
		_, ok, err := Normalize(raw)
		if err != nil {
			t.Errorf("%s: unmapped hook should not error, got %v", hook, err)
		}
		if ok {
			t.Errorf("%s: should be ignored", hook)
		}
	}
}

func TestMalformedInputErrorsRatherThanPanics(t *testing.T) {
	cases := map[string][]byte{
		"empty":              []byte(``),
		"not json":           []byte(`this is not json`),
		"truncated":          []byte(`{"hook_event_name": "Stop"`),
		"json array":         []byte(`["Stop"]`),
		"null":               []byte(`null`),
		"no hook name":       []byte(`{"session_id":"s-1"}`),
		"no session id":      []byte(`{"hook_event_name":"Stop"}`),
		"wrong field types":  []byte(`{"hook_event_name": 42, "session_id": []}`),
		"empty hook name":    []byte(`{"hook_event_name":"","session_id":"s-1"}`),
		"empty session id":   []byte(`{"hook_event_name":"Stop","session_id":""}`),
		"deeply nested junk": []byte(`{"hook_event_name":"Stop","session_id":"s","x":{"y":{"z":[1,2,3]}}}`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			// The contract is simply that it returns rather than panics:
			// a panic in a hook process would violate fail-open at the
			// process level.
			ev, ok, err := Normalize(raw)
			if ok && err != nil {
				t.Errorf("returned both a mapping and an error: %+v / %v", ev, err)
			}
		})
	}
}

// TestEventIDsAreUnique covers the resume case: SessionStart fires twice
// for one session (startup, then resume — both observed in the captures).
// A content-derived id would collide and the server would dedup the
// second away, silently losing the session reset.
func TestEventIDsAreUnique(t *testing.T) {
	raw := loadFixture(t, "SessionStart")
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		ev, ok, err := Normalize(raw)
		if err != nil || !ok {
			t.Fatalf("Normalize failed: %v", err)
		}
		if seen[ev.EventID] {
			t.Fatalf("duplicate eventId %q — a resumed session would be deduplicated away", ev.EventID)
		}
		seen[ev.EventID] = true
	}
}

// TestOutputPassesTheRealServerValidator closes the loop between the two
// halves of the boundary. The adapter runs hook-side and the validator
// runs server-side, so nothing else would catch a disagreement between
// them — an event the adapter happily produces but the server rejects
// would simply vanish, and the light would quietly stop matching reality.
// Asserting against events.Validate rather than a reimplementation of it
// is the point: a copy of the rules here could drift from the real ones.
func TestOutputPassesTheRealServerValidator(t *testing.T) {
	for hook := range mapping {
		t.Run(hook, func(t *testing.T) {
			ev, ok, err := Normalize(loadFixture(t, hook))
			if err != nil || !ok {
				t.Fatalf("Normalize failed: ok=%v err=%v", ok, err)
			}
			if err := events.Validate(ev); err != nil {
				t.Errorf("server would reject this adapter's own output: %v", err)
			}
		})
	}
}

// TestOutputSurvivesTheWire runs the adapter's output through the exact
// path a real POST takes: JSON encode, then the server's own parser,
// which rejects unknown fields.
func TestOutputSurvivesTheWire(t *testing.T) {
	ev, ok, err := Normalize(loadFixture(t, "PermissionRequest"))
	if err != nil || !ok {
		t.Fatalf("Normalize failed: %v", err)
	}
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	got, err := events.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("the server could not parse this adapter's own output: %v", err)
	}
	if got.Event != types.EventPermissionRequested || got.SessionID != ev.SessionID {
		t.Errorf("round trip changed the event: %+v vs %+v", got, ev)
	}
}
