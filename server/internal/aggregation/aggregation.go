// Package aggregation collapses per-session state into the one-status-
// per-tool map and the single headline `overall` field served by
// GET /state (docs/protocol.md §9, §10).
//
// The priority order is applied identically at both levels, and `overall`
// is computed here rather than by clients so every surface — desktop,
// terminal, watch — shows the same headline colour without re-deriving
// it (PRD.md §5, "combined desktop icon").
package aggregation

import (
	"sort"
	"time"

	"trafficlight/internal/types"
)

// priority is protocol.md §9: WAITING > ERROR > UNKNOWN > EXECUTING >
// DONE > IDLE. UNKNOWN outranks EXECUTING/DONE/IDLE because "I'm not
// sure" is more worth surfacing than a possibly-stale "it's fine".
var priority = map[types.AgentState]int{
	types.StateWaiting:   6,
	types.StateError:     5,
	types.StateUnknown:   4,
	types.StateExecuting: 3,
	types.StateDone:      2,
	types.StateIdle:      1,
}

// Build renders the GET /state payload. waitingTooLong is the urgency
// threshold (default 30s) that flips a tool's waitingTooLong flag; it
// plays no part in deciding WAITING itself (PRD.md §5).
func Build(recs []types.SessionRecord, updatedAt, now time.Time, waitingTooLong time.Duration) types.StateResponse {
	byTool := make(map[string][]types.SessionRecord)
	for _, rec := range recs {
		byTool[rec.Tool] = append(byTool[rec.Tool], rec)
	}

	tools := make(map[string]types.ToolStatus, len(byTool))
	for tool, sessions := range byTool {
		tools[tool] = toolStatus(sessions, now, waitingTooLong)
	}

	return types.StateResponse{
		Version:   1,
		UpdatedAt: formatTime(updatedAt),
		Overall:   overall(tools),
		Tools:     tools,
	}
}

func toolStatus(sessions []types.SessionRecord, now time.Time, waitingTooLong time.Duration) types.ToolStatus {
	state := types.StateIdle
	for _, rec := range sessions {
		if priority[rec.State] > priority[state] {
			state = rec.State
		}
	}

	// `since` is when the tool reached this state, i.e. the earliest of
	// the sessions currently holding it — not the newest event.
	var since time.Time
	tooLong := false
	for _, rec := range sessions {
		if rec.State != state {
			continue
		}
		if since.IsZero() || rec.Since.Before(since) {
			since = rec.Since
		}
		if state == types.StateWaiting && now.Sub(rec.Since) >= waitingTooLong {
			tooLong = true
		}
	}

	return types.ToolStatus{
		State:          state,
		Since:          formatTime(since),
		ActiveSessions: len(sessions),
		WaitingTooLong: tooLong,
	}
}

// overall picks the single most urgent tool. The responsible tool is
// named so a tray can render "Claude needs you" directly; it is null
// when the headline is IDLE, where no tool is meaningfully responsible.
func overall(tools map[string]types.ToolStatus) types.OverallStatus {
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic tie-break between equal states

	state := types.StateIdle
	var responsible string
	for _, name := range names {
		if priority[tools[name].State] > priority[state] {
			state = tools[name].State
			responsible = name
		}
	}

	if state == types.StateIdle || responsible == "" {
		return types.OverallStatus{State: types.StateIdle}
	}
	return types.OverallStatus{State: state, Tool: &responsible}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
