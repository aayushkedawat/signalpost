// Package sessions owns the live session table: creation, deduplication,
// the session_ended lifecycle (docs/protocol.md §6), the ephemeral DONE
// window (§5) and the staleness -> UNKNOWN heuristic (§7). It delegates
// every state transition to internal/statemachine so the matrix lives in
// exactly one place.
//
// Nothing here blocks on disk, network or logging: Apply is on the hot
// path for POST /events and must stay fast (PRD.md §7, fail-open). Time-
// based effects (DONE expiry, scheduled removal, staleness) are resolved
// lazily whenever the table is touched, so there is no timer goroutine to
// contend with.
package sessions

import (
	"sort"
	"sync"
	"time"

	"trafficlight/internal/statemachine"
	"trafficlight/internal/types"
)

// Config holds the tunables from protocol.md §5 and §7.
type Config struct {
	// DoneDuration is how long DONE stays visible before reverting to
	// IDLE, and how long an urgent state survives session_ended (§5, §6).
	DoneDuration time.Duration

	// StaleFactor multiplies a session's own median event interval to get
	// its silence threshold — this is what makes staleness cadence-
	// relative rather than a fixed timeout (§7).
	StaleFactor float64

	// StaleFloor is the minimum silence before any session can go
	// UNKNOWN, regardless of how chatty it was. It exists because false
	// positives (spurious UNKNOWN) are worse than false negatives
	// (staying EXECUTING too long) — PRD.md §7.
	StaleFloor time.Duration

	// StaleMinSamples is how many intervals a session must have produced
	// before its cadence is trusted enough to judge silence against.
	StaleMinSamples int

	// MaxIntervals bounds the retained cadence history per session.
	MaxIntervals int

	// MaxSeenEventIDs bounds the per-session dedup set (§3, duplicate
	// eventId). Oldest ids are evicted first.
	MaxSeenEventIDs int
}

// DefaultConfig is the v1 tuning. The staleness numbers are explicitly a
// first pass meant to be tuned empirically (protocol.md §7).
func DefaultConfig() Config {
	return Config{
		DoneDuration: 5 * time.Second,
		// Tuned against captured sessions, as protocol.md §7 asks for.
		// Real in-turn gaps while EXECUTING measured p50 16s, p90 31s,
		// p95 97s, with legitimate gaps observed up to 192s (a long tool
		// call) and 18min (thinking before a permission prompt). The
		// original 2min floor fired on 6 of 165 real gaps — every one a
		// false positive, showing "not sure" while the agent was working
		// perfectly well.
		//
		// §7 is explicit that false negatives (staying EXECUTING too
		// long) beat false positives, so these are deliberately generous.
		// The one clearly-abandoned session in the captures sat silent
		// for 8.5 hours, so there is a lot of room between "thinking" and
		// "gone" and no need to crowd the former.
		StaleFactor:     40,
		StaleFloor:      15 * time.Minute,
		StaleMinSamples: 5,
		MaxIntervals:    10,
		MaxSeenEventIDs: 256,
	}
}

type key struct {
	tool      string
	sessionID string
}

// Manager is the in-memory session table. Safe for concurrent use.
type Manager struct {
	cfg Config
	now func() time.Time

	mu        sync.Mutex
	sessions  map[key]*types.SessionRecord
	updatedAt time.Time
}

func New(cfg Config) *Manager {
	return NewWithClock(cfg, time.Now)
}

// NewWithClock injects the clock so tests can drive DONE expiry and
// staleness deterministically instead of sleeping.
func NewWithClock(cfg Config, now func() time.Time) *Manager {
	return &Manager{
		cfg:       cfg,
		now:       now,
		sessions:  make(map[key]*types.SessionRecord),
		updatedAt: now(),
	}
}

// Outcome describes what one Apply call did. It is returned rather than
// logged so the caller can log off the hot path, outside the lock.
type Outcome struct {
	Tool      string
	SessionID string

	From types.AgentState
	To   types.AgentState

	// Duplicate reports a repeated eventId: a no-op, not a re-applied
	// transition (protocol.md §3).
	Duplicate bool
	// Unexpected reports a (state, event) pair with no matrix row. The
	// session has moved to UNKNOWN and this is worth logging.
	Unexpected bool
	// Created reports that this event brought a new session into the
	// table.
	Created bool
	// NoHistory reports that a non-session_started event created the
	// session, so it began in UNKNOWN and its state was derived from the
	// event alone. Normal after a server restart (PRD.md §5).
	NoHistory bool
	// Removed reports that the session left the table immediately.
	Removed bool
	// RemovalScheduled reports session_ended on an urgent state: the
	// state stays visible for DoneDuration first (protocol.md §6).
	RemovalScheduled bool
	// Ignored reports an event for a session the server does not track
	// and will not create (session_ended for an unknown session).
	Ignored bool
}

// Apply feeds one validated normalized event through the state machine.
func (m *Manager) Apply(ev types.NormalizedEvent) Outcome {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	m.reconcile(now)

	k := key{tool: ev.Source, sessionID: ev.SessionID}
	out := Outcome{Tool: ev.Source, SessionID: ev.SessionID}

	rec, ok := m.sessions[k]
	if ok && m.seen(rec, ev.EventID) {
		out.Duplicate = true
		out.From, out.To = rec.State, rec.State
		return out
	}

	if !ok {
		if ev.Event == types.EventSessionEnded {
			// Nothing to end. Creating a session just to remove it would
			// briefly publish a state the server never observed.
			out.Ignored = true
			return out
		}
		if ev.Event == types.EventSessionStarted {
			// "(none) | session_started | IDLE" — creation *is* the
			// transition, so this does not go through the matrix (which
			// has no IDLE + session_started row, by design).
			rec = m.create(k, types.StateIdle, now)
			m.markSeen(rec, ev.EventID)
			m.observe(rec, now)
			m.updatedAt = now
			out.Created = true
			out.To = types.StateIdle
			return out
		}
		// No history for this session, so the last known state is by
		// definition untrusted: start in UNKNOWN and let the UNKNOWN row
		// of §3 derive the state from this event alone.
		rec = m.create(k, types.StateUnknown, now)
		out.Created = true
		out.NoHistory = true
	}

	out.From = rec.State
	m.markSeen(rec, ev.EventID)
	m.observe(rec, now)
	m.updatedAt = now

	if ev.Event == types.EventSessionEnded {
		m.end(rec, k, now, &out)
		return out
	}
	// Everything else goes through the matrix unmodified — including
	// session_started for a session already in the table, which has no
	// matrix row and so lands in UNKNOWN as an unexpected transition.
	// Left unspecial-cased so §3's "no pair is guessed at" rule holds
	// uniformly.
	res := statemachine.Next(rec.State, ev.Event)
	out.Unexpected = res.Unexpected
	m.setState(rec, res.Next, now)
	if ev.Event == types.EventSessionStarted {
		// A session declaring itself freshly started is alive, so a
		// removal scheduled by an earlier session_ended no longer
		// applies — otherwise a resumed session would be reaped moments
		// after coming back.
		rec.RemoveAt = nil
	}
	out.To = rec.State
	return out
}

// end applies protocol.md §6. Urgent states (WAITING, ERROR, UNKNOWN)
// linger for DoneDuration so an agent that errored and exited does not
// make the error vanish before it is seen; everything else is removed at
// once.
func (m *Manager) end(rec *types.SessionRecord, k key, now time.Time, out *Outcome) {
	out.To = rec.State
	switch rec.State {
	case types.StateWaiting, types.StateError, types.StateUnknown:
		at := now.Add(m.cfg.DoneDuration)
		rec.RemoveAt = &at
		rec.DoneExpiresAt = nil
		out.RemovalScheduled = true
	default:
		// IDLE and DONE are named in §6; EXECUTING is not covered there,
		// and is treated as non-urgent (it is not an attention state).
		// Deliberately NOT turned into ERROR — ERROR requires an explicit
		// failure event and is never inferred (PRD.md §5).
		delete(m.sessions, k)
		out.Removed = true
	}
}

// reconcile resolves everything time-based: scheduled removals, the DONE
// window, and staleness. Called before every read and every write, so
// callers never observe an expired state.
func (m *Manager) reconcile(now time.Time) {
	changed := false
	for k, rec := range m.sessions {
		if rec.RemoveAt != nil && !now.Before(*rec.RemoveAt) {
			delete(m.sessions, k)
			changed = true
			continue
		}
		if rec.State == types.StateDone && rec.DoneExpiresAt != nil && !now.Before(*rec.DoneExpiresAt) {
			// Date the transition from the deadline, not from whenever the
			// table happened to be touched.
			deadline := *rec.DoneExpiresAt
			rec.DoneExpiresAt = nil
			m.setState(rec, types.StateIdle, deadline)
			changed = true
			continue
		}
		if rec.State == types.StateExecuting && m.isStale(rec, now) {
			m.setState(rec, types.StateUnknown, now)
			// Cadence history described a session that is no longer
			// behaving that way; drop it so staleness cannot re-fire and
			// so the next real event rebuilds it from scratch.
			rec.RecentIntervals = nil
			changed = true
		}
	}
	if changed {
		m.updatedAt = now
	}
}

// isStale implements protocol.md §7: silence judged against the
// session's own cadence, never a blanket timeout. A session that was
// always infrequent is not penalized for continuing to be infrequent,
// and no session goes UNKNOWN before StaleFloor.
func (m *Manager) isStale(rec *types.SessionRecord, now time.Time) bool {
	if len(rec.RecentIntervals) < m.cfg.StaleMinSamples {
		return false // not enough signal — trust the last known state
	}
	if rec.LastEventAt.IsZero() {
		return false
	}
	threshold := time.Duration(float64(median(rec.RecentIntervals)) * m.cfg.StaleFactor)
	if threshold < m.cfg.StaleFloor {
		threshold = m.cfg.StaleFloor
	}
	return now.Sub(rec.LastEventAt) > threshold
}

// Snapshot returns copies of the live sessions plus the time state last
// changed. Copies keep aggregation and the HTTP layer from touching
// mutable server-internal records.
func (m *Manager) Snapshot() ([]types.SessionRecord, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	m.reconcile(now)

	out := make([]types.SessionRecord, 0, len(m.sessions))
	for _, rec := range m.sessions {
		out = append(out, types.SessionRecord{
			Tool:      rec.Tool,
			SessionID: rec.SessionID,
			State:     rec.State,
			Since:     rec.Since,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tool != out[j].Tool {
			return out[i].Tool < out[j].Tool
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out, m.updatedAt
}

func (m *Manager) create(k key, state types.AgentState, now time.Time) *types.SessionRecord {
	rec := &types.SessionRecord{
		Tool:         k.tool,
		SessionID:    k.sessionID,
		State:        state,
		Since:        now,
		SeenEventIDs: make(map[string]struct{}),
	}
	m.sessions[k] = rec
	return rec
}

func (m *Manager) setState(rec *types.SessionRecord, next types.AgentState, at time.Time) {
	if rec.State != next {
		rec.State = next
		rec.Since = at
	}
	if next == types.StateDone {
		// Re-entering DONE restarts the window; the clock runs from server
		// receive time, not the hook's timestamp (protocol.md §5).
		deadline := at.Add(m.cfg.DoneDuration)
		rec.DoneExpiresAt = &deadline
	} else {
		rec.DoneExpiresAt = nil
	}
}

func (m *Manager) seen(rec *types.SessionRecord, eventID string) bool {
	_, ok := rec.SeenEventIDs[eventID]
	return ok
}

func (m *Manager) markSeen(rec *types.SessionRecord, eventID string) {
	if rec.SeenEventIDs == nil {
		rec.SeenEventIDs = make(map[string]struct{})
	}
	rec.SeenEventIDs[eventID] = struct{}{}
	rec.SeenEventIDFIFO = append(rec.SeenEventIDFIFO, eventID)
	if len(rec.SeenEventIDFIFO) > m.cfg.MaxSeenEventIDs {
		drop := rec.SeenEventIDFIFO[0]
		rec.SeenEventIDFIFO = rec.SeenEventIDFIFO[1:]
		delete(rec.SeenEventIDs, drop)
	}
}

// observe records the arrival for cadence purposes. LastEventAt is the
// server's receivedAt, which is authoritative for ordering (PRD.md §5).
func (m *Manager) observe(rec *types.SessionRecord, now time.Time) {
	if !rec.LastEventAt.IsZero() {
		if d := now.Sub(rec.LastEventAt); d > 0 {
			rec.RecentIntervals = append(rec.RecentIntervals, d)
			if len(rec.RecentIntervals) > m.cfg.MaxIntervals {
				rec.RecentIntervals = rec.RecentIntervals[len(rec.RecentIntervals)-m.cfg.MaxIntervals:]
			}
		}
	}
	rec.LastEventAt = now
}

func median(d []time.Duration) time.Duration {
	sorted := make([]time.Duration, len(d))
	copy(sorted, d)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
