package aggregation

import (
	"encoding/json"
	"testing"
	"time"

	"trafficlight/internal/types"
)

var base = time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)

func rec(tool, session string, state types.AgentState, sinceOffset time.Duration) types.SessionRecord {
	return types.SessionRecord{
		Tool:      tool,
		SessionID: session,
		State:     state,
		Since:     base.Add(sinceOffset),
	}
}

// TestPriorityOrderIsComplete pins protocol.md §9 exactly:
// WAITING > ERROR > UNKNOWN > EXECUTING > DONE > IDLE.
func TestPriorityOrderIsComplete(t *testing.T) {
	order := []types.AgentState{
		types.StateWaiting, types.StateError, types.StateUnknown,
		types.StateExecuting, types.StateDone, types.StateIdle,
	}
	if len(priority) != len(order) {
		t.Fatalf("priority table has %d entries, want %d — a state is unranked", len(priority), len(order))
	}
	for i := 1; i < len(order); i++ {
		if priority[order[i-1]] <= priority[order[i]] {
			t.Fatalf("%s does not outrank %s", order[i-1], order[i])
		}
	}
}

// TestPerToolAggregationPicksMostUrgent: one session waiting and another
// executing means the tool shows WAITING.
func TestPerToolAggregationPicksMostUrgent(t *testing.T) {
	resp := Build([]types.SessionRecord{
		rec("claude", "s1", types.StateExecuting, 0),
		rec("claude", "s2", types.StateWaiting, time.Second),
	}, base, base, 30*time.Second)

	got := resp.Tools["claude"]
	if got.State != types.StateWaiting {
		t.Fatalf("tool state = %s, want waiting", got.State)
	}
	if got.ActiveSessions != 2 {
		t.Fatalf("activeSessions = %d, want 2", got.ActiveSessions)
	}
}

// TestUnknownOutranksExecuting: "I'm not sure" is more worth surfacing
// than a possibly-stale "it's fine".
func TestUnknownOutranksExecuting(t *testing.T) {
	resp := Build([]types.SessionRecord{
		rec("claude", "s1", types.StateExecuting, 0),
		rec("claude", "s2", types.StateUnknown, 0),
		rec("claude", "s3", types.StateDone, 0),
	}, base, base, 30*time.Second)

	if got := resp.Tools["claude"].State; got != types.StateUnknown {
		t.Fatalf("tool state = %s, want unknown", got)
	}
}

// TestWaitingOutranksError: the developer is needed either way, but a
// prompt waiting on them is the actionable one.
func TestWaitingOutranksError(t *testing.T) {
	resp := Build([]types.SessionRecord{
		rec("claude", "s1", types.StateError, 0),
		rec("claude", "s2", types.StateWaiting, 0),
	}, base, base, 30*time.Second)

	if got := resp.Tools["claude"].State; got != types.StateWaiting {
		t.Fatalf("tool state = %s, want waiting", got)
	}
}

// TestSinceIsEarliestSessionHoldingTheState: the tool has been waiting
// since the first session started waiting, not the most recent.
func TestSinceIsEarliestSessionHoldingTheState(t *testing.T) {
	resp := Build([]types.SessionRecord{
		rec("claude", "s1", types.StateWaiting, 5*time.Minute),
		rec("claude", "s2", types.StateWaiting, time.Minute),
		rec("claude", "s3", types.StateExecuting, 0), // earlier, but not the aggregate state
	}, base, base, 30*time.Second)

	want := base.Add(time.Minute).Format(time.RFC3339)
	if got := resp.Tools["claude"].Since; got != want {
		t.Fatalf("since = %s, want %s", got, want)
	}
}

// TestCrossToolAggregation: no blended state — the most urgent tool wins
// and is named.
func TestCrossToolAggregation(t *testing.T) {
	resp := Build([]types.SessionRecord{
		rec("claude", "s1", types.StateExecuting, 0),
		rec("copilot", "c1", types.StateWaiting, 0),
	}, base, base, 30*time.Second)

	if resp.Overall.State != types.StateWaiting {
		t.Fatalf("overall state = %s, want waiting", resp.Overall.State)
	}
	if resp.Overall.Tool == nil || *resp.Overall.Tool != "copilot" {
		t.Fatalf("overall tool = %v, want copilot", resp.Overall.Tool)
	}
	if resp.Tools["claude"].State != types.StateExecuting {
		t.Fatal("per-tool detail lost; the popover still needs both tools")
	}
}

// TestOverallTieBreakIsDeterministic: two tools in the same state must
// not make the headline flicker between them across polls.
func TestOverallTieBreakIsDeterministic(t *testing.T) {
	recs := []types.SessionRecord{
		rec("copilot", "c1", types.StateWaiting, 0),
		rec("claude", "s1", types.StateWaiting, 0),
	}
	for i := 0; i < 50; i++ {
		resp := Build(recs, base, base, 30*time.Second)
		if resp.Overall.Tool == nil || *resp.Overall.Tool != "claude" {
			t.Fatalf("tie-break not deterministic: got %v", resp.Overall.Tool)
		}
	}
}

// TestOverallToolIsNullWhenIdle: no tool is meaningfully "responsible"
// for idle.
func TestOverallToolIsNullWhenIdle(t *testing.T) {
	resp := Build([]types.SessionRecord{
		rec("claude", "s1", types.StateIdle, 0),
	}, base, base, 30*time.Second)

	if resp.Overall.State != types.StateIdle {
		t.Fatalf("overall state = %s, want idle", resp.Overall.State)
	}
	if resp.Overall.Tool != nil {
		t.Fatalf("overall tool = %v, want null", *resp.Overall.Tool)
	}
}

// TestNoSessions: an empty server is idle with an empty (not null) tools
// map, so clients can iterate it without a nil check.
func TestNoSessions(t *testing.T) {
	resp := Build(nil, base, base, 30*time.Second)
	if resp.Overall.State != types.StateIdle || resp.Overall.Tool != nil {
		t.Fatalf("overall = %+v, want idle with no tool", resp.Overall)
	}

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["tools"] == nil {
		t.Fatalf("tools serialized as null, want {}: %s", raw)
	}
}

// TestWaitingTooLong is the urgency flag only — it never decides WAITING.
func TestWaitingTooLong(t *testing.T) {
	t.Run("under threshold", func(t *testing.T) {
		resp := Build([]types.SessionRecord{
			rec("claude", "s1", types.StateWaiting, 0),
		}, base, base.Add(29*time.Second), 30*time.Second)
		if resp.Tools["claude"].WaitingTooLong {
			t.Fatal("flagged too long at 29s with a 30s threshold")
		}
	})

	t.Run("at threshold", func(t *testing.T) {
		resp := Build([]types.SessionRecord{
			rec("claude", "s1", types.StateWaiting, 0),
		}, base, base.Add(30*time.Second), 30*time.Second)
		if !resp.Tools["claude"].WaitingTooLong {
			t.Fatal("not flagged too long at 30s with a 30s threshold")
		}
	})

	t.Run("longest waiting session wins the flag", func(t *testing.T) {
		resp := Build([]types.SessionRecord{
			rec("claude", "s1", types.StateWaiting, 40*time.Second), // recent
			rec("claude", "s2", types.StateWaiting, 0),              // long-suffering
		}, base, base.Add(60*time.Second), 30*time.Second)
		if !resp.Tools["claude"].WaitingTooLong {
			t.Fatal("a session waiting 60s did not flag the tool as urgent")
		}
	})

	t.Run("never set for a non-waiting tool", func(t *testing.T) {
		resp := Build([]types.SessionRecord{
			rec("claude", "s1", types.StateExecuting, 0),
		}, base, base.Add(time.Hour), 30*time.Second)
		if resp.Tools["claude"].WaitingTooLong {
			t.Fatal("waitingTooLong set on an executing tool")
		}
	})
}

// TestResponseShapeMatchesProtocol checks the wire contract in §10 field
// by field, since three separate clients are written against it.
func TestResponseShapeMatchesProtocol(t *testing.T) {
	resp := Build([]types.SessionRecord{
		rec("claude", "s1", types.StateWaiting, 0),
		rec("copilot", "c1", types.StateExecuting, 0),
	}, base, base.Add(time.Minute), 30*time.Second)

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Version   int    `json:"version"`
		UpdatedAt string `json:"updatedAt"`
		Overall   struct {
			State string  `json:"state"`
			Tool  *string `json:"tool"`
		} `json:"overall"`
		Tools map[string]struct {
			State          string `json:"state"`
			Since          string `json:"since"`
			ActiveSessions int    `json:"activeSessions"`
			WaitingTooLong bool   `json:"waitingTooLong"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("response does not match the documented shape: %v\n%s", err, raw)
	}

	if decoded.Version != 1 {
		t.Errorf("version = %d, want 1", decoded.Version)
	}
	if decoded.UpdatedAt != base.Format(time.RFC3339) {
		t.Errorf("updatedAt = %q, want RFC 3339 UTC %q", decoded.UpdatedAt, base.Format(time.RFC3339))
	}
	if decoded.Overall.State != "waiting" || decoded.Overall.Tool == nil || *decoded.Overall.Tool != "claude" {
		t.Errorf("overall = %+v, want waiting/claude", decoded.Overall)
	}
	if len(decoded.Tools) != 2 {
		t.Errorf("tools has %d entries, want 2", len(decoded.Tools))
	}
	if !decoded.Tools["claude"].WaitingTooLong {
		t.Error("claude waited a minute and is not flagged urgent")
	}
	if decoded.Tools["copilot"].WaitingTooLong {
		t.Error("copilot is executing and must not be flagged urgent")
	}
}

// TestUnrankedStateDoesNotWinAggregation guards against a future state
// being added to types without being ranked in §9.
func TestUnrankedStateDoesNotWinAggregation(t *testing.T) {
	resp := Build([]types.SessionRecord{
		rec("claude", "s1", types.AgentState("brand_new"), 0),
		rec("claude", "s2", types.StateExecuting, 0),
	}, base, base, 30*time.Second)

	if got := resp.Tools["claude"].State; got != types.StateExecuting {
		t.Fatalf("tool state = %s, want executing — an unranked state must not outrank a known one", got)
	}
}
