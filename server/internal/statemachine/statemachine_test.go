package statemachine

import (
	"testing"

	"trafficlight/internal/types"
)

// TestMatrixRows covers every row of docs/protocol.md §3 that the pure
// state machine owns. Session creation, dedup, session_ended and
// staleness are covered in internal/sessions.
func TestMatrixRows(t *testing.T) {
	cases := []struct {
		name  string
		from  types.AgentState
		event types.NormalizedEventType
		want  types.AgentState
	}{
		{"idle + task_started", types.StateIdle, types.EventTaskStarted, types.StateExecuting},
		{"executing + permission_requested", types.StateExecuting, types.EventPermissionRequested, types.StateWaiting},
		{"executing + task_completed", types.StateExecuting, types.EventTaskCompleted, types.StateDone},
		{"executing + task_failed", types.StateExecuting, types.EventTaskFailed, types.StateError},
		{"waiting + permission_granted", types.StateWaiting, types.EventPermissionGranted, types.StateExecuting},
		{"waiting + input_received", types.StateWaiting, types.EventInputReceived, types.StateExecuting},
		{"waiting + task_failed", types.StateWaiting, types.EventTaskFailed, types.StateError},
		{"error + task_started", types.StateError, types.EventTaskStarted, types.StateExecuting},
		{"done + task_started", types.StateDone, types.EventTaskStarted, types.StateExecuting},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Next(tc.from, tc.event)
			if got.Unexpected {
				t.Fatalf("Next(%s, %s) reported unexpected; want a listed transition", tc.from, tc.event)
			}
			if got.Next != tc.want {
				t.Fatalf("Next(%s, %s) = %s, want %s", tc.from, tc.event, got.Next, tc.want)
			}
		})
	}
}

// TestUnknownRowDerivesFromEventAlone covers "UNKNOWN | any valid
// lifecycle event | derived per this table, from the event alone".
func TestUnknownRowDerivesFromEventAlone(t *testing.T) {
	cases := map[types.NormalizedEventType]types.AgentState{
		types.EventSessionStarted:      types.StateIdle,
		types.EventTaskStarted:         types.StateExecuting,
		types.EventPermissionRequested: types.StateWaiting,
		types.EventPermissionGranted:   types.StateExecuting,
		types.EventInputReceived:       types.StateExecuting,
		types.EventTaskCompleted:       types.StateDone,
		types.EventTaskFailed:          types.StateError,
	}

	for event, want := range cases {
		got := Next(types.StateUnknown, event)
		if got.Unexpected {
			t.Errorf("Next(unknown, %s) reported unexpected; the UNKNOWN row accepts any valid lifecycle event", event)
		}
		if got.Next != want {
			t.Errorf("Next(unknown, %s) = %s, want %s", event, got.Next, want)
		}
	}
}

// TestUnlistedPairsGoToUnknown covers §3's final rule: a pair with no row
// is logged as unexpected and moves to UNKNOWN, never ignored or guessed.
func TestUnlistedPairsGoToUnknown(t *testing.T) {
	allStates := []types.AgentState{
		types.StateIdle, types.StateExecuting, types.StateWaiting,
		types.StateDone, types.StateError, types.StateUnknown,
	}
	allEvents := []types.NormalizedEventType{
		types.EventSessionStarted, types.EventTaskStarted,
		types.EventPermissionRequested, types.EventPermissionGranted,
		types.EventInputReceived, types.EventTaskCompleted,
		types.EventTaskFailed, types.EventSessionEnded,
	}

	// Every (state, event) pair is either a listed row or must be
	// reported unexpected with next == UNKNOWN. This is exhaustive, so a
	// future edit to the matrix cannot leave a pair silently ignored.
	for _, state := range allStates {
		for _, event := range allEvents {
			got := Next(state, event)
			_, listed := matrix[key{state, event}]
			if state == types.StateUnknown && event != types.EventSessionEnded {
				listed = true
			}
			// session_started applies from any state (see TestSessionStartedResetsFromAnyState).
			if event == types.EventSessionStarted {
				listed = true
			}
			if listed {
				if got.Unexpected {
					t.Errorf("Next(%s, %s): listed row reported unexpected", state, event)
				}
				continue
			}
			if !got.Unexpected {
				t.Errorf("Next(%s, %s): unlisted pair not reported unexpected", state, event)
			}
			if got.Next != types.StateUnknown {
				t.Errorf("Next(%s, %s) = %s, want unknown", state, event, got.Next)
			}
		}
	}
}

// TestSpecificUnlistedPairs pins down cases worth naming explicitly,
// because each is a plausible real hook sequence.
func TestSpecificUnlistedPairs(t *testing.T) {
	cases := []struct {
		name  string
		from  types.AgentState
		event types.NormalizedEventType
	}{
		// A permission prompt cannot appear while nothing is running.
		{"idle + permission_requested", types.StateIdle, types.EventPermissionRequested},
		// A completion with no task in flight means we missed the start.
		{"idle + task_completed", types.StateIdle, types.EventTaskCompleted},
		// A second Stop for one task, or a lost task_started.
		{"done + task_completed", types.StateDone, types.EventTaskCompleted},
		// Permission granted with no prompt outstanding.
		{"executing + permission_granted", types.StateExecuting, types.EventPermissionGranted},
		// session_ended is a lifecycle concern, not a matrix event.
		{"executing + session_ended", types.StateExecuting, types.EventSessionEnded},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Next(tc.from, tc.event)
			if !got.Unexpected || got.Next != types.StateUnknown {
				t.Fatalf("Next(%s, %s) = %+v, want unexpected -> unknown", tc.from, tc.event, got)
			}
		})
	}
}

// TestSessionStartedResetsFromAnyState covers the ruling on a repeated
// session_started: it declares the session fresh and idle regardless of
// what the server previously believed, rather than raising UNKNOWN.
// Claude Code fires SessionStart on /clear and on resume, so this is an
// ordinary event, not an inconsistency.
func TestSessionStartedResetsFromAnyState(t *testing.T) {
	for _, from := range []types.AgentState{
		types.StateIdle, types.StateExecuting, types.StateWaiting,
		types.StateDone, types.StateError, types.StateUnknown,
	} {
		got := Next(from, types.EventSessionStarted)
		if got.Unexpected {
			t.Errorf("Next(%s, session_started) reported unexpected", from)
		}
		if got.Next != types.StateIdle {
			t.Errorf("Next(%s, session_started) = %s, want idle", from, got.Next)
		}
	}
}

// TestNextIsPure guards the "no clocks, no I/O" property: the same input
// always yields the same output.
func TestNextIsPure(t *testing.T) {
	first := Next(types.StateExecuting, types.EventTaskCompleted)
	for i := 0; i < 100; i++ {
		if Next(types.StateExecuting, types.EventTaskCompleted) != first {
			t.Fatal("Next is not deterministic")
		}
	}
}
