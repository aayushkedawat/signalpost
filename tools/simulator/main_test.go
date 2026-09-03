package main

import "testing"

// TestScenarioStepsAreValidEvents: every scenario must be built from
// protocol.md §1 names, or it fails at the server with a 400 instead of
// exercising the pipeline.
func TestScenarioStepsAreValidEvents(t *testing.T) {
	for name, steps := range scenarios {
		if len(steps) == 0 {
			t.Errorf("scenario %q is empty", name)
		}
		for _, step := range steps {
			if !valid(step) {
				t.Errorf("scenario %q uses unknown event %q", name, step)
			}
		}
	}
}

// TestScenariosStartASession, so replaying one against a fresh server
// exercises the intended path rather than the no-history path.
func TestScenariosStartASession(t *testing.T) {
	for name, steps := range scenarios {
		if steps[0] != "session_started" {
			t.Errorf("scenario %q starts with %q, want session_started", name, steps[0])
		}
	}
}

// TestCatalogIsPrintable covers the names printed by --list, which are
// listed separately from the map and would otherwise drift out of sync.
func TestCatalogIsPrintable(t *testing.T) {
	printed := []string{"normal", "permission", "input", "error", "crash", "abandoned", "inconsistent", "full"}
	if len(printed) != len(scenarios) {
		t.Fatalf("--list prints %d scenarios but %d are defined", len(printed), len(scenarios))
	}
	for _, name := range printed {
		if _, ok := scenarios[name]; !ok {
			t.Errorf("--list prints unknown scenario %q", name)
		}
	}
}

func TestEventCatalogMatchesProtocol(t *testing.T) {
	want := []string{
		"session_started", "task_started", "permission_requested",
		"permission_granted", "input_received", "task_completed",
		"task_failed", "session_ended",
	}
	if len(events) != len(want) {
		t.Fatalf("simulator knows %d events, protocol.md §1 lists %d", len(events), len(want))
	}
	for _, e := range want {
		if !valid(e) {
			t.Errorf("simulator does not accept %q", e)
		}
	}
}
