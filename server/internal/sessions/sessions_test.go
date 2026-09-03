package sessions

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"trafficlight/internal/types"
)

// clock is a manually advanced clock so DONE expiry and staleness are
// tested deterministically rather than by sleeping.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)}
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

func newTestManager(t *testing.T) (*Manager, *clock) {
	t.Helper()
	c := newClock()
	return NewWithClock(DefaultConfig(), c.Now), c
}

var eventSeq int

func ev(tool, session string, kind types.NormalizedEventType) types.NormalizedEvent {
	eventSeq++
	return types.NormalizedEvent{
		EventID:   fmt.Sprintf("evt_%d", eventSeq),
		Source:    tool,
		SessionID: session,
		Event:     kind,
		Timestamp: "2026-09-03T10:00:00Z",
	}
}

// stateOf reads one session's state out of a snapshot.
func stateOf(t *testing.T, m *Manager, tool, session string) (types.AgentState, bool) {
	t.Helper()
	recs, _ := m.Snapshot()
	for _, rec := range recs {
		if rec.Tool == tool && rec.SessionID == session {
			return rec.State, true
		}
	}
	return "", false
}

func mustState(t *testing.T, m *Manager, tool, session string, want types.AgentState) {
	t.Helper()
	got, ok := stateOf(t, m, tool, session)
	if !ok {
		t.Fatalf("session %s/%s not present; want state %s", tool, session, want)
	}
	if got != want {
		t.Fatalf("session %s/%s = %s, want %s", tool, session, got, want)
	}
}

// TestNormalFlow is the happy path: start, run, finish, revert to idle.
func TestNormalFlow(t *testing.T) {
	m, c := newTestManager(t)

	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	mustState(t, m, "claude", "s1", types.StateIdle)

	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	mustState(t, m, "claude", "s1", types.StateExecuting)

	m.Apply(ev("claude", "s1", types.EventTaskCompleted))
	mustState(t, m, "claude", "s1", types.StateDone)

	// DONE is ephemeral: still DONE just before the window closes.
	c.advance(4 * time.Second)
	mustState(t, m, "claude", "s1", types.StateDone)

	c.advance(2 * time.Second)
	mustState(t, m, "claude", "s1", types.StateIdle)
}

// TestPermissionFlow is the flow the product exists for: EXECUTING ->
// WAITING -> EXECUTING.
func TestPermissionFlow(t *testing.T) {
	for _, resolve := range []types.NormalizedEventType{
		types.EventPermissionGranted,
		types.EventInputReceived,
	} {
		t.Run(string(resolve), func(t *testing.T) {
			m, _ := newTestManager(t)
			m.Apply(ev("claude", "s1", types.EventSessionStarted))
			m.Apply(ev("claude", "s1", types.EventTaskStarted))
			m.Apply(ev("claude", "s1", types.EventPermissionRequested))
			mustState(t, m, "claude", "s1", types.StateWaiting)

			m.Apply(ev("claude", "s1", resolve))
			mustState(t, m, "claude", "s1", types.StateExecuting)
		})
	}
}

// TestWaitingIsNeverInferred guards the non-negotiable: WAITING comes
// only from an explicit event, never from elapsed time or silence.
func TestWaitingIsNeverInferred(t *testing.T) {
	m, c := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	m.Apply(ev("claude", "s1", types.EventTaskStarted))

	// A long-running build: hours of silence must not manufacture
	// WAITING. It may go UNKNOWN (that is staleness), but never WAITING.
	c.advance(3 * time.Hour)
	got, ok := stateOf(t, m, "claude", "s1")
	if !ok {
		t.Fatal("session disappeared without a session_ended event")
	}
	if got == types.StateWaiting {
		t.Fatal("silence produced WAITING; WAITING must come only from explicit events")
	}
}

// TestErrorFlow: ERROR requires an explicit failure event, from either
// EXECUTING or WAITING, and recovers on the next task_started.
func TestErrorFlow(t *testing.T) {
	t.Run("from executing", func(t *testing.T) {
		m, _ := newTestManager(t)
		m.Apply(ev("claude", "s1", types.EventSessionStarted))
		m.Apply(ev("claude", "s1", types.EventTaskStarted))
		m.Apply(ev("claude", "s1", types.EventTaskFailed))
		mustState(t, m, "claude", "s1", types.StateError)

		m.Apply(ev("claude", "s1", types.EventTaskStarted))
		mustState(t, m, "claude", "s1", types.StateExecuting)
	})

	t.Run("from waiting", func(t *testing.T) {
		m, _ := newTestManager(t)
		m.Apply(ev("claude", "s1", types.EventSessionStarted))
		m.Apply(ev("claude", "s1", types.EventTaskStarted))
		m.Apply(ev("claude", "s1", types.EventPermissionRequested))
		m.Apply(ev("claude", "s1", types.EventTaskFailed))
		mustState(t, m, "claude", "s1", types.StateError)
	})
}

// TestErrorDoesNotExpireLikeDone: ERROR is not ephemeral. It stays until
// an explicit event moves it.
func TestErrorDoesNotExpireLikeDone(t *testing.T) {
	m, c := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	m.Apply(ev("claude", "s1", types.EventTaskFailed))

	c.advance(10 * time.Minute)
	mustState(t, m, "claude", "s1", types.StateError)
}

// TestDuplicateEventIDIsNoOp covers the "any | duplicate eventId |
// unchanged" row — a fail-open hook retry must not re-apply a transition.
func TestDuplicateEventIDIsNoOp(t *testing.T) {
	m, _ := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))

	start := ev("claude", "s1", types.EventTaskStarted)
	first := m.Apply(start)
	if first.Duplicate {
		t.Fatal("first delivery reported as duplicate")
	}
	mustState(t, m, "claude", "s1", types.StateExecuting)

	m.Apply(ev("claude", "s1", types.EventTaskCompleted))
	mustState(t, m, "claude", "s1", types.StateDone)

	// The retry of task_started must not drag the session back to
	// EXECUTING out of DONE.
	retry := m.Apply(start)
	if !retry.Duplicate {
		t.Fatal("repeated eventId not reported as duplicate")
	}
	mustState(t, m, "claude", "s1", types.StateDone)
}

// TestDuplicateDetectionIsPerSession: the same eventId under a different
// session is a different event as far as that session is concerned.
func TestDuplicateDetectionIsPerSession(t *testing.T) {
	m, _ := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	m.Apply(ev("claude", "s2", types.EventSessionStarted))

	shared := types.NormalizedEvent{
		EventID: "evt_shared", Source: "claude", SessionID: "s1",
		Event: types.EventTaskStarted, Timestamp: "2026-09-03T10:00:00Z",
	}
	m.Apply(shared)
	shared.SessionID = "s2"
	if out := m.Apply(shared); out.Duplicate {
		t.Fatal("same eventId on a different session treated as duplicate")
	}
	mustState(t, m, "claude", "s1", types.StateExecuting)
	mustState(t, m, "claude", "s2", types.StateExecuting)
}

// TestDedupSetIsBounded: the dedup set must not grow without limit for a
// long-lived session.
func TestDedupSetIsBounded(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSeenEventIDs = 4
	c := newClock()
	m := NewWithClock(cfg, c.Now)

	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	for i := 0; i < 20; i++ {
		m.Apply(ev("claude", "s1", types.EventTaskStarted))
		m.Apply(ev("claude", "s1", types.EventTaskCompleted))
	}

	m.mu.Lock()
	rec := m.sessions[key{"claude", "s1"}]
	seen, fifo := len(rec.SeenEventIDs), len(rec.SeenEventIDFIFO)
	m.mu.Unlock()

	if seen > cfg.MaxSeenEventIDs || fifo > cfg.MaxSeenEventIDs {
		t.Fatalf("dedup set unbounded: %d ids, %d in fifo, cap %d", seen, fifo, cfg.MaxSeenEventIDs)
	}
	if seen != fifo {
		t.Fatalf("dedup set and eviction order disagree: %d vs %d", seen, fifo)
	}
}

// TestSessionEndedWhileIdleOrDoneRemovesImmediately covers protocol.md §6.
func TestSessionEndedWhileIdleOrDoneRemovesImmediately(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		m, _ := newTestManager(t)
		m.Apply(ev("claude", "s1", types.EventSessionStarted))
		out := m.Apply(ev("claude", "s1", types.EventSessionEnded))
		if !out.Removed {
			t.Fatal("session_ended while IDLE did not remove the session")
		}
		if _, ok := stateOf(t, m, "claude", "s1"); ok {
			t.Fatal("session still present after session_ended while IDLE")
		}
	})

	t.Run("done", func(t *testing.T) {
		m, _ := newTestManager(t)
		m.Apply(ev("claude", "s1", types.EventSessionStarted))
		m.Apply(ev("claude", "s1", types.EventTaskStarted))
		m.Apply(ev("claude", "s1", types.EventTaskCompleted))
		m.Apply(ev("claude", "s1", types.EventSessionEnded))
		if _, ok := stateOf(t, m, "claude", "s1"); ok {
			t.Fatal("session still present after session_ended while DONE")
		}
	})
}

// TestSessionEndedWhileUrgentStaysVisible covers the other half of §6:
// an agent that errored and exited must not make the error vanish.
func TestSessionEndedWhileUrgentStaysVisible(t *testing.T) {
	cases := []struct {
		name  string
		setup func(m *Manager)
		want  types.AgentState
	}{
		{
			name: "waiting",
			setup: func(m *Manager) {
				m.Apply(ev("claude", "s1", types.EventSessionStarted))
				m.Apply(ev("claude", "s1", types.EventTaskStarted))
				m.Apply(ev("claude", "s1", types.EventPermissionRequested))
			},
			want: types.StateWaiting,
		},
		{
			name: "error",
			setup: func(m *Manager) {
				m.Apply(ev("claude", "s1", types.EventSessionStarted))
				m.Apply(ev("claude", "s1", types.EventTaskStarted))
				m.Apply(ev("claude", "s1", types.EventTaskFailed))
			},
			want: types.StateError,
		},
		{
			name: "unknown",
			setup: func(m *Manager) {
				// An unlisted transition is what drives a session to
				// UNKNOWN without any timing involved.
				m.Apply(ev("claude", "s1", types.EventSessionStarted))
				m.Apply(ev("claude", "s1", types.EventPermissionGranted))
			},
			want: types.StateUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, c := newTestManager(t)
			tc.setup(m)
			mustState(t, m, "claude", "s1", tc.want)

			out := m.Apply(ev("claude", "s1", types.EventSessionEnded))
			if !out.RemovalScheduled {
				t.Fatalf("session_ended while %s removed immediately; it must linger", tc.want)
			}

			// Still visible, and still showing the urgent state.
			c.advance(4 * time.Second)
			mustState(t, m, "claude", "s1", tc.want)

			c.advance(2 * time.Second)
			if _, ok := stateOf(t, m, "claude", "s1"); ok {
				t.Fatalf("session still present %s after session_ended while %s", "6s", tc.want)
			}
		})
	}
}

// TestSessionEndedForUntrackedSessionIsIgnored: ending a session the
// server never saw must not briefly publish a state it never observed.
func TestSessionEndedForUntrackedSessionIsIgnored(t *testing.T) {
	m, _ := newTestManager(t)
	out := m.Apply(ev("claude", "ghost", types.EventSessionEnded))
	if !out.Ignored {
		t.Fatalf("session_ended for an untracked session was not ignored: %+v", out)
	}
	recs, _ := m.Snapshot()
	if len(recs) != 0 {
		t.Fatalf("untracked session_ended created %d session(s)", len(recs))
	}
}

// TestEventForUntrackedSessionStartsFromUnknown covers the post-restart
// case in PRD.md §5: no history means the last known state is untrusted,
// so the state is derived from the event alone.
func TestEventForUntrackedSessionStartsFromUnknown(t *testing.T) {
	cases := map[types.NormalizedEventType]types.AgentState{
		types.EventTaskStarted:         types.StateExecuting,
		types.EventPermissionRequested: types.StateWaiting,
		types.EventTaskFailed:          types.StateError,
		types.EventTaskCompleted:       types.StateDone,
	}

	for event, want := range cases {
		t.Run(string(event), func(t *testing.T) {
			m, _ := newTestManager(t)
			out := m.Apply(ev("claude", "orphan", event))
			if !out.NoHistory || !out.Created {
				t.Fatalf("expected a no-history creation, got %+v", out)
			}
			if out.Unexpected {
				t.Fatal("a valid lifecycle event on a fresh session should not be flagged unexpected")
			}
			mustState(t, m, "claude", "orphan", want)
		})
	}
}

// TestUnexpectedTransitionGoesToUnknown: an inconsistent sequence is
// logged and moves to UNKNOWN, never guessed at.
func TestUnexpectedTransitionGoesToUnknown(t *testing.T) {
	m, _ := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))

	out := m.Apply(ev("claude", "s1", types.EventTaskCompleted))
	if !out.Unexpected {
		t.Fatal("idle + task_completed was not reported as unexpected")
	}
	mustState(t, m, "claude", "s1", types.StateUnknown)

	// UNKNOWN resolves only on a real event, and does so from the event
	// alone.
	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	mustState(t, m, "claude", "s1", types.StateExecuting)
}

// TestRepeatedSessionStartedResetsToIdle covers the ruling on a second
// session_started for a tracked session: Claude Code fires SessionStart
// on /clear and on resume, so it must reset the session rather than
// raise a false UNKNOWN.
func TestRepeatedSessionStartedResetsToIdle(t *testing.T) {
	m, _ := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	m.Apply(ev("claude", "s1", types.EventPermissionRequested))
	mustState(t, m, "claude", "s1", types.StateWaiting)

	out := m.Apply(ev("claude", "s1", types.EventSessionStarted))
	if out.Unexpected {
		t.Fatal("a repeated session_started was flagged as an inconsistency")
	}
	mustState(t, m, "claude", "s1", types.StateIdle)

	// And the session still works normally afterwards.
	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	mustState(t, m, "claude", "s1", types.StateExecuting)

	recs, _ := m.Snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d sessions, want 1 — a reset must not fork the session", len(recs))
	}
}

// TestSessionStartedCancelsScheduledRemoval: a session that ended while
// urgent is pending removal; if it comes back, it must not be reaped
// moments later.
func TestSessionStartedCancelsScheduledRemoval(t *testing.T) {
	m, c := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	m.Apply(ev("claude", "s1", types.EventTaskFailed))
	if out := m.Apply(ev("claude", "s1", types.EventSessionEnded)); !out.RemovalScheduled {
		t.Fatal("expected removal to be scheduled")
	}

	c.advance(time.Second)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	mustState(t, m, "claude", "s1", types.StateIdle)

	c.advance(10 * time.Second)
	if _, ok := stateOf(t, m, "claude", "s1"); !ok {
		t.Fatal("a resumed session was still reaped by the earlier session_ended")
	}
}

// TestSessionEndedWhileExecutingRemovesImmediately records the ruling on
// the case protocol.md §6 leaves undefined. EXECUTING is treated as
// non-urgent, like IDLE and DONE: quitting mid-task is a deliberate act
// the developer already knows about, so it should not flash anything.
// Deliberately NOT turned into ERROR — that requires an explicit failure
// event and is never inferred.
func TestSessionEndedWhileExecutingRemovesImmediately(t *testing.T) {
	m, _ := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	mustState(t, m, "claude", "s1", types.StateExecuting)

	out := m.Apply(ev("claude", "s1", types.EventSessionEnded))
	if !out.Removed {
		t.Fatalf("session_ended while EXECUTING did not remove the session: %+v", out)
	}
	if _, ok := stateOf(t, m, "claude", "s1"); ok {
		t.Fatal("session still present after session_ended while EXECUTING")
	}
}

// TestUnknownDoesNotAutoResolve: UNKNOWN persists until a real event.
func TestUnknownDoesNotAutoResolve(t *testing.T) {
	m, c := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	m.Apply(ev("claude", "s1", types.EventPermissionGranted))
	mustState(t, m, "claude", "s1", types.StateUnknown)

	c.advance(time.Hour)
	mustState(t, m, "claude", "s1", types.StateUnknown)
}

// TestOutOfOrderEventsDoNotRegressIncorrectly: a late-arriving event
// never rewrites history behind a newer one. Ordering is receivedAt, and
// the hook-supplied timestamp is informational only, so an old timestamp
// on a newly arrived event must not be honoured as "earlier".
func TestOutOfOrderEventsDoNotRegressIncorrectly(t *testing.T) {
	m, _ := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	m.Apply(ev("claude", "s1", types.EventPermissionRequested))
	mustState(t, m, "claude", "s1", types.StateWaiting)

	// A task_started that the hook believes happened well before the
	// permission prompt, arriving late. WAITING is the most urgent state
	// and the one the developer needs to see; the pair is unlisted, so it
	// must land in UNKNOWN rather than silently reverting to EXECUTING
	// (which would look like "all fine, still working").
	stale := ev("claude", "s1", types.EventTaskStarted)
	stale.Timestamp = "2026-09-03T09:00:00Z"
	out := m.Apply(stale)
	if !out.Unexpected {
		t.Fatal("waiting + task_started should be an unexpected transition")
	}
	got, _ := stateOf(t, m, "claude", "s1")
	if got == types.StateExecuting {
		t.Fatal("a late event downgraded WAITING to EXECUTING; attention state lost")
	}
	if got != types.StateUnknown {
		t.Fatalf("got %s, want unknown", got)
	}
}

// TestHookTimestampDoesNotDriveDoneWindow: the DONE clock runs from
// server receive time, not the hook's timestamp (protocol.md §5).
func TestHookTimestampDoesNotDriveDoneWindow(t *testing.T) {
	m, c := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	m.Apply(ev("claude", "s1", types.EventTaskStarted))

	done := ev("claude", "s1", types.EventTaskCompleted)
	done.Timestamp = "2020-01-01T00:00:00Z" // long expired, if it counted
	m.Apply(done)
	mustState(t, m, "claude", "s1", types.StateDone)

	c.advance(6 * time.Second)
	mustState(t, m, "claude", "s1", types.StateIdle)
}

// TestMultipleSessionsPerToolAreIndependent: tmux panes and extra windows
// each get their own session record.
func TestMultipleSessionsPerToolAreIndependent(t *testing.T) {
	m, _ := newTestManager(t)
	for _, s := range []string{"s1", "s2", "s3"} {
		m.Apply(ev("claude", s, types.EventSessionStarted))
		m.Apply(ev("claude", s, types.EventTaskStarted))
	}
	m.Apply(ev("claude", "s2", types.EventPermissionRequested))
	m.Apply(ev("claude", "s3", types.EventTaskFailed))

	mustState(t, m, "claude", "s1", types.StateExecuting)
	mustState(t, m, "claude", "s2", types.StateWaiting)
	mustState(t, m, "claude", "s3", types.StateError)
}

// TestSessionsAreKeyedByToolAndID: the same sessionId reported by two
// different tools is two sessions, not one.
func TestSessionsAreKeyedByToolAndID(t *testing.T) {
	m, _ := newTestManager(t)
	m.Apply(ev("claude", "same", types.EventSessionStarted))
	m.Apply(ev("copilot", "same", types.EventSessionStarted))
	m.Apply(ev("claude", "same", types.EventTaskStarted))

	mustState(t, m, "claude", "same", types.StateExecuting)
	mustState(t, m, "copilot", "same", types.StateIdle)
}

// TestStalenessMarksChattySessionUnknown covers protocol.md §7: a session
// that was emitting regularly and then goes fully silent while EXECUTING.
func TestStalenessMarksChattySessionUnknown(t *testing.T) {
	m, c := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))

	// Establish a ~1s cadence, ending in EXECUTING.
	for i := 0; i < 8; i++ {
		c.advance(time.Second)
		m.Apply(ev("claude", "s1", types.EventTaskStarted))
		c.advance(time.Second)
		m.Apply(ev("claude", "s1", types.EventTaskCompleted))
	}
	c.advance(time.Second)
	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	mustState(t, m, "claude", "s1", types.StateExecuting)

	// Below the floor: still trusted, however unusual the silence is
	// relative to cadence. False negatives beat false positives.
	c.advance(90 * time.Second)
	mustState(t, m, "claude", "s1", types.StateExecuting)

	c.advance(60 * time.Second)
	mustState(t, m, "claude", "s1", types.StateUnknown)
}

// TestStalenessDoesNotPenalizeInfrequentSession is the requirement that
// staleness is cadence-relative, not a blanket timeout: a session that
// was always slow is not penalized for continuing to be slow.
func TestStalenessDoesNotPenalizeInfrequentSession(t *testing.T) {
	m, c := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))

	// A ~5-minute cadence: long builds between events.
	for i := 0; i < 8; i++ {
		c.advance(5 * time.Minute)
		m.Apply(ev("claude", "s1", types.EventTaskStarted))
		c.advance(5 * time.Minute)
		m.Apply(ev("claude", "s1", types.EventTaskCompleted))
	}
	c.advance(5 * time.Minute)
	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	mustState(t, m, "claude", "s1", types.StateExecuting)

	// Ten minutes of silence would have flipped the chatty session long
	// ago; for this one it is ordinary.
	c.advance(10 * time.Minute)
	mustState(t, m, "claude", "s1", types.StateExecuting)
}

// TestLongRunningTaskWithNoCadenceStaysTrusted: PRD.md §7 explicitly
// requires that 10+ minutes in EXECUTING is normal and must not flip to
// UNKNOWN just for being long.
func TestLongRunningTaskWithNoCadenceStaysTrusted(t *testing.T) {
	m, c := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	c.advance(time.Second)
	m.Apply(ev("claude", "s1", types.EventTaskStarted))

	// Two events is not enough cadence history to judge silence against.
	c.advance(45 * time.Minute)
	mustState(t, m, "claude", "s1", types.StateExecuting)
}

// TestStalenessOnlyAppliesToExecuting: WAITING must never be overwritten
// by staleness — it is the state the product exists to surface.
func TestStalenessOnlyAppliesToExecuting(t *testing.T) {
	m, c := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	for i := 0; i < 8; i++ {
		c.advance(time.Second)
		m.Apply(ev("claude", "s1", types.EventTaskStarted))
		c.advance(time.Second)
		m.Apply(ev("claude", "s1", types.EventTaskCompleted))
	}
	c.advance(time.Second)
	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	c.advance(time.Second)
	m.Apply(ev("claude", "s1", types.EventPermissionRequested))
	mustState(t, m, "claude", "s1", types.StateWaiting)

	c.advance(2 * time.Hour)
	mustState(t, m, "claude", "s1", types.StateWaiting)
}

// TestStalenessDoesNotRefire: once a session has gone UNKNOWN through
// staleness it stays there until a real event, and cadence history is
// rebuilt from scratch.
func TestStalenessDoesNotRefire(t *testing.T) {
	m, c := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	for i := 0; i < 8; i++ {
		c.advance(time.Second)
		m.Apply(ev("claude", "s1", types.EventTaskStarted))
		c.advance(time.Second)
		m.Apply(ev("claude", "s1", types.EventTaskCompleted))
	}
	c.advance(time.Second)
	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	c.advance(3 * time.Minute)
	mustState(t, m, "claude", "s1", types.StateUnknown)

	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	mustState(t, m, "claude", "s1", types.StateExecuting)

	// Cadence was discarded, so this session is now treated as one with
	// no history rather than immediately flipping again.
	c.advance(3 * time.Minute)
	mustState(t, m, "claude", "s1", types.StateExecuting)
}

// TestUpdatedAtAdvancesOnStateChange: /state's updatedAt must move when
// state actually changes, including for a purely time-driven change.
func TestUpdatedAtAdvancesOnStateChange(t *testing.T) {
	m, c := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	m.Apply(ev("claude", "s1", types.EventTaskCompleted))
	_, before := m.Snapshot()

	c.advance(6 * time.Second) // DONE -> IDLE, with no event involved
	_, after := m.Snapshot()
	if !after.After(before) {
		t.Fatalf("updatedAt did not advance across a time-driven transition: %s -> %s", before, after)
	}
}

// TestSinceTracksStateEntry: `since` is when the state was entered, not
// when the last event arrived. Note that no matrix row maps a state to
// itself, so every accepted event either changes state or is a duplicate.
func TestSinceTracksStateEntry(t *testing.T) {
	m, c := newTestManager(t)
	sinceOf := func() time.Time {
		t.Helper()
		recs, _ := m.Snapshot()
		if len(recs) != 1 {
			t.Fatalf("got %d sessions, want 1", len(recs))
		}
		return recs[0].Since
	}

	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	if got := sinceOf(); !got.Equal(c.Now()) {
		t.Fatalf("since after creation = %s, want %s", got, c.Now())
	}

	c.advance(time.Second)
	m.Apply(ev("claude", "s1", types.EventTaskStarted))
	firstExecuting := c.Now()
	if got := sinceOf(); !got.Equal(firstExecuting) {
		t.Fatalf("since on entering EXECUTING = %s, want %s", got, firstExecuting)
	}

	c.advance(30 * time.Second)
	m.Apply(ev("claude", "s1", types.EventPermissionRequested))
	enteredWaiting := c.Now()
	if got := sinceOf(); !got.Equal(enteredWaiting) {
		t.Fatalf("since on entering WAITING = %s, want %s", got, enteredWaiting)
	}

	// Re-entering EXECUTING starts a new EXECUTING period, so `since`
	// restamps rather than reporting the original start.
	c.advance(5 * time.Second)
	m.Apply(ev("claude", "s1", types.EventPermissionGranted))
	if got := sinceOf(); !got.Equal(c.Now()) {
		t.Fatalf("since on re-entering EXECUTING = %s, want %s", got, c.Now())
	}

	// A duplicate does not touch `since` — it is a no-op, not a
	// re-applied transition.
	dup := ev("claude", "s1", types.EventPermissionRequested)
	m.Apply(dup)
	stamped := sinceOf()
	c.advance(10 * time.Second)
	m.Apply(dup)
	if got := sinceOf(); !got.Equal(stamped) {
		t.Fatalf("duplicate event restamped since: %s -> %s", stamped, got)
	}
}

// TestSnapshotDoesNotLeakInternals: clients must never be handed the
// mutable server-internal bookkeeping.
func TestSnapshotDoesNotLeakInternals(t *testing.T) {
	m, _ := newTestManager(t)
	m.Apply(ev("claude", "s1", types.EventSessionStarted))
	recs, _ := m.Snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d sessions, want 1", len(recs))
	}
	if recs[0].SeenEventIDs != nil || recs[0].SeenEventIDFIFO != nil ||
		recs[0].RecentIntervals != nil || recs[0].RemoveAt != nil || recs[0].DoneExpiresAt != nil {
		t.Fatal("snapshot exposed internal bookkeeping fields")
	}
}

// TestConcurrentApplyIsRaceFree exercises the lock under -race.
func TestConcurrentApplyIsRaceFree(t *testing.T) {
	m := New(DefaultConfig())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			session := fmt.Sprintf("s%d", n)
			for j := 0; j < 50; j++ {
				m.Apply(types.NormalizedEvent{
					EventID:   fmt.Sprintf("evt_%d_%d", n, j),
					Source:    "claude",
					SessionID: session,
					Event:     types.EventTaskStarted,
					Timestamp: "2026-09-03T10:00:00Z",
				})
				m.Snapshot()
			}
		}(i)
	}
	wg.Wait()

	recs, _ := m.Snapshot()
	if len(recs) != 8 {
		t.Fatalf("got %d sessions, want 8", len(recs))
	}
}
