# Traffic Light — Protocol Specification

This is the authoritative reference for the event model, state machine,
and API contract. Implementation should follow this document exactly;
if code and doc disagree, this doc wins (update it deliberately, not
by accident).

## 1. Normalized event types

Hooks/adapters emit these — never vendor-specific event names directly
into the state machine:

```
session_started
task_started
permission_requested
permission_granted
input_received
task_completed
task_failed
session_ended
```

## 2. States

```
IDLE       No active agent work.
EXECUTING  Agent is actively processing a task or tool call.
WAITING    Agent has explicitly requested input/permission.
DONE       Task completed successfully. Ephemeral (see §5).
ERROR      Task or agent failed. Requires an explicit failure event.
UNKNOWN    Server no longer trusts its last known state for this session.
```

## 3. Transition matrix

| Current | Event | Next |
|---|---|---|
| (none) | session_started | IDLE |
| IDLE | task_started | EXECUTING |
| EXECUTING | permission_requested | WAITING |
| EXECUTING | task_completed | DONE |
| EXECUTING | task_failed | ERROR |
| WAITING | permission_granted / input_received | EXECUTING |
| WAITING | task_failed | ERROR |
| ERROR | task_started | EXECUTING |
| DONE | task_started | EXECUTING |
| any | session_started | IDLE |
| UNKNOWN | any valid lifecycle event | derived per this table, from the event alone |
| any | duplicate `eventId` | unchanged (no-op) |
| any | session_ended | see §6 (session lifecycle) |
| any | staleness detected (internal, not an event) | UNKNOWN (see §7) |

Any current/event pair not listed above is logged as an unexpected
transition and the session moves to UNKNOWN rather than silently
ignored or guessed at.

`session_started` applies from **any** state, not just from `(none)`: it
declares that the session is now fresh and idle, whatever the server
previously believed. This was settled during phase 1 — Claude Code fires
`SessionStart` on `/clear` and on resume, so a repeat for an already-
tracked session is an ordinary occurrence, and treating it as an
inconsistency would raise a false UNKNOWN during normal use. A
`session_started` arriving for a session with a pending removal (see §6)
also cancels that removal, since the session has evidently come back.

## 4. Event envelope

```json
{
  "eventId": "evt_9f2a1c",
  "source": "claude",
  "sessionId": "abc123",
  "event": "permission_requested",
  "timestamp": "2026-09-03T10:42:31Z"
}
```

- `eventId` — hook-generated (UUID/ULID), dedup identity only.
- `source` — `"claude"` or `"copilot"` (extensible string, not an enum
  the server hardcodes to exactly two values).
- `sessionId` — hook/vendor-provided session identifier.
- `event` — one of §1's normalized names. **Adapters** are responsible
  for turning vendor-specific hook payloads into this normalized shape
  before it reaches the state machine — see §8.
- `timestamp` — informational only, not used for ordering (see §5 in the
  PRD — server `receivedAt` is authoritative for ordering). Informational
  does not mean unvalidated: it is required and must parse as RFC 3339.
  Settled during phase 1 — a malformed timestamp indicates an adapter
  bug, and it is better to fail loudly while we control both sides of the
  boundary than to ship a subtly wrong adapter.

No other fields are accepted. Unexpected fields are rejected at the
schema level (privacy requirement — see PRD §9).

## 5. DONE / ephemeral state timing

`doneDuration` = 5s default, configurable. Clock starts at server
`receivedAt` for the triggering event, not the hook's `timestamp`.
After `doneDuration`, DONE → IDLE automatically (a server-internal
timer, not an event).

## 6. Session lifecycle

- `session_started` creates the session in IDLE, and resets an existing
  session to IDLE (see §3).
- `session_ended` while IDLE, DONE, or EXECUTING → session removed
  immediately.
- `session_ended` while WAITING, ERROR, or UNKNOWN → state remains
  visible for `doneDuration` before removal (an agent that errored and
  the process exited shouldn't make the error vanish instantly).

The EXECUTING case was left undefined by the original draft and was
settled during phase 1: EXECUTING is treated as non-urgent, like IDLE and
DONE. Quitting an agent mid-task is a deliberate act the developer
already knows about, so the light should go quiet rather than flash
something at them — the same "never cry wolf" discipline that makes
WAITING worth trusting. It is deliberately **not** turned into ERROR:
ERROR requires an explicit failure event and is never inferred.

`session_ended` for a session the server does not track is ignored
outright, rather than creating a session in order to remove it — the
server should never publish, even briefly, a state it never observed.

## 7. Staleness → UNKNOWN

Not a fixed timeout. Heuristic (v1, to be tuned empirically): track each
session's recent event cadence; if a session has been emitting events
regularly and then goes silent for a duration that's unusual relative to
*its own* cadence while in EXECUTING, mark UNKNOWN. A session that was
already infrequent isn't penalized for continuing to be infrequent.
Default to trusting the last known state — false negatives (staying
EXECUTING too long) are preferred over false positives (spurious UNKNOWN
alerts), per PRD §"biggest remaining risks".

## 8. Vendor adapters

Adapters live in `server/internal/adapters/<vendor>/` (the original draft
said `server/src/adapters/`, written before the server was settled as Go)
and are the only place vendor-specific hook event names are known. Each
adapter exposes a single function, `(vendorPayload) -> (NormalizedEvent,
bool)`, where the bool reports whether the payload maps to anything at
all (false = ignore, e.g. a hook event with no mapping yet). Nothing
downstream of the adapter sees vendor-specific names.

**Open for phase 2:** this section places adapters server-side, but §4
and §10 define `POST /events` as accepting the already-normalized
envelope and rejecting every other field. Both cannot be true at once.
Either the hook command normalizes and posts the envelope (adapters
become hook-side, and §8 moves), or the server grows a second
vendor-facing endpoint alongside `/events`. Phase 1 does not resolve
this: the simulator posts already-normalized events, so it never crosses
the adapter boundary. Decide this before writing the Claude adapter.

### Claude Code hook mapping

| Claude Code hook | Normalized event |
|---|---|
| SessionStart | session_started |
| PreToolUse (first in a task) | task_started |
| Notification (permission/needs-input) | permission_requested |
| PostToolUse (following a WAITING resolution) | permission_granted |
| Stop | task_completed |
| (explicit failure signal, if/when available) | task_failed |
| SessionEnd | session_ended |

### Copilot CLI hook mapping

| Copilot CLI hook | Normalized event |
|---|---|
| sessionStart | session_started |
| userPromptSubmitted | task_started |
| permissionRequest | permission_requested |
| (post-permission continuation) | permission_granted |
| agentStop | task_completed |
| errorOccurred | task_failed |
| sessionEnd | session_ended |

Both mappings are **best-effort until validated against real hook
payloads** — this is flagged as an open risk in the PRD (vendor event
semantics may not map 1:1, and exact hook availability/behavior should be
confirmed against the current CLI version during adapter implementation,
not assumed from this table alone).

## 9. Aggregation priority

Per-tool (across sessions of the same tool) and global (across tools),
same ordering:

```
WAITING > ERROR > UNKNOWN > EXECUTING > DONE > IDLE
```

## 10. API

```
POST /events   Authorization: Bearer <token>
GET  /state    Authorization: Bearer <token>
GET  /health   (no auth — trivial liveness/version check)
```

`GET /state`:
```json
{
  "version": 1,
  "updatedAt": "2026-09-03T10:42:31Z",
  "overall": {"state": "waiting", "tool": "claude"},
  "tools": {
    "claude":  {"state": "waiting", "since": "...", "activeSessions": 1, "waitingTooLong": true},
    "copilot": {"state": "executing", "since": "...", "activeSessions": 1, "waitingTooLong": false}
  }
}
```

`GET /health`:
```json
{"status": "ok", "version": "0.1.0", "uptimeSeconds": 12345}
```
