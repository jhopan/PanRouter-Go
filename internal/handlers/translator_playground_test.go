package handlers

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/handlers/chat"
)

func translatorTestRouter(t *testing.T) (*TranslatorHandler, chi.Router, func()) {
	t.Helper()
	database, cleanup := setupTestDB(t)
	repo := db.NewRepo(database)
	h := NewTranslatorHandler(t.TempDir(), chat.NewChatHandler(repo))
	r := chi.NewRouter()
	r.Get("/api/translator/load", h.HandleLoad)
	r.Post("/api/translator/save", h.HandleSave)
	r.Post("/api/translator/send", h.HandleSend)
	r.Post("/api/translator/translate", h.HandleTranslate)
	return h, r, cleanup
}

func decodeTranslatorEnvelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

// TestTranslatorLoadReturnsUpstreamShape pins the load contract the editor
// reads: {success: true, content}, and the refusal codes around it.
func TestTranslatorLoadReturnsUpstreamShape(t *testing.T) {
	h, r, cleanup := translatorTestRouter(t)
	defer cleanup()

	const content = `{"model":"claude-sonnet-4","messages":[]}`
	if err := os.WriteFile(filepath.Join(h.Dir, "1_req_client.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/translator/load?file=1_req_client.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := decodeTranslatorEnvelope(t, rec)
	if body["success"] != true {
		t.Errorf("success = %v, want true", body["success"])
	}
	if body["content"] != content {
		t.Errorf("content = %q, want the file's bytes", body["content"])
	}

	cases := []struct {
		name   string
		query  string
		status int
		error  string
	}{
		{name: "missing parameter", query: "", status: http.StatusBadRequest, error: "File parameter required"},
		{name: "path is not allowlisted", query: "?file=../../etc/passwd", status: http.StatusBadRequest, error: "Invalid file name"},
		{name: "allowlisted but absent", query: "?file=7_res_client.txt", status: http.StatusNotFound, error: "File not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/translator/load"+tc.query, nil))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			body := decodeTranslatorEnvelope(t, rec)
			if body["success"] != false || body["error"] != tc.error {
				t.Errorf("body = %v, want {success:false, error:%q}", body, tc.error)
			}
		})
	}
}

// TestTranslatorSaveWritesTheAllowlistedFile covers the write half of the
// editor: the content lands on disk and the response is {success:true}.
func TestTranslatorSaveWritesTheAllowlistedFile(t *testing.T) {
	h, r, cleanup := translatorTestRouter(t)
	defer cleanup()

	payload := `{"file":"3_req_openai.json","content":"{\"messages\":[]}"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/translator/save", strings.NewReader(payload))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if body := decodeTranslatorEnvelope(t, rec); body["success"] != true {
		t.Errorf("success = %v, want true", body["success"])
	}
	written, err := os.ReadFile(filepath.Join(h.Dir, "3_req_openai.json"))
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(written) != `{"messages":[]}` {
		t.Errorf("saved content = %q", written)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/translator/save",
		strings.NewReader(`{"file":"9_other.json","content":"x"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid file status = %d, want 400", rec.Code)
	}
	if body := decodeTranslatorEnvelope(t, rec); body["error"] != "Invalid file name" {
		t.Errorf("error = %v, want upstream's message", body["error"])
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/translator/save",
		strings.NewReader(`{"content":"x"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing file status = %d, want 400", rec.Code)
	}
	if body := decodeTranslatorEnvelope(t, rec); body["error"] != "File and content required" {
		t.Errorf("error = %v, want upstream's message", body["error"])
	}
}

// TestTranslatorTranslateStep1DetectsFormats pins step 1's result shape and
// the format detection the editor's first pane shows.
func TestTranslatorTranslateStep1DetectsFormats(t *testing.T) {
	_, r, cleanup := translatorTestRouter(t)
	defer cleanup()

	payload := `{"step":1,"body":{"model":"claude/claude-sonnet-4","system":"be brief",
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/translator/translate", strings.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := decodeTranslatorEnvelope(t, rec)
	if body["success"] != true {
		t.Fatalf("success = %v, want true", body["success"])
	}
	result, _ := body["result"].(map[string]any)
	if result["provider"] != "claude" {
		t.Errorf("provider = %v, want claude", result["provider"])
	}
	if result["model"] != "claude-sonnet-4" {
		t.Errorf("model = %v, want claude-sonnet-4", result["model"])
	}
	if result["sourceFormat"] != "claude" {
		t.Errorf("sourceFormat = %v, want claude", result["sourceFormat"])
	}
	if result["targetFormat"] != "claude" {
		t.Errorf("targetFormat = %v, want claude", result["targetFormat"])
	}
}

// TestTranslatorTranslateStep2PivotsThroughOpenAI proves the first half of the
// pipeline reaches the real converter rather than echoing the request.
func TestTranslatorTranslateStep2PivotsThroughOpenAI(t *testing.T) {
	_, r, cleanup := translatorTestRouter(t)
	defer cleanup()

	payload := `{"step":2,"body":{"model":"claude/claude-sonnet-4","max_tokens":64,"system":"be brief",
		"messages":[{"role":"user","content":"hello"}]}}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/translator/translate", strings.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := decodeTranslatorEnvelope(t, rec)
	result, _ := body["result"].(map[string]any)
	converted, _ := result["body"].(map[string]any)
	if _, has := converted["_toolNameMap"]; has {
		t.Error("the OpenAI intermediate must not carry the tool-name sidecar")
	}
	messages, _ := converted["messages"].([]any)
	if len(messages) < 2 {
		t.Fatalf("converted body has no system+user pair: %v", converted)
	}
	// Claude's `system` becomes an OpenAI system turn, so the user message is
	// the second one — that ordering is the converter's contract, not an echo.
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Errorf("first message = %v, want the system prompt", first)
	}
	second, _ := messages[1].(map[string]any)
	if second["role"] != "user" {
		t.Errorf("second message role = %v, want user", second["role"])
	}
}

// TestTranslatorTranslateRefusesMismatchedPair pins upstream's refusals: a
// provider/model pair the step requires but did not receive, and a step outside
// 1-3. Both keep the exact message the Next route returns.
func TestTranslatorTranslateRefusesMismatchedPair(t *testing.T) {
	_, r, cleanup := translatorTestRouter(t)
	defer cleanup()

	cases := []struct {
		name    string
		payload string
		status  int
		error   string
	}{
		{
			name:    "step 3 without the pair",
			payload: `{"step":3,"body":{"model":"gpt-4o","messages":[]}}`,
			status:  http.StatusBadRequest,
			error:   "provider and model required",
		},
		{
			name:    "step outside the pipeline",
			payload: `{"step":9,"body":{"model":"gpt-4o"}}`,
			status:  http.StatusBadRequest,
			error:   "Invalid step (1-3)",
		},
		{
			name:    "no step",
			payload: `{"body":{"model":"gpt-4o"}}`,
			status:  http.StatusBadRequest,
			error:   "Step and body required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/translator/translate", strings.NewReader(tc.payload)))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			body := decodeTranslatorEnvelope(t, rec)
			if body["success"] != false || body["error"] != tc.error {
				t.Errorf("body = %v, want {success:false, error:%q}", body, tc.error)
			}
		})
	}
}

// TestTranslatorTranslateRefusesPairThisPortCannotProduce is the honest half of
// the playground: a pair with no Go converter is a 400 naming both formats, not
// a body silently passed through as if it had been translated.
func TestTranslatorTranslateRefusesPairThisPortCannotProduce(t *testing.T) {
	_, r, cleanup := translatorTestRouter(t)
	defer cleanup()

	payload := `{"step":3,"body":{"provider":"anthropic","model":"claude-sonnet-4",
		"body":{"messages":[{"role":"user","content":"hi"}]}}}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/translator/translate", strings.NewReader(payload)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	body := decodeTranslatorEnvelope(t, rec)
	message, _ := body["error"].(string)
	if !strings.Contains(message, "openai -> claude") {
		t.Errorf("error = %q, want the unsupported pair named", message)
	}
}

// TestTranslatorSendRefusesWithoutAPairOrConnection covers the two refusals
// that happen before any provider call: upstream's required-fields message and
// the missing active connection.
func TestTranslatorSendRefusesWithoutAPairOrConnection(t *testing.T) {
	_, r, cleanup := translatorTestRouter(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/translator/send",
		strings.NewReader(`{"provider":"openai","model":"gpt-4o"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if body := decodeTranslatorEnvelope(t, rec); body["error"] != "provider, model, and body required" {
		t.Errorf("error = %v, want upstream's message", body["error"])
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/translator/send",
		strings.NewReader(`{"provider":"openai","model":"gpt-4o","body":{"messages":[]}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if body := decodeTranslatorEnvelope(t, rec); body["error"] != "No active connection for provider: openai" {
		t.Errorf("error = %v, want the missing-connection message", body["error"])
	}
}

// TestTranslatorRoutesAreMounted checks the four paths on the router the server
// actually builds, so a route cannot exist only in this test's own table.
func TestTranslatorRoutesAreMounted(t *testing.T) {
	t.Setenv("JWT_SECRET", "translator-test-secret")
	database, cleanup := setupTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	r := chi.NewRouter()
	SetupDashboardRoutes(r, repo, nil)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/translator/load", nil))
	if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/translator/load is not mounted: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing file parameter)", rec.Code)
	}
	for _, path := range []string{"/api/translator/save", "/api/translator/send", "/api/translator/translate"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("POST %s is not mounted: %d", path, rec.Code)
		}
	}
}
