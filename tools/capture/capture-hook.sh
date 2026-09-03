#!/bin/sh
# Phase-2 payload capture. Throwaway tooling: it exists to answer "what
# does Claude Code actually send?", which docs/protocol.md §8 flags as
# unvalidated guesswork. Delete once the adapter is written.
#
# Invoked by a Claude Code hook as:  capture-hook.sh <HookEventName>
# The raw payload arrives on stdin and is appended verbatim, one JSON
# object per line, in arrival order.
#
# Fail-open is the whole discipline here (PRD.md §7): this runs inside
# Claude Code's critical path, so it must never block, never fail, and
# never write to stdout/stderr. Every path exits 0.

CAPTURE_DIR="${TL_CAPTURE_DIR:-$(CDPATH= cd -- "$(dirname -- "$0")/../../capture" 2>/dev/null && pwd)}"
[ -z "$CAPTURE_DIR" ] && exit 0
mkdir -p "$CAPTURE_DIR" 2>/dev/null || exit 0

payload=$(cat 2>/dev/null)
[ -z "$payload" ] && payload=null

# Millisecond resolution: hooks can fire in bursts, and the gaps between
# them are part of what we are trying to learn. Falls back to whole
# seconds if perl is unavailable.
at=$(perl -MTime::HiRes -e 'printf "%.3f", Time::HiRes::time()' 2>/dev/null) || at=""
[ -z "$at" ] && at=$(date -u +%s 2>/dev/null)
[ -z "$at" ] && at=0

# A single printf under O_APPEND, so concurrent hook processes cannot
# interleave and file order is arrival order.
printf '{"at":%s,"hook":"%s","cwd":"%s","payload":%s}\n' \
    "$at" "${1:-unknown}" "${PWD:-}" "$payload" >> "$CAPTURE_DIR/raw.jsonl" 2>/dev/null

exit 0
