package chat

import "testing"

// TestIsAntigravityModelServed_EffortVariantIsVouchedByItsFamily pins the roster
// read after the 2026-11-18 capture: the discovery RPC enumerates a family
// (`gemini-3.8-flash-tiered`), the client sends the effort ids
// (`gemini-3.8-flash-high`), and the backend serves both. A roster read as an
// exact allow-list blocked the served id — and only after the first quota refresh
// filled the roster, so a model that answered on startup began answering 502.
func TestIsAntigravityModelServed_EffortVariantIsVouchedByItsFamily(t *testing.T) {
	const conn = "ag-roster-effort"
	seedRoster := func(models ...string) {
		roster := make(map[string]struct{}, len(models))
		for _, m := range models {
			roster[m] = struct{}{}
		}
		agRosterMu.Lock()
		agRosterCache[conn] = roster
		agRosterMu.Unlock()
	}
	t.Cleanup(func() {
		agRosterMu.Lock()
		delete(agRosterCache, conn)
		agRosterMu.Unlock()
	})

	// The captured roster: the tiered family id, none of the effort ids.
	seedRoster("gemini-3.8-flash-tiered", "gemini-3-flash", "claude-sonnet-4-6")

	for _, model := range []string{
		"gemini-3.8-flash-tiered", // listed as-is
		"gemini-3.8-flash-high",   // effort variant of the listed family
		"gemini-3.8-flash-medium",
		"gemini-3.8-flash-low",
		"gemini-3.8-flash-agent",
		"gemini-3.8-flash-thinking",
		"claude-sonnet-4-6", // listed as-is
	} {
		if !IsAntigravityModelServed(conn, model) {
			t.Errorf("IsAntigravityModelServed(%q) = false, want true — the account serves it", model)
		}
	}

	// A model of a family the account does not serve still gets no vouch.
	for _, model := range []string{"gemini-2.5-flash", "gpt-oss-120b-medium", "xmodel-1"} {
		if IsAntigravityModelServed(conn, model) {
			t.Errorf("IsAntigravityModelServed(%q) = true, want false — the roster does not carry that family", model)
		}
	}

	// A roster that carries only the 2.5 family must not vouch for the 3.8 one.
	seedRoster("gemini-2.5-flash")
	if IsAntigravityModelServed(conn, "gemini-3.8-flash-high") {
		t.Error("a 2.5-only roster vouched for a 3.8 model")
	}
	if !IsAntigravityModelServed(conn, "gemini-2.5-flash-thinking") {
		t.Error("the 2.5 family must vouch for its own effort variant")
	}

	// An unknown roster is optimistic: a fresh account is not blanked by a
	// discovery that never ran.
	agRosterMu.Lock()
	delete(agRosterCache, conn)
	agRosterMu.Unlock()
	if !IsAntigravityModelServed(conn, "gemini-3.8-flash-high") {
		t.Error("an unknown roster must not block a model")
	}
}
