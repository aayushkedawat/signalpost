// Package claude translates Claude Code hook payloads into normalized
// events. It is the only place in the tree that knows Claude Code's hook
// names, per docs/protocol.md §8 — nothing downstream of Normalize sees a
// vendor-specific name.
//
// The mapping is validated against payloads captured from real sessions
// (see tools/capture/), not inferred from documentation. Notes on what
// the captures corrected are in protocol.md §8.
//
// Normalize is a pure function of a single payload: it holds no state
// between calls and never consults a clock beyond stamping the event.
// That is deliberate and load-bearing — it is what allows this code to
// run inside the hook process, which is in turn what keeps raw payloads
// (prompts, file contents, tool arguments) from ever crossing the
// hook→server boundary, as PRD §9 requires.
package claude

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"trafficlight/internal/types"
)

// Source is the tool identifier this adapter emits.
const Source = "claude"

// payload is the subset of a Claude Code hook payload we read. Every
// other field — prompt text, tool_input, tool_response, transcript_path —
// is deliberately not decoded: it must not travel to the server, and not
// naming it here is the simplest way to guarantee that.
type payload struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
}

// mapping is the validated hook → normalized event table (protocol.md §8).
//
// PreToolUse is deliberately absent rather than missing by oversight:
// UserPromptSubmit already establishes EXECUTING at the start of the turn,
// earlier than the first tool call, so PreToolUse carries no state. Were
// both mapped to task_started they would arrive back to back and put every
// tool call into UNKNOWN, since EXECUTING+task_started is not a listed
// transition.
var mapping = map[string]types.NormalizedEventType{
	"SessionStart":       types.EventSessionStarted,
	"UserPromptSubmit":   types.EventTaskStarted,
	"PermissionRequest":  types.EventPermissionRequested,
	"PostToolUse":        types.EventPermissionGranted,
	"PostToolUseFailure": types.EventTaskFailed,
	"Stop":               types.EventTaskCompleted,
	"SessionEnd":         types.EventSessionEnded,
}

// Normalize converts one raw Claude Code hook payload into a normalized
// event.
//
// The bool reports whether the payload maps to anything at all: false
// means "ignore this hook", which is a normal outcome (PreToolUse, and
// any hook Claude Code adds that we have not mapped), not a failure.
// An error means the payload was unreadable or unusable.
//
// Callers must treat every non-nil error as "do nothing and exit
// successfully". Nothing here is worth interrupting an agent over.
func Normalize(raw []byte) (types.NormalizedEvent, bool, error) {
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return types.NormalizedEvent{}, false, fmt.Errorf("malformed hook payload: %w", err)
	}
	if p.HookEventName == "" {
		return types.NormalizedEvent{}, false, fmt.Errorf("hook payload has no hook_event_name")
	}

	event, ok := mapping[p.HookEventName]
	if !ok {
		// Not an error: an unmapped hook is simply not interesting.
		return types.NormalizedEvent{}, false, nil
	}

	// Every hook observed in the captures carries session_id. A payload
	// without one cannot be attributed to a session, and a normalized
	// event with an empty sessionId would be rejected by the server, so
	// reject it here where the reason is legible.
	if p.SessionID == "" {
		return types.NormalizedEvent{}, false, fmt.Errorf("hook %s has no session_id", p.HookEventName)
	}

	id, err := newEventID()
	if err != nil {
		return types.NormalizedEvent{}, false, err
	}

	return types.NormalizedEvent{
		EventID:   id,
		Source:    Source,
		SessionID: p.SessionID,
		Event:     event,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}, true, nil
}

// newEventID returns a random dedup identity.
//
// Deliberately random rather than derived from payload fields. A
// content-derived id was considered — Claude Code supplies prompt_id and
// tool_use_id, which look like natural keys — but SessionStart carries
// neither, so two legitimate SessionStart events for one session (startup
// then resume, both observed in the captures) would collide and the
// second would be silently deduplicated away, losing the session reset.
// Duplicate delivery is not a risk worth trading that for: the hook posts
// once and never retries.
func newEventID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating event id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
