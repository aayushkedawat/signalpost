// Package statemachine implements the transition matrix in
// docs/protocol.md §3. It is pure: no clocks, no I/O, no vendor
// knowledge. Session creation, dedup, session_ended and staleness are
// deliberately NOT handled here — they are session-lifecycle concerns
// (protocol.md §5-§7) owned by internal/sessions.
package statemachine

import "trafficlight/internal/types"

type key struct {
	state types.AgentState
	event types.NormalizedEventType
}

// matrix is protocol.md §3, one entry per listed row. A pair absent
// from this table is an unexpected transition: the session moves to
// UNKNOWN and the caller logs it (never silently ignored or guessed at).
var matrix = map[key]types.AgentState{
	{types.StateIdle, types.EventTaskStarted}:              types.StateExecuting,
	{types.StateExecuting, types.EventPermissionRequested}: types.StateWaiting,
	{types.StateExecuting, types.EventTaskCompleted}:       types.StateDone,
	{types.StateExecuting, types.EventTaskFailed}:          types.StateError,
	{types.StateWaiting, types.EventPermissionGranted}:     types.StateExecuting,
	// Added in phase 2 from captured Claude Code sequences. PostToolUse
	// fires after *every* tool call, not only after an approval, so
	// permission_granted arrives routinely while already EXECUTING;
	// without this row ordinary tool use would fall off the matrix and
	// land in UNKNOWN. Idempotent: it confirms EXECUTING, never leaves it.
	{types.StateExecuting, types.EventPermissionGranted}: types.StateExecuting,
	// Also phase 2, from a captured denial: rejecting a permission
	// prompt emits no PermissionDenied hook and no PostToolUse — the
	// only signal is the user's next prompt, which normalizes to
	// task_started while the session is still WAITING. Work has resumed,
	// so this is EXECUTING.
	{types.StateWaiting, types.EventTaskStarted}:   types.StateExecuting,
	{types.StateWaiting, types.EventInputReceived}: types.StateExecuting,
	{types.StateWaiting, types.EventTaskFailed}:    types.StateError,
	{types.StateError, types.EventTaskStarted}:     types.StateExecuting,
	// The five rows below were added after replaying real captured Claude
	// Code sessions through this table: each occurred in practice and fell
	// off the matrix, turning the light cyan ("not sure") during ordinary
	// work.
	//
	// ERROR is not terminal — a failed tool call is followed by more work,
	// not by the end of the session. A command exits non-zero (ERROR) and
	// the next tool call then succeeds, or asks for approval.
	{types.StateError, types.EventPermissionGranted}:   types.StateExecuting,
	{types.StateError, types.EventPermissionRequested}: types.StateWaiting,
	// A second prompt submitted while the agent is still working: the user
	// queues a message, or interrupts and retypes. Work continues.
	{types.StateExecuting, types.EventTaskStarted}: types.StateExecuting,
	// Two permission prompts back to back. Idempotent: still waiting.
	{types.StateWaiting, types.EventPermissionRequested}: types.StateWaiting,
	// The turn ended while a prompt was outstanding — a denial that
	// stopped the agent, for instance. The prompt goes with it.
	{types.StateWaiting, types.EventTaskCompleted}: types.StateDone,
	{types.StateDone, types.EventTaskStarted}:      types.StateExecuting,
	// session_started is handled ahead of this table (it applies from any
	// state), so it deliberately has no rows here.
}

// fromEvent is the UNKNOWN row of protocol.md §3: "any valid lifecycle
// event -> derived per this table, from the event alone". A session the
// server has lost confidence in regains a confident state purely from
// the semantics of the next event it sees, ignoring the stale current
// state.
var fromEvent = map[types.NormalizedEventType]types.AgentState{
	// Also short-circuited in Next() ahead of this table, since it
	// applies from any state; kept here so the table stays a complete
	// description of the UNKNOWN row.
	types.EventSessionStarted:      types.StateIdle,
	types.EventTaskStarted:         types.StateExecuting,
	types.EventPermissionRequested: types.StateWaiting,
	types.EventPermissionGranted:   types.StateExecuting,
	types.EventInputReceived:       types.StateExecuting,
	types.EventTaskCompleted:       types.StateDone,
	types.EventTaskFailed:          types.StateError,
}

// Result is the outcome of applying one event to one session's state.
type Result struct {
	Next types.AgentState
	// Unexpected reports that the (current, event) pair is not in the
	// matrix. Next is UNKNOWN in that case and the caller should log the
	// transition (protocol.md §3, final paragraph).
	Unexpected bool
}

// Next resolves one transition. session_ended is not a matrix event
// (protocol.md §6 routes it through the session lifecycle instead); if
// it reaches here it is reported as unexpected rather than guessed at.
func Next(cur types.AgentState, ev types.NormalizedEventType) Result {
	if ev == types.EventSessionEnded {
		return Result{Next: types.StateUnknown, Unexpected: true}
	}
	// session_started declares "this session is now fresh and idle",
	// whatever the server previously believed — the same rule as
	// "(none) | session_started | IDLE", applied uniformly. Claude Code
	// fires SessionStart on /clear and on resume, so a repeat for an
	// already-tracked session is an ordinary occurrence rather than an
	// inconsistency, and must not raise a false UNKNOWN (protocol.md §3).
	if ev == types.EventSessionStarted {
		return Result{Next: types.StateIdle}
	}
	if cur == types.StateUnknown {
		if next, ok := fromEvent[ev]; ok {
			return Result{Next: next}
		}
		return Result{Next: types.StateUnknown, Unexpected: true}
	}
	if next, ok := matrix[key{cur, ev}]; ok {
		return Result{Next: next}
	}
	return Result{Next: types.StateUnknown, Unexpected: true}
}
