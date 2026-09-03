# Architecture — as built

Referenced by PRD §14. This describes what phase 1 actually produced, and
where it deviates from the PRD's sketch. `docs/protocol.md` remains the
authoritative contract; this document is orientation, not specification.

## Modules

Three Go modules, joined by a `go.work` at the repo root:

```
server/          module trafficlight        — the state authority
tools/simulator/ module .../tools/simulator — synthetic event source (dev/test)
apps/cli/        module .../apps/cli        — minimal terminal output
```

The simulator and CLI **do not import the server's packages**. They speak
only HTTP and JSON, and each keeps its own small copy of the response
shape. That is deliberate: it is the same boundary the Flutter desktop
app and the watchOS app will sit behind, so keeping it honest from the
start means those clients are not a new kind of integration later. It
also means a client cannot accidentally acquire state-machine logic,
which PRD §11 explicitly forbids.

## Server packages

Data flows in one direction; each package below depends only on the ones
after it, plus `types`.

```
POST /events
    │
    ▼
internal/api           HTTP layer, stdlib net/http only. Auth, body
    │                  limits, JSON encoding. Logs off the hot path.
    ▼
internal/events        Envelope decode + validation. Rejects unknown
    │                  fields — the enforcement point for PRD §9.
    ▼
internal/sessions      The live session table. Owns creation, dedup,
    │                  the DONE window, session_ended, and staleness.
    ▼
internal/statemachine  The transition matrix, and nothing else.
                       Pure: no clocks, no I/O, no vendor knowledge.

GET /state
    │
    ▼
internal/api ──▶ internal/sessions.Snapshot ──▶ internal/aggregation
                                                (priority collapse,
                                                 per-tool + overall)
```

Supporting: `internal/types` (shared shapes), `internal/config`
(tunables and defaults), `internal/auth` (token file + bearer middleware).

### Why the split is where it is

`statemachine` holds only the matrix so that every row in protocol.md §3
has an obvious home and an obvious test. Everything time-dependent —
the DONE window, scheduled removal, staleness — lives in `sessions`
instead, because those are session-lifecycle rules (§5–§7), not
transitions. Keeping clocks out of `statemachine` is what makes it
testable by table rather than by simulation.

### No timers

Time-based effects are resolved lazily, in `reconcile()`, whenever the
session table is touched by a read or a write. There is no background
goroutine and no ticker. Nothing observes state except through
`GET /state`, so a state that has expired but not yet been recomputed is
unobservable — and this keeps the hot path free of timer contention.

## Hot path discipline

`POST /events` does no disk I/O, no network calls, and no logging on the
accepted path. `Apply` returns an `Outcome` describing what happened, and
the HTTP layer decides what (if anything) to log **after** the session
lock is released. This is what PRD §7 means by never being the slow party:
a hook must never wait on us.

## Deviations from PRD §14

- **Layout.** PRD §14 sketches `server/traffic_light_server/` with
  `adapters/ events/ sessions/ state/ aggregation/ api/`. The actual
  layout is `server/internal/...`, per the conventions in `CLAUDE.md`,
  which are more specific and were written later. `state/` is named
  `statemachine/` because it holds the matrix, not the state.
- **CLI language.** PRD §10 specifies a Dart CLI sharing
  `traffic_light_core` with the desktop app. Phase 1's `apps/cli` is Go,
  because phase 1 is explicitly "minimal terminal output … just enough to
  watch state changes happen live while testing" and adding a second
  toolchain to get that would have been cost without benefit. The Dart
  TUI remains phase 5; this program is scaffolding for it, not it.
- **`packages/traffic_light_core/`** does not exist yet. It is only
  needed once there are two Dart clients to share it (phases 4–5).

## Where phase 2 plugs in

Adapters are the only place vendor hook names may appear. Nothing in
`statemachine`, `sessions`, or `aggregation` should learn a vendor name.

Before writing the Claude adapter, settle the boundary question recorded
in protocol.md §8: today `POST /events` accepts only the normalized
envelope and rejects everything else, which leaves no route by which a
raw vendor payload could reach a server-side adapter. Phase 1 never hits
this because the simulator posts normalized events directly.
