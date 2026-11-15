// Kiro on the Claude wire — parity with the direct `kiro:claude` route
// (open-sse/translator/response/kiro-to-claude.js).
//
// Kiro's gateway speaks AWS EventStream, which the executor turns into
// OpenAI-shaped chunks. A /v1/messages client cannot read those: it needs
// message_start / content_block_* / message_stop frames, and without the
// translation it sees an upstream "answering in the wrong format" and retries.
package executor

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"panrouter/proxy/internal/providers"
)

// kiroClaudeFrames runs ForwardKiro with a Claude client and returns the raw body.
func kiroClaudeFrames(t *testing.T, stream bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		// Kiro streams the reasoning event before the answer, which is the order
		// the Claude client renders as a thinking block followed by text.
		_, _ = w.Write(kiroEventFrame("reasoningContentEvent", `{"text":"thinking"}`))
		_, _ = w.Write(kiroEventFrame("assistantResponseEvent", `{"content":"Hello"}`))
		_, _ = w.Write(kiroEventFrame("assistantResponseEvent", `{"content":" there"}`))
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	err := ForwardKiro(rec, &Request{
		Client:        srv.Client(),
		Config:        &providers.ProviderConfig{BaseURL: srv.URL, AuthHeader: "Authorization", AuthScheme: "bearer"},
		Provider:      "kiro",
		APIKey:        "token",
		Body:          []byte(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		IsStream:      stream,
		ClaudeClient:  true,
		TranslateResp: true,
		ModelName:     "claude-sonnet-4.5",
		SessionID:     "session-1",
	})
	if err != nil {
		t.Fatalf("ForwardKiro: %v", err)
	}
	return rec.Body.String()
}

func TestForwardKiro_ClaudeClientGetsClaudeStream(t *testing.T) {
	body := kiroClaudeFrames(t, true)

	for _, want := range []string{`"type":"message_start"`, `"type":"content_block_delta"`, `"thinking_delta"`, `"text_delta"`, `"type":"message_stop"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Claude stream missing %s\nbody: %s", want, body)
		}
	}
	// OpenAI markers must not leak into a Messages stream.
	if strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Errorf("OpenAI chunk shape leaked to a Claude client: %s", body)
	}
	// The reasoning event has to become a thinking block, in order, before the text.
	if strings.Index(body, "thinking_delta") > strings.Index(body, "text_delta") {
		t.Errorf("thinking must precede text\nbody: %s", body)
	}
}

func TestForwardKiro_ClaudeClientNonStreamGetsClaudeMessage(t *testing.T) {
	body := kiroClaudeFrames(t, false)
	if !strings.Contains(body, `"type":"message"`) {
		t.Fatalf("non-stream Claude answer must be a message object, got %s", body)
	}
	if strings.Contains(body, `"object":"chat.completion"`) {
		t.Errorf("OpenAI completion shape leaked to a Claude client: %s", body)
	}
	if !strings.Contains(body, "Hello") {
		t.Errorf("the assistant text is missing: %s", body)
	}
}

func TestForwardKiro_ClaudeClientRestoresToolNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(kiroEventFrame("toolUseEvent", `{"name":"mcp_server_do_thing","toolUseId":"tu-1","input":{"a":1},"stop":true}`))
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	err := ForwardKiro(rec, &Request{
		Client:        srv.Client(),
		Config:        &providers.ProviderConfig{BaseURL: srv.URL, AuthHeader: "Authorization", AuthScheme: "bearer"},
		Provider:      "kiro",
		APIKey:        "token",
		Body:          []byte(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		IsStream:      true,
		ClaudeClient:  true,
		TranslateResp: true,
		ModelName:     "claude-sonnet-4.5",
		// The request sanitized the client's `mcp.server.do_thing`; the client can
		// only dispatch the name it declared.
		ToolNameMap: map[string]string{"mcp_server_do_thing": "mcp.server.do_thing"},
	})
	if err != nil {
		t.Fatalf("ForwardKiro: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"name":"mcp.server.do_thing"`) {
		t.Errorf("tool name was not restored for the client: %s", body)
	}
	if strings.Contains(body, `"name":"mcp_server_do_thing"`) {
		t.Errorf("the sanitized name reached the client: %s", body)
	}
}
