#!/usr/bin/env python3
"""Summarize captured Claude Code hook payloads.

Answers the questions docs/protocol.md §8 currently guesses at:
which hooks actually fire, in what order, with what fields, and which
of them can carry a normalized event.

Prints only hook names, field *names*, ordering and counts — never field
values, which contain prompts and code (PRD §9).

    python3 tools/capture/summarize.py [capture/raw.jsonl]
"""

import json
import sys
from collections import Counter, defaultdict

path = sys.argv[1] if len(sys.argv) > 1 else "capture/raw.jsonl"

try:
    with open(path) as fh:
        records = [json.loads(line) for line in fh if line.strip()]
except FileNotFoundError:
    sys.exit(f"no capture file at {path} — install the hooks and run a session first")

if not records:
    sys.exit(f"{path} is empty — the hooks are configured but nothing has fired yet")

records.sort(key=lambda r: r.get("at", 0))

# --- which hooks fire, and how often -----------------------------------
counts = Counter(r["hook"] for r in records)
print(f"{len(records)} events across {len(counts)} hook types\n")
print("HOOK FREQUENCY")
for hook, n in counts.most_common():
    print(f"  {hook:<22} {n}")

# --- what fields each hook carries -------------------------------------
# Field names only. A hook that carries no stable session id cannot
# support the normalized envelope's sessionId.
fields = defaultdict(Counter)
for r in records:
    payload = r.get("payload")
    if isinstance(payload, dict):
        for k in payload:
            fields[r["hook"]][k] += 1

print("\nPAYLOAD FIELDS (names only — values are never printed)")
for hook in counts:
    keys = fields.get(hook)
    if not keys:
        print(f"  {hook:<22} (no object payload)")
        continue
    n = counts[hook]
    always = sorted(k for k, c in keys.items() if c == n)
    sometimes = sorted(k for k, c in keys.items() if c < n)
    print(f"  {hook}")
    print(f"      always:    {', '.join(always) or '-'}")
    if sometimes:
        print(f"      sometimes: {', '.join(sometimes)}")

# --- can every hook identify its session? ------------------------------
print("\nSESSION IDENTITY (needed for the normalized envelope's sessionId)")
for hook in counts:
    keys = fields.get(hook, {})
    found = [k for k in ("session_id", "sessionId") if k in keys]
    ok = found and keys[found[0]] == counts[hook]
    print(f"  {hook:<22} {'yes (' + found[0] + ')' if ok else 'MISSING — cannot be normalized'}")

# --- ordering, per session ---------------------------------------------
by_session = defaultdict(list)
for r in records:
    payload = r.get("payload") or {}
    sid = payload.get("session_id") or payload.get("sessionId") or "<none>"
    by_session[sid].append(r)

print(f"\nSEQUENCES ({len(by_session)} session(s))")
for sid, evs in by_session.items():
    print(f"\n  session {sid[:12]}  ({len(evs)} events)")
    prev = None
    for r in evs:
        gap = ""
        if prev is not None:
            delta = r.get("at", 0) - prev
            if delta >= 0.5:
                gap = f"   (+{delta:.1f}s)"
        prev = r.get("at", 0)
        payload = r.get("payload") or {}
        # tool_name is a Claude Code identifier, not user content.
        tool = payload.get("tool_name", "")
        print(f"    {r['hook']:<22}{tool:<16}{gap}")

# --- the transitions we actually need ----------------------------------
print("\nMAPPING CANDIDATES")
interesting = {
    "SessionStart": "session_started",
    "SessionEnd": "session_ended",
    "UserPromptSubmit": "task_started",
    "PermissionRequest": "permission_requested",
    "PermissionDenied": "(denial — no normalized event yet)",
    "Notification": "permission_requested (older guess)",
    "PostToolUseFailure": "task_failed",
    "StopFailure": "task_failed",
    "Stop": "task_completed",
}
for hook, candidate in interesting.items():
    seen = "fired" if hook in counts else "NEVER FIRED"
    print(f"  {hook:<22} {seen:<12} -> {candidate}")
