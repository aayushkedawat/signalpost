# PRD — Traffic Light for Claude Code / Copilot CLI

**Status:** Draft v4 — frozen for implementation
**Owner:** Aayush
**Last updated:** 2026-09-03

---

## 1. Problem

Agentic coding tools (Claude Code, Copilot CLI) run autonomously, but their
state is buried in a terminal window. The failure mode that actually costs
time isn't the agent doing something wrong — it's **the developer not
noticing the agent needs them** (a permission prompt, an error) because
they've mentally moved on.

This is an **attention-management problem**, not an observability problem.
The most valuable state to surface isn't "agent is running" — it's
**"agent needs me."**

## 2. Goal

An always-visible, glanceable signal — red (waiting) / yellow (executing)
/ green (done, briefly) / gray (idle) — per tool, per surface (desktop,
terminal, watch), that's accurate enough to trust without double-checking
the terminal.

**Product success metric:** time between an agent entering WAITING and the
developer noticing — not "clients update within N seconds" (that's a
technical metric in service of this one).

## 3. Non-goals

- No chat, tool approval, or transcript viewing from any client (v1) —
  display only, all clients are read-only
- No multi-user, remote, or cloud access — local/LAN only, single developer
- No cost/token analytics
- Not hardcoded to exactly two tools at the protocol level, but only
  Claude/Copilot are integrated in v1

## 4. Architecture

```
Claude Code hooks ──┐
                     ├──▶ POST /events ──▶ Event Normalizer ──▶ Session
Copilot CLI hooks ──┘                                            Manager
                                                                     │
                                                                     ▼
                                                              State Machine
                                                                     │
                                                                     ▼
                                                               Aggregator
                                                                     │
                                          ┌──────────────────────────┼──────────────────────────┐
                                          ▼                          ▼                          ▼
                                    Desktop (Flutter)         Terminal (Dart CLI)         watchOS (SwiftUI)
                                          └──────────────────────────┴──────────────────────────┘
                                                          all read from GET /state
```

Clients never talk to Claude/Copilot directly and never understand vendor
hook semantics — they only understand `GET /state`. This is what makes
adding a hardware client later, or a third agent (Codex, Cursor) later,
additive rather than a rearchitecture.

**The server is the sole authority over state.** No client or hook ever
sets a state directly — every state value is derived by the server's
state machine from a sequence of normalized events. This is what makes
the aggregation, staleness, and ordering rules below meaningful: they're
rules the server enforces once, not conventions every client has to
respect independently.

## 5. State model

```
IDLE
    No active agent work.

EXECUTING
    Agent is actively processing a task or tool call.

WAITING
    Agent has explicitly requested user input, permission, or
    another interaction that requires developer attention.

DONE
    Agent has successfully completed its current task.
    Ephemeral; transitions to IDLE after 5 seconds.

ERROR
    Agent or its current task terminated unsuccessfully or encountered
    an error requiring developer attention.

UNKNOWN
    The server cannot confidently determine the current session state.
    This may occur due to stale sessions, missing lifecycle events,
    server restart, or inconsistent event sequences.
```

- **WAITING is set only from explicit semantic events** — a
  permission-request / needs-input lifecycle event — never inferred from
  a timeout, absence of events, or elapsed time. A 30s threshold only
  flips `waitingTooLong: true` for visual urgency (solid red → pulsing
  red); it plays no role in *deciding* WAITING in the first place.
- **ERROR is distinct from DONE** — a failed tool/agent run must not look
  like a successful one. Transition: any session in EXECUTING or WAITING
  moves to ERROR only on an explicit failure event (tool failure, agent
  crash signal, non-zero exit reported by the hook) — never inferred.
- **UNKNOWN is a confidence state, not a failure state** — it means "the
  server no longer trusts its last known state for this session," reached
  via staleness (see §7), a server restart with no session history, or an
  event arriving that doesn't fit the session's current state (out-of-
  order/inconsistent sequence — logged, not guessed at). A session in
  UNKNOWN returns to a confident state only on the next real event for
  that session; it does not auto-resolve on its own.
- **DONE is ephemeral** — auto-reverts to IDLE after `doneDuration`
  (default 5s, configurable). The clock starts from **server receive
  time**, not the hook's `timestamp`, since the server controls the
  state lifecycle.

### Event transition matrix

| Current | Event | Next |
|---|---|---|
| IDLE | task_started | EXECUTING |
| EXECUTING | permission_requested | WAITING |
| EXECUTING | task_completed | DONE |
| EXECUTING | task_failed | ERROR |
| WAITING | permission_granted / input_received | EXECUTING |
| WAITING | task_failed | ERROR |
| ERROR | task_started | EXECUTING |
| DONE | task_started | EXECUTING |
| UNKNOWN | any valid lifecycle event | derived per this table |
| any | duplicate `eventId` | unchanged (no-op) |

This table is the authoritative reference during implementation — it
belongs in `docs/protocol.md`, not just here.

### Session lifecycle

- `session_started` → session exists, state IDLE.
- `session_ended` while IDLE/DONE → session removed immediately.
- `session_ended` while WAITING/ERROR/UNKNOWN → **state remains visible
  briefly** (same ephemeral window as DONE, `doneDuration`) rather than
  vanishing instantly — an agent that errored out and closed shouldn't
  make the error disappear before you see it.

### Timestamps and event ordering

All timestamps are ISO 8601 UTC (`2026-09-03T10:42:31Z`). Every event
carries a **hook-generated** `eventId` (UUID/ULID) plus the hook-supplied
`timestamp`. The server additionally records its own `receivedAt` the
moment the event arrives. These three serve distinct purposes:

- `eventId` — deduplication identity only (not ordering — a UUID has no
  inherent order). A repeated `eventId` (e.g. a hook retry after a
  fail-open timeout that actually succeeded) is a no-op, not a
  re-applied transition.
- `receivedAt` (server clock) — authoritative for server-side processing
  order, since it isn't subject to client clock drift.
- `timestamp` (hook-supplied) — informational only: when the hook
  believes the event occurred, not used for ordering.

For v1, ordering = `receivedAt` + `eventId` dedup. If a vendor exposes a
real sequence number later, that becomes the stronger ordering signal.

### Per-tool aggregation (multiple sessions)

A tool can have several concurrent sessions (tmux, multiple windows). The
server tracks sessions individually and aggregates to one status per tool
by priority:

```
WAITING > ERROR > UNKNOWN > EXECUTING > DONE > IDLE
```

One Claude session waiting + another executing → Claude shows WAITING.
UNKNOWN outranks EXECUTING/DONE/IDLE because "I'm not sure" is more
worth surfacing than a possibly-stale "it's fine."

### Combined desktop icon / overall state

No blended/combined color when Claude and Copilot disagree. The same
priority order applies globally across tools, and is exposed directly as
an `overall` field in `/state` (see §6) so every client computes the
headline color identically instead of re-deriving it from the per-tool
map. Tray shows the single most urgent color; clicking reveals both tools
individually.

## 6. API

```
POST /events   — hooks push normalized lifecycle events
GET  /state    — clients read current aggregated state
GET  /health   — {"status": "ok", "version": "0.1.0", "uptimeSeconds": N}
                 — deliberately minimal; LAN reachability is a
                 client-side question ("can I reach the server?"), not
                 something the server can meaningfully self-report
```

`POST /events` body:
```json
{
  "eventId": "evt_9f2a1c",
  "source": "claude",
  "sessionId": "abc123",
  "event": "permission_requested",
  "timestamp": "2026-09-03T10:42:31Z"
}
```
`eventId` is generated by the hook command (a UUID is fine) and is what
makes the call idempotent — if a fail-open retry double-sends, the second
copy is a no-op rather than a re-applied transition.

`GET /state` response:
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
`overall.state` is the headline color; `overall.tool` names which tool
caused it — lets the desktop tray show "🔴 Claude needs you" directly
without recomputing anything, while the popover still lists both tools.

State is **derived from events server-side**, not set directly by clients
— clients/hooks never `POST /state` with an arbitrary state string. This
keeps state-transition logic in one place.

## 7. Reliability requirements

- **Fail-open, always.** If the server is down or slow, the hook call
  must fail silently and let Claude/Copilot continue normally. The
  Traffic Light must never be able to block an agent. Hook HTTP calls use
  a short timeout and swallow errors.
- Event processing target: low-double-digit ms locally (no DB writes, no
  network calls, no logging pipeline on the hot path).
- In-memory state only for v1 — no database. Server restart → all
  sessions reset to UNKNOWN/IDLE, which is acceptable since this is a
  live-state tool, not a historical log.
- **Stale detection is conservative, not a blanket timeout.** A session
  sitting in EXECUTING for 10+ minutes is normal (builds, large codegen,
  package installs) and must not flip to UNKNOWN just for being long-
  running. Staleness is judged by *silence relative to the session's own
  event cadence*, not a fixed global clock — e.g. a session that's been
  emitting periodic tool-use events every few seconds and then goes fully
  silent for several minutes is a stronger stale signal than one that was
  already infrequent. The exact heuristic is a v1 implementation detail
  to tune empirically, but the requirement is: **default to trusting the
  last known state; only move to UNKNOWN when silence is unusual for that
  session, not merely long.**

## 8. Security

Once the watch needs LAN access, the server can't stay `127.0.0.1`-only.
Minimal version of the review's recommendation: a random token generated
on first run, written to a local file (permissions `0600`, user-readable
only), read by every client and by the hook commands (`Authorization:
Bearer <token>`). The token is never logged, never included in
diagnostics, and never exposed via `/state` or `/health`. No
rotation/expiry system — this is a personal LAN tool, not a multi-tenant
service. Default stays localhost-only; LAN mode is an explicit opt-in.

## 9. Privacy

The server stores only what it needs to compute state — never agent
content. Explicitly **never persisted or transmitted**: prompts, agent
responses, tool output/arguments, file contents, code, credentials. The
event payload is limited to `eventId`, `source`, `sessionId`, `event`
type, and `timestamp` — nothing else crosses the hook → server boundary.
This should be enforced at the schema level (reject/strip unexpected
fields), not just as a convention hooks are expected to follow.

## 10. Feature scope by surface

Unchanged from v1's split — desktop (Flutter, menu bar/tray via
`tray_manager`, click for per-tool detail popover), terminal (Dart CLI,
compact TUI panel for a tmux pane), watchOS (native Swift/SwiftUI,
Simulator-first, polls `GET /state` over LAN). All three are pure display
— no interactivity — and all three must show state via **text/label, not
color alone** (accessibility: color-blindness, terminal theme variance).

## 11. Shared Dart core (`traffic_light_core`)

Used by desktop + terminal only (watch is standalone Swift). Deliberately
thin — **all state-machine, aggregation, and staleness logic lives
server-side only** (§4's "server is sole authority" applies here too):
```
traffic_light_core/
├── models/      (tool, state, event, session — data shapes only)
└── api/         (client for GET /state)
```
Vendor-specific hook parsing (Claude adapter, Copilot adapter) lives in
the server, not in this shared package — clients stay vendor-agnostic
and dumb-by-design.

## 12. What's explicitly out of v1

Chat/approval from any client, token/cost tracking, transcript viewer,
remote/cloud access, multi-user, SSE (polling at ~1s is sufficient at this
scale — revisit only if latency/battery become real issues), a formal
adapter framework for agents beyond Claude/Copilot (design the protocol to
allow it, don't build it speculatively), the physical hardware device
(separate future track, contingent on validating demand from the free
software first).

## 13. Build order

1. **Server + Claude integration + terminal output** — prove the
   event→state mapping is reliable before building any client UI on top
   of it. This is the highest-risk piece, so it goes first, not last.
   Build the **fake event simulator** (below) alongside this, not after —
   it's how every subsequent step gets tested without needing a live
   Claude/Copilot session running.
2. Add Copilot integration to the same server — confirm both vendors map
   cleanly onto the shared normalized state machine
3. Desktop app (Flutter)
4. Terminal app polish (Dart CLI)
5. watchOS app (Swift, Simulator-only initially)
6. Hardening (stale detection tuning, setup diagnostics)
7. Hardware (future, contingent on demand)

### Fake event simulator

A small CLI (`traffic-light simulate <event> --tool claude`) that POSTs
synthetic events straight to `/events`, so the full flow — event → state
machine → aggregation → every client — can be exercised without Claude
Code or Copilot CLI actually running. This is dev/test tooling, not a
shipped feature, but it's what makes steps 3–5 buildable/testable in
isolation rather than requiring a real agent session for every test pass.

## 14. Repository layout

```
traffic-light/
├── packages/traffic_light_core/   (models/, api/ only — see §11)
├── server/traffic_light_server/
│   ├── adapters/ (claude/, copilot/)
│   ├── events/ sessions/ state/ aggregation/ api/
├── apps/ (desktop/, cli/, watch/)
├── tools/simulator/
├── tests/ (fixtures/, integration/, state_machine/)
└── docs/ (protocol.md, architecture.md)
```

`docs/protocol.md` is the next artifact — not another PRD revision. It
should contain the transition matrix (§5), normalized event definitions,
and the Claude/Copilot event mappings, as the direct precursor to writing
the server.

## 15. Open questions

- Concrete stale-detection heuristic (cadence-relative, per §7) needs an
  actual implementation and some empirical tuning — the requirement is
  fixed, the algorithm isn't yet.
- Normalized event names beyond what's in the transition matrix
  (`session_started`, `session_ended`) — finalized during Claude/Copilot
  adapter implementation, not before, since they should be validated
  against real hook payloads rather than guessed upfront.
