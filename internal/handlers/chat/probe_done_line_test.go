package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/log"
)

// TestProbeAttemptAlsoPrintsTheDoneLine pins the half of the console log a
// dashboard "Test" used to be missing.
//
// A probe is dispatched into the real chat path with a probe marker, and that
// marker returns before the usage write — correctly, because a test is not
// operator traffic and must not appear in the ledger. The completion line was
// behind the same return, so the operator saw the request line ("ACC:Key 2",
// "REQ:…") and then nothing at all: no IN, no OUT, no cache, no latency. The log
// is read to see exactly those numbers, so the line is now emitted first and the
// ledger stays untouched.
func TestProbeAttemptAlsoPrintsTheDoneLine(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","choices":[{"message":{"content":"ok"}}],` +
			`"usage":{"prompt_tokens":12,"completion_tokens":4,"cached_tokens":7}}`))
	}))
	defer upstream.Close()

	seedConnDB(t, database, "deepseek", "conn-done", "sk-1", upstream.URL)
	h := NewChatHandler(db.NewRepo(database))

	log.ClearConsoleLogs()
	body := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	ctx := handlerutil.WithProbeContext(context.Background())
	if err := h.PingConnection(ctx, "deepseek", "deepseek-chat", "conn-done", body); err != nil {
		t.Fatalf("PingConnection: %v", err)
	}

	var done string
	for _, entry := range log.ConsoleEntries() {
		if strings.Contains(entry.Line, "📊") {
			done = entry.Line
		}
	}
	if done == "" {
		t.Fatalf("the probe printed no completion line: %+v", log.ConsoleEntries())
	}
	for _, want := range []string{"DONE ", "IN 12", "CACHE ↻7", "OUT 4"} {
		if !strings.Contains(done, want) {
			t.Errorf("completion line %q is missing %q", done, want)
		}
	}

	// The reason the line used to be missing: the probe must stay out of the
	// ledger, and that has not changed.
	var rows int
	if err := database.QueryRow(`SELECT COUNT(1) FROM usageHistory`).Scan(&rows); err != nil {
		t.Fatalf("count usage rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("the probe wrote %d usage rows, want 0", rows)
	}
}
