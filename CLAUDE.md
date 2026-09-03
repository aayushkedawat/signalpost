# CLAUDE.md — Traffic Light for Claude Code / Copilot CLI

This file orients Claude Code on this repo. Read `docs/PRD.md` and
`docs/protocol.md` before writing code — they contain product and
protocol decisions that are **frozen** (two rounds of technical review
already happened). Don't relitigate them. If something in them looks
wrong once you're implementing it, say so explicitly and ask — don't
silently change the behavior.

## What this is

An ambient status indicator for Claude Code / Copilot CLI: red
(waiting for permission/input) / yellow (executing) / green (done,
briefly) / gray (idle), surfaced on desktop, terminal, and eventually
a watch. This repo currently only builds the **server** — the piece
everything else depends on. Do not build desktop/terminal/watch clients
until told to; see build order below.

## Non-negotiables (do not change without asking)

- **The server is the sole authority over state.** Nothing else ever
  sets a state directly — state is always derived from a sequence of
  normalized events via the transition matrix in `docs/protocol.md` §3.
- **Fail-open, always.** A hook calling this server must never be able
  to block or slow down Claude Code / Copilot CLI. If the server errors
  or a call fails, the hook side (not our concern in this repo, but keep
  the server itself always fast) should be assumed to swallow the error.
  Server-side: no operation on the hot path (`POST /events`) may block on
  disk, network, or anything slow.
- **WAITING comes only from explicit events**, never inferred from a
  timeout or silence. Don't "helpfully" add a timeout-based fallback.
- **In-memory state only.** No database in v1. Server restart = sessions
  reset. This is intentional, not a shortcut to fix later.
- **No scope creep.** No chat, no approval-from-client, no token/cost
  tracking, no transcripts, no multi-user, no cloud/remote access. If a
  task seems to need one of these, stop and flag it rather than adding it.

## Build order (current phase: **2**)

1. **Server + state machine + simulator + minimal terminal output** (done)
2. Claude Code hook integration (adapter), tested against real hook
   payloads ← **we are here** (adapter + hook binary done and validated
   against captured payloads; remaining: run it against a live session)
3. Copilot CLI hook integration (adapter)
4. Desktop app (Flutter, menu bar/tray)
5. Terminal app polish (Dart CLI)
6. watchOS app (Swift, Simulator-only initially)
7. Hardening / hardware (later, separate tracks)

Only do the current phase unless explicitly told otherwise.

## Phase 1 scope — what "done" looks like

- [x] `server/` — Go, implementing exactly the API in
      `docs/protocol.md` §10 (`POST /events`, `GET /state`, `GET /health`)
- [x] State machine implements the transition matrix in `docs/protocol.md`
      §3 exactly — every row should have a corresponding test
- [x] Per-tool and global aggregation per `docs/protocol.md` §9
      (`WAITING > ERROR > UNKNOWN > EXECUTING > DONE > IDLE`)
- [x] DONE ephemeral timeout (§5) and session-ends-while-urgent behavior
      (§6) both implemented and tested
- [x] Staleness → UNKNOWN heuristic (§7) — implement the simple
      cadence-relative version described there; don't over-build this,
      a first-pass heuristic is fine, it's explicitly meant to be tuned
      later
- [x] Bearer token auth on `/events` and `/state` (not `/health`); token
      generated on first run, written to a local file with `0600`
      permissions, never logged or echoed back in any response
- [x] `tools/simulator/` — a small CLI that POSTs synthetic normalized
      events to `/events` (e.g. `traffic-light simulate task_started
      --tool claude --session s1`), so the whole pipeline is testable
      without a real Claude/Copilot session
- [x] `apps/cli/` — minimal terminal output: poll `GET /state` and print
      current state per tool. This is intentionally basic in phase 1 —
      not the polished TUI described in the PRD, just enough to watch
      state changes happen live while testing
- [x] Test coverage for the transition matrix, including: normal flow,
      permission flow, error flow, multiple sessions per tool,
      cross-tool aggregation, duplicate `eventId`, and out-of-order
      events not causing incorrect regressions

## What's already scaffolded

- `docs/PRD.md` — full product spec (frozen v4)
- `docs/protocol.md` — the authoritative transition matrix, event
  envelope, and API contract. **This is the spec to implement against.**
- `server/go.mod` — module `trafficlight`, Go 1.23
- `server/internal/types/types.go` — core types already defined
  (`AgentState`, `NormalizedEvent`, `SessionRecord`, `ToolStatus`,
  `OverallStatus`, `StateResponse`, `HealthResponse`) — reuse these,
  don't redefine. **Not yet compiled/verified in this environment (no Go
  toolchain available where this was drafted) — run `go build ./...`
  first thing and fix anything that doesn't compile cleanly.**

## Conventions

- Standard library only for the HTTP layer (`net/http`) — don't pull in
  a router/framework (chi, gin, echo, etc.) unless there's a concrete
  reason; this is small enough that stdlib is sufficient
- `gofmt`/`go vet` clean at all times; run both before considering any
  piece done
- Package layout: `internal/` for everything not meant to be imported by
  other modules (this is a standalone server, not a library — almost
  everything belongs under `internal/`). Suggested packages alongside
  `internal/types`: `internal/events` (normalization/validation),
  `internal/sessions` (session manager, dedup, staleness), `internal/
  statemachine` (transition table), `internal/aggregation`, `internal/
  api` (HTTP handlers). Adjust if a cleaner split becomes obvious once
  you're actually writing the logic — this is a suggestion, not a
  mandate.
- Keep vendor-specific knowledge (Claude/Copilot hook event names) out
  of the state machine entirely — that's what adapters (phase 2/3) are
  for. Phase 1 has no real adapters yet; the simulator bypasses them by
  posting already-normalized events directly.
- No premature abstraction — this is described in the PRD as "avoid
  over-engineering v1" repeatedly; take that literally
- Errors: return explicit errors, don't panic in request-handling paths
  (panicking on a malformed hook payload would violate fail-open at the
  process level, not just the logical level)

## Running things

Three separate Go modules tied together by a `go.work` at the repo root,
so `go build ./server/...` and friends work from anywhere in the tree.
The simulator and CLI speak only HTTP/JSON — they deliberately do not
import the server's packages, which is the same "clients are dumb and
vendor-agnostic" boundary the Flutter/Swift clients will sit behind.

Phase 2 adds the Claude Code hook. Adapters run **hook-side**, not in the
server: PRD §9 limits what crosses the hook→server boundary to five
fields, and a server-side adapter would require raw payloads (prompts,
file contents, tool arguments) to be transmitted first. See
`docs/architecture.md`, "Adapters and the hook boundary".

```bash
# build the hook and point Claude Code at it
go build -o ~/bin/traffic-light-hook ./server/cmd/traffic-light-hook
cp tools/hook-settings.example.json .claude/settings.json   # edit paths first
# then restart Claude Code — the settings watcher only watches directories
# that already had a settings file when the session started

# debugging the hook (silent by default: stdout is parsed by Claude Code)
TRAFFIC_LIGHT_HOOK_DEBUG=1 ~/bin/traffic-light-hook < some-payload.json
```

`tools/capture/` holds the throwaway instrumentation used to capture real
payloads, plus `summarize.py` to analyze them. Delete it once the mapping
is settled. `capture/` is gitignored — payloads contain real prompts and
code, which PRD §9 keeps out of this repo.

```bash
# start the server (generates ~/.traffic-light/token 0600 on first run)
go run ./server

# watch state changes live, in another pane
go run ./apps/cli

# drive it, in a third
go run ./tools/simulator --list                      # events + scenarios
go run ./tools/simulator --scenario permission       # a full permission flow
go run ./tools/simulator task_started --tool claude --session s1
```

Useful server flags: `-addr` (defaults to loopback; LAN is an explicit
opt-in), `-done-duration`, `-waiting-too-long`, `-stale-floor`,
`-stale-factor`. The last two exist because protocol.md §7 says the
staleness heuristic is meant to be tuned empirically — lowering
`-stale-floor` to a few seconds makes it observable without waiting
minutes.

Checks, all currently clean:

```bash
gofmt -l . && go vet ./server/... ./tools/simulator/... ./apps/cli/...
go test -race ./server/... ./tools/simulator/... ./apps/cli/...   # 101 funcs / 208 cases
```
