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
| WAITING | task_started | EXECUTING |
| EXECUTING | permission_granted | EXECUTING |
| WAITING | task_failed | ERROR |
| ERROR | task_started | EXECUTING |
| ERROR | permission_granted | EXECUTING |
| ERROR | permission_requested | WAITING |
| EXECUTING | task_started | EXECUTING |
| WAITING | permission_requested | WAITING |
| WAITING | task_completed | DONE |
| DONE | task_started | EXECUTING |
| any | session_started | IDLE |
| UNKNOWN | any valid lifecycle event | derived per this table, from the event alone |
| any | duplicate `eventId` | unchanged (no-op) |
| any | session_ended | see §6 (session lifecycle) |
| any | staleness detected (internal, not an event) | UNKNOWN (see §7) |

Any current/event pair not listed above is logged as an unexpected
transition and the session moves to UNKNOWN rather than silently
ignored or guessed at.

Five further rows were added in phase 4, after a user reported the light
turning cyan ("not sure") during ordinary work. Replaying the captured
sessions through the table found seven transitions falling off it:

- `ERROR + permission_granted` and `ERROR + permission_requested` —
  **ERROR is not a terminal state.** A command exits non-zero and the
  next one succeeds or asks for approval. This was the most frequent
  cause: any failed tool call poisoned the rest of the turn.
- `EXECUTING + task_started` — a second prompt while the agent is still
  working (the user queues a message, or interrupts and retypes).
- `WAITING + permission_requested` — two prompts back to back.
- `WAITING + task_completed` — the turn ends while a prompt is
  outstanding, e.g. a denial that stopped the agent.

Each of these was worse than an ordinary wrong answer, because UNKNOWN
outranks EXECUTING in §9 aggregation: one stray transition hijacked the
headline colour for the whole system.

The pattern is worth noting for §3 as a whole. This table was written as
"the transitions we expect" and has now needed three rounds of additions,
each found only by running it against reality. Every addition so far has
been semantically obvious in hindsight. It may be worth asking whether
unlisted pairs should derive from the event alone — the UNKNOWN row's own
rule — rather than falling to UNKNOWN, which would make the whole class
of defect impossible. That is a real design change and is not made here.

The two rows marked EXECUTING above were added in phase 2 from captured
Claude Code sequences, not from reasoning:

- `EXECUTING + permission_granted` — `PostToolUse` fires after *every*
  tool call, not only after an approval, so `permission_granted` arrives
  routinely while already EXECUTING. Without this row ordinary tool use
  falls off the matrix into UNKNOWN. It is idempotent: it confirms
  EXECUTING and never leaves it.
- `WAITING + task_started` — denying a permission prompt emits no
  `PermissionDenied` hook and no `PostToolUse`. The only signal that the
  prompt was answered is the user's next prompt, which normalizes to
  `task_started` and arrives while the session is WAITING.

  This row replaced an earlier deliberate choice to leave the pair
  unlisted, so that a late `task_started` could not silently downgrade
  WAITING to EXECUTING and hide the fact that the agent needed
  attention. Real captures made the opposite risk larger: without the
  row, every denial leaves the light stuck red indefinitely, and a red
  that outlives its cause is precisely what teaches a developer to stop
  trusting red. The trade is narrow — hooks arrive in order over
  loopback HTTP, so a genuinely out-of-order `task_started` is unlikely,
  while denials are routine.

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

**Tuned (phase 4):** `StaleFloor` 15min, `StaleFactor` 40,
`StaleMinSamples` 5. Measured against captured sessions, in-turn gaps
while EXECUTING ran p50 16s, p90 31s, p95 97s, with legitimate gaps up to
192s and one of 18 minutes spent thinking before a permission prompt. The
original 2min floor fired on 6 of 165 real gaps and every one was a false
positive. The only genuinely abandoned session in the captures had been
silent for 8.5 hours, so there is a wide margin between "thinking" and
"gone" and no reason to crowd the former.

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

**Validated** against payloads captured from real Claude Code sessions
(see `tools/capture/`). Every mapping is a pure function of one payload —
no adapter-side memory — which is what lets adapters run hook-side.

| Claude Code hook | Normalized event |
|---|---|
| SessionStart | session_started |
| UserPromptSubmit | task_started |
| Notification (`notification_type: permission_prompt`) | permission_requested |
| PostToolUse | permission_granted |
| PostToolUseFailure | task_failed |
| Stop | task_completed |
| SessionEnd | session_ended |
| PreToolUse | *(ignored)* |

Notes from the capture, correcting the original best-effort table:

- **`Notification` is the WAITING source, and the original table had it
  right.** An intermediate revision of this document claimed it "never
  fires" and that it "carries no `session_id`". Both were false. The
  first came from a capture window too short to contain one — it only
  fires after a prompt has gone unanswered for 6 seconds. The second came
  from a hand-written test fixture rather than a real payload, which is
  the exact failure this section's real-payload fixtures exist to
  prevent. See the corrected row above.
- **`UserPromptSubmit`, not `PreToolUse`, is `task_started`.** The
  original "PreToolUse (first in a task)" required adapter memory to
  identify "first", and broke outright on a turn with no tool calls: a
  captured `SessionStart -> UserPromptSubmit -> Stop` session (the user
  typed "hi") emitted no `task_started` at all, so `task_completed`
  arrived at an IDLE session and landed in UNKNOWN.
- **`PreToolUse` is ignored.** `UserPromptSubmit` already establishes
  EXECUTING at the start of the turn, earlier than the first tool call,
  so `PreToolUse` adds no state. `PostToolUse` still provides a
  per-tool-call heartbeat for the §7 staleness cadence.
- **`PostToolUseFailure` exists** and fires on an ordinary non-zero exit
  (verified deliberately). The original table had no failure signal at
  all and speculated one might not exist; ERROR is reachable for Claude
  Code.
- **A denial is invisible except as an absence.** `PermissionDenied` did
  not fire on a real denial. The observed sequence is
  `PreToolUse -> PermissionRequest -> UserPromptSubmit` with no
  `PostToolUse`, versus `... -> PostToolUse` when approved.
- **`PermissionRequest` follows `PreToolUse`**, rather than preceding the
  tool call as the original table's ordering implied.

Field notes: every hook carries `session_id`, `cwd`, `transcript_path`
and `hook_event_name`. `SessionStart` carries `source` (observed:
`startup`, `resume`), `SessionEnd` carries `reason`, and the in-turn
hooks carry `prompt_id`, which identifies a single turn and may be a
better basis for task identity than inference if that is ever needed.

> **Corrected after running against a live session.** This row is the
> one the captures got wrong twice, in opposite directions.
>
> The original draft mapped `Notification` → `permission_requested`. An
> early capture window contained no `Notification` at all, so it was
> recorded here as "never fires" and replaced with `PermissionRequest`.
> That was absence of evidence: `Notification` fires only once a prompt
> has gone unanswered for a few seconds, which no short capture had hit.
>
> `PermissionRequest` then turned out to be the wrong signal. It fires
> when the permission system is *consulted* — 7 times in 59 tool calls in
> the captures — and is resolved by an auto-approving classifier as often
> as by a person. Nothing in its payload separates the two, because at
> the moment it fires Claude Code has not decided yet. Mapping it to
> WAITING put the light in "needs you" while an agent worked unattended.
>
> `Notification` carries `notification_type`, which states the case
> outright:
>
> | `notification_type` | meaning | mapped |
> |---|---|---|
> | `permission_prompt` | "Claude needs your permission to use Bash" | ✅ permission_requested |
> | `idle_prompt` | "Claude is waiting for your input" | ❌ ignored |
>
> `idle_prompt` is excluded deliberately: it fires after a turn ends,
> which `Stop` has already reported as DONE, so mapping it would turn
> every completed turn red instead of green.
>
> **The threshold is exactly 6 seconds.** Across 12 captured pairs the
> delay from `PermissionRequest` to its `Notification` was 5.99–6.02s,
> so this is a fixed timer in Claude Code rather than an approximation.
> The practical rule: a prompt answered within 6 seconds never turns the
> light red; one that outlives 6 seconds does.
>
> **The 6 seconds cannot be shortened from our side**, which is worth
> recording because the shortcut is tempting. `PermissionRequest` fires
> immediately, so a short timer of our own looks like an obvious
> improvement. It is not: `PostToolUse` arrives when the tool *finishes*,
> not when permission is granted, so an auto-approved command is
> indistinguishable from a pending human prompt until either the tool
> completes or the notification fires. Measured over 26 auto-resolved
> permission requests (gap to `PostToolUse` 1.47–13.5s, median 4.04s), a
> timer of our own would flash red on 100% of them at 1s, 85% at 2s and
> 35% at 5s. Claude Code's notification knows something we cannot
> observe — whether a prompt is actually on screen — and 6s is the price
> of that knowledge.
>
> End to end the delay is 6.0s plus the client poll interval; the server
> itself contributes 0.5ms. The desktop app polls every 300ms for that
> reason: polling is the only part of the delay we own, so it should not
> be the part anyone notices.
>
> That is the right boundary for an ambient signal — it will not flash
> for a prompt already being dealt with — but it is a false negative, and
> it is Claude Code's number, not ours. It could change in any release
> without notice, and nothing here would fail loudly if it did: the light
> would simply stop going red for short prompts. PRD §7 prefers that to a
> red that does not mean "needs you", but it is worth re-checking against
> captures if WAITING ever seems to under-report.
>
> Measured over the same captures, mapping `PermissionRequest` instead
> would have been wrong **52% of the time** (13 of 25 were auto-resolved
> with no human involved).
>
> `SubagentStop` is also left unmapped — a subagent finishing says
> nothing about the main agent, which is usually still working.

A bare permission denial — tapping "No" without typing a reason — emits
nothing at all: no `PermissionDenied`, no `PostToolUse`, no `Stop`. The
session therefore stays WAITING until the next `UserPromptSubmit`, which
is defensible, since after a rejection the agent genuinely is awaiting
direction. Note that §9 aggregation lets one such session pin the global
light red and mask every other tool until that session is touched or
quit.

Still unobserved: `StopFailure` and `PermissionDenied`. The latter is
suspected to cover rule-based denials rather than user rejections.

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
