package main

import (
	"strings"
	"testing"
)

func waitingState() state {
	tool := "claude"
	var st state
	st.Version = 1
	st.Overall.State = "waiting"
	st.Overall.Tool = &tool
	st.Tools = map[string]struct {
		State          string `json:"state"`
		Since          string `json:"since"`
		ActiveSessions int    `json:"activeSessions"`
		WaitingTooLong bool   `json:"waitingTooLong"`
	}{
		"claude":  {State: "waiting", ActiveSessions: 2, WaitingTooLong: true},
		"copilot": {State: "executing", ActiveSessions: 1},
	}
	return st
}

// TestRenderIsStableAcrossPolls is the property the change-detection
// depends on: two polls of identical state must render identically, or
// the CLI reprints every second and the scrollback becomes useless.
func TestRenderIsStableAcrossPolls(t *testing.T) {
	st := waitingState()
	first := render(st, false)
	for i := 0; i < 5; i++ {
		if got := render(st, false); got != first {
			t.Fatalf("render is not stable across polls:\n%q\n%q", first, got)
		}
	}
	if strings.Contains(first, ":") && strings.Count(first, ":") > 0 {
		// A timestamp inside render() is exactly what broke this before.
		for _, line := range strings.Split(first, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "2") && strings.Count(line, ":") == 2 {
				t.Fatalf("render embedded a timestamp: %q", line)
			}
		}
	}
}

// TestRenderChangesWhenStateChanges guards the other direction.
func TestRenderChangesWhenStateChanges(t *testing.T) {
	before := render(waitingState(), false)
	st := waitingState()
	entry := st.Tools["claude"]
	entry.State = "error"
	st.Tools["claude"] = entry
	if render(st, false) == before {
		t.Fatal("a state change did not change the rendered output")
	}
}

// TestEveryStateHasATextLabel: PRD.md §10 requires state to be conveyed
// by text, not colour alone.
func TestEveryStateHasATextLabel(t *testing.T) {
	for _, s := range []string{"waiting", "executing", "done", "error", "unknown", "idle"} {
		d, ok := display[s]
		if !ok {
			t.Errorf("state %q has no display entry", s)
			continue
		}
		if strings.TrimSpace(d.label) == "" {
			t.Errorf("state %q has no text label", s)
		}
	}
}

// TestRenderWithoutColorHasNoEscapes so the output is usable in a pipe,
// a dumb terminal, or a log.
func TestRenderWithoutColorHasNoEscapes(t *testing.T) {
	if strings.Contains(render(waitingState(), false), "\033") {
		t.Fatal("ANSI escapes present with colour disabled")
	}
	if !strings.Contains(render(waitingState(), true), "\033") {
		t.Fatal("no ANSI escapes present with colour enabled")
	}
}

// TestRenderHandlesUnknownStateName: the client must not break if the
// server gains a state before this client does.
func TestRenderHandlesUnknownStateName(t *testing.T) {
	st := waitingState()
	st.Overall.State = "hibernating"
	out := render(st, false)
	if !strings.Contains(out, "HIBERNATING") {
		t.Fatalf("unrecognised state not surfaced: %q", out)
	}
}

// TestRenderEmptyState
func TestRenderEmptyState(t *testing.T) {
	var st state
	st.Overall.State = "idle"
	out := render(st, false)
	if !strings.Contains(out, "no active sessions") {
		t.Fatalf("empty state rendered as %q", out)
	}
}
