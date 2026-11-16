//go:build integration

package integration

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"

	json "encoding/json/v2"
)

// TestCodeBuddyWireIdentityEndToEnd drives a real request through the router to a
// fake CodeBuddy Intl upstream: the body must arrive gzipped with the CLI's folded
// system prompt, and the headers must carry the identity the gateway keys on — the
// CLI user agent, the per-turn id family and the tracing block.
func TestCodeBuddyWireIdentityEndToEnd(t *testing.T) {
	env := newEnv(t)

	var got http.Header
	up := env.NewUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	env.AddConnection(t, "cb-intl-1", "codebuddy-intl", "CodeBuddy Intl", up, "cb-token")

	body := map[string]any{
		"model": "cbai/claude-opus-4-6",
		"messages": []map[string]any{
			{"role": "system", "content": "You are Claude Code, Anthropic's official CLI for Claude."},
			{"role": "developer", "content": "Answer in one sentence."},
			{"role": "user", "content": "ping"},
		},
	}
	res := env.Post(t, "/v1/chat/completions", body)
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody: %s", res.Status, truncate(res.Body))
	}

	// ── headers ──────────────────────────────────────────────────────────────
	if ua := got.Get("User-Agent"); ua != "CLI/2.178.0 CodeBuddy/2.178.0" {
		t.Errorf("User-Agent = %q, want the CLI identity", ua)
	}
	for _, key := range []string{"X-Root-Request-ID", "traceparent", "b3", "X-B3-TraceId", "X-Trace-ID", "baggage", "X-Conversation-ID"} {
		if got.Get(key) == "" {
			t.Errorf("%s is missing from the outbound request", key)
		}
	}
	for _, absent := range []string{"X-App", "X-Product-Version", "X-Stainless-Helper-Method"} {
		if got.Get(absent) != "" {
			t.Errorf("%s = %q, want it absent (the CLI never sends it)", absent, got.Get(absent))
		}
	}

	// ── body ────────────────────────────────────────────────────────────────
	// The harness records the body before the handler runs, so it is read from the
	// recording rather than from r.Body.
	rawBody := up.Last(t).Body
	if enc := got.Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	gz, err := gzip.NewReader(bytes.NewReader(rawBody))
	if err != nil {
		t.Fatalf("wire body is not gzip: %v", err)
	}
	plain, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("decompress wire body: %v", err)
	}
	var sent struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(plain, &sent); err != nil {
		t.Fatalf("decode wire body: %v", err)
	}
	if !sent.Stream {
		t.Error("stream must be forced true: the gateway refuses non-stream requests")
	}
	if len(sent.Messages) != 2 || sent.Messages[0].Role != "system" {
		t.Fatalf("expected ONE leading system message plus the user turn, got %s", plain)
	}
	leading, _ := sent.Messages[0].Content.(string)
	if strings.Contains(leading, "Claude Code") {
		t.Errorf("the agent identity reached the gateway: %q", leading)
	}
	if !strings.Contains(leading, "Answer in one sentence.") {
		t.Errorf("the client's own instruction was dropped: %q", leading)
	}
	if !strings.Contains(leading, "helpful AI assistant") {
		t.Errorf("the neutral replacement is missing from the fold: %q", leading)
	}
	if blocks, ok := sent.Messages[1].Content.([]any); !ok || len(blocks) == 0 {
		t.Errorf("user content must travel as typed blocks, got %#v", sent.Messages[1].Content)
	}
}

// TestCodeBuddyIntlRefusalIsNotReplayed pins the transport contract for this lane:
// one refused turn is ONE upstream request. A gateway that bans accounts over
// repeated request patterns reads a replayed turn as three, and the replay was
// happening on any refusal whose body sniffed as text/plain.
func TestCodeBuddyIntlRefusalIsNotReplayed(t *testing.T) {
	env := newEnv(t)

	up := env.NewUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// No Content-Type: Go sniffs text/plain, which is what a proxy refusal page
		// looks like too. The body is CodeBuddy's own answer.
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":11140,"msg":"request illegal"}`))
	})
	env.AddConnection(t, "cb-refused-1", "codebuddy-intl", "refused account", up, "cb-token")

	res := env.Post(t, "/v1/chat/completions", ChatBody("cbai/claude-opus-4-6", false))
	if res.Status == http.StatusOK {
		t.Fatalf("a refused turn must not be answered as a success: %s", truncate(res.Body))
	}
	if got := up.Count(); got != 1 {
		t.Errorf("upstream requests = %d, want 1: the refusal must not be replayed", got)
	}
}

// TestCodeBuddyBanRotationEndToEnd parks a banned account through the real router
// and proves the turn still succeeds on the next one — the behaviour the field log
// showed failing ("3 accounts refused — stopping rotation").
func TestCodeBuddyBanRotationEndToEnd(t *testing.T) {
	env := newEnv(t)

	banned := env.NewUpstream(t, JSONResponder(http.StatusForbidden,
		`{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity"}}`))
	healthy := env.NewUpstream(t, chatCompletionResponder())

	env.AddConnection(t, "cb-ban-1", "codebuddy-intl", "banned account", banned, "cb-token-1")
	env.AddConnection(t, "cb-ok-1", "codebuddy-intl", "healthy account", healthy, "cb-token-2")

	res := env.Post(t, "/v1/chat/completions", ChatBody("cbai/claude-opus-4-6", false))
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want the healthy account to serve the turn\nbody: %s", res.Status, truncate(res.Body))
	}
	if healthy.Count() == 0 {
		t.Fatal("the healthy account was never dialled — a ban must not end the rotation")
	}
	if banned.Count() != 1 {
		t.Errorf("the banned account was dialled %d times, want 1", banned.Count())
	}

	// The ban must be visible on the row, with its reason, so the dashboard can
	// show it and the selector can skip the account.
	conn, err := env.Repo.GetProviderConnectionByID("cb-ban-1")
	if err != nil || conn == nil {
		t.Fatalf("read banned connection: %v %v", conn, err)
	}
	if !strings.Contains(conn.Data, "bannedUntil") || !strings.Contains(conn.Data, "unusual activity") {
		t.Errorf("the ban was not recorded with its reason: %s", conn.Data)
	}
}
