// Package types defines the core data shapes for the Traffic Light
// server. Mirrors docs/protocol.md exactly — if these disagree, the doc
// is authoritative; update both together.
package types

import "time"

type AgentState string

const (
	StateIdle      AgentState = "idle"
	StateExecuting AgentState = "executing"
	StateWaiting   AgentState = "waiting"
	StateDone      AgentState = "done"
	StateError     AgentState = "error"
	StateUnknown   AgentState = "unknown"
)

type NormalizedEventType string

const (
	EventSessionStarted      NormalizedEventType = "session_started"
	EventTaskStarted         NormalizedEventType = "task_started"
	EventPermissionRequested NormalizedEventType = "permission_requested"
	EventPermissionGranted   NormalizedEventType = "permission_granted"
	EventInputReceived       NormalizedEventType = "input_received"
	EventTaskCompleted       NormalizedEventType = "task_completed"
	EventTaskFailed          NormalizedEventType = "task_failed"
	EventSessionEnded        NormalizedEventType = "session_ended"
)

// NormalizedEvent is the only shape that crosses the hook -> server
// boundary. No other fields are accepted (privacy requirement — see
// PRD.md §9); the HTTP layer should reject unexpected fields rather
// than silently dropping them.
type NormalizedEvent struct {
	EventID   string              `json:"eventId"`
	Source    string              `json:"source"` // "claude" | "copilot" | future vendors — not a closed set
	SessionID string              `json:"sessionId"`
	Event     NormalizedEventType `json:"event"`
	Timestamp string              `json:"timestamp"` // informational only, ISO 8601; not used for ordering
}

// SessionRecord is server-internal state, never serialized directly to
// clients (ToolStatus/StateResponse below are the client-facing shapes).
type SessionRecord struct {
	Tool            string
	SessionID       string
	State           AgentState
	Since           time.Time
	LastEventAt     time.Time           // server receivedAt of the last accepted event
	SeenEventIDs    map[string]struct{} // dedup; bounded — see session manager
	SeenEventIDFIFO []string            // insertion order for SeenEventIDs, so the dedup set stays bounded
	RecentIntervals []time.Duration     // cadence tracking for staleness heuristic, protocol.md §7
	RemoveAt        *time.Time          // scheduled removal (session_ended-while-urgent, protocol.md §6)
	DoneExpiresAt   *time.Time          // DONE -> IDLE deadline (protocol.md §5); distinct from RemoveAt
}

type ToolStatus struct {
	State          AgentState `json:"state"`
	Since          string     `json:"since"`
	ActiveSessions int        `json:"activeSessions"`
	WaitingTooLong bool       `json:"waitingTooLong"`
}

type OverallStatus struct {
	State AgentState `json:"state"`
	Tool  *string    `json:"tool"` // null when overall is idle and no tool is "responsible"
}

type StateResponse struct {
	Version   int                   `json:"version"`
	UpdatedAt string                `json:"updatedAt"`
	Overall   OverallStatus         `json:"overall"`
	Tools     map[string]ToolStatus `json:"tools"`
}

type HealthResponse struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
}
