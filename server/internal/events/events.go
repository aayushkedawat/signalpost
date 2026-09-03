// Package events decodes and validates the normalized event envelope
// (docs/protocol.md §4). Unexpected fields are rejected at the schema
// level rather than stripped, which is the enforcement point for the
// privacy requirement in PRD.md §9: nothing but eventId, source,
// sessionId, event and timestamp may cross the hook -> server boundary.
//
// This package knows nothing about vendor hook names — that is what
// adapters are for (protocol.md §8, phases 2 and 3).
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"trafficlight/internal/types"
)

// MaxFieldLen bounds every string field. The server keeps sessions in
// memory keyed by source+sessionId, so unbounded identifiers would be an
// unbounded allocation driven by a caller.
const MaxFieldLen = 200

var ErrEmptyBody = errors.New("empty request body")

// known is the set from protocol.md §1. Anything else is rejected —
// the state machine must never see a name it has no row for.
var known = map[types.NormalizedEventType]struct{}{
	types.EventSessionStarted:      {},
	types.EventTaskStarted:         {},
	types.EventPermissionRequested: {},
	types.EventPermissionGranted:   {},
	types.EventInputReceived:       {},
	types.EventTaskCompleted:       {},
	types.EventTaskFailed:          {},
	types.EventSessionEnded:        {},
}

// Parse decodes exactly one event envelope from r and validates it.
func Parse(r io.Reader) (types.NormalizedEvent, error) {
	var ev types.NormalizedEvent

	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ev); err != nil {
		if errors.Is(err, io.EOF) {
			return ev, ErrEmptyBody
		}
		return ev, fmt.Errorf("malformed event: %w", err)
	}
	// Reject trailing content so a body carrying a second (possibly
	// content-bearing) object can't slip past DisallowUnknownFields.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return ev, errors.New("malformed event: unexpected trailing content")
	}
	return ev, Validate(ev)
}

// Validate enforces the envelope contract on an already-decoded event.
func Validate(ev types.NormalizedEvent) error {
	for _, f := range []struct {
		name  string
		value string
	}{
		{"eventId", ev.EventID},
		{"source", ev.Source},
		{"sessionId", ev.SessionID},
	} {
		if err := checkIdentifier(f.name, f.value); err != nil {
			return err
		}
	}
	if ev.Event == "" {
		return errors.New("event is required")
	}
	if _, ok := known[ev.Event]; !ok {
		return fmt.Errorf("unknown event type %q", ev.Event)
	}
	if ev.Timestamp == "" {
		return errors.New("timestamp is required")
	}
	// timestamp is informational only (protocol.md §4) — the server
	// orders by its own receivedAt — but it is still part of the frozen
	// envelope, so a malformed one is an adapter bug worth surfacing now
	// rather than in phase 2.
	if _, err := time.Parse(time.RFC3339, ev.Timestamp); err != nil {
		return fmt.Errorf("timestamp must be RFC 3339: %w", err)
	}
	return nil
}

func checkIdentifier(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > MaxFieldLen {
		return fmt.Errorf("%s exceeds %d bytes", name, MaxFieldLen)
	}
	if strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("%s contains control characters", name)
	}
	return nil
}
