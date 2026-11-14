package oauth

import (
	"encoding/base64"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"panrouter/proxy/internal/db"
)

// codexImportTokenJWT builds an unsigned JWT-shaped access token. The import
// lane reads the claims without verifying the signature, exactly as upstream
// does, so a fabricated payload is the honest fixture here.
func codexImportTokenJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

// storedConnectionData reads one connection row's JSON payload.
func storedConnectionData(t *testing.T, repo *db.Repo, id string) (string, string, string, map[string]any) {
	t.Helper()
	var provider, authType, name, raw string
	if err := repo.RawDB().QueryRow(
		`SELECT provider, authType, name, data FROM providerConnections WHERE id = ?`, id,
	).Scan(&provider, &authType, &name, &raw); err != nil {
		t.Fatalf("read stored connection: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("decode stored connection data: %v", err)
	}
	return provider, authType, name, data
}

// TestHandleCodexImportToken_StoresAccountMetadata pins the import contract:
// the token is stored as an access_token connection, the account email/workspace
// /plan are read out of the token's claims, and the answer describes the row it
// wrote.
func TestHandleCodexImportToken_StoresAccountMetadata(t *testing.T) {
	database, cleanup := setupOAuthTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	handler := NewOAuthHandler(repo)

	token := codexImportTokenJWT(t, map[string]any{
		"preferred_username": "fallback",
		"exp":                1900000000,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-42",
			"chatgpt_plan_type":  "pro",
		},
		"https://api.openai.com/profile": map[string]any{
			"email": "ada@example.com",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/oauth/codex/import-token",
		strings.NewReader(`{"accessToken":"`+token+`"}`))
	rec := httptest.NewRecorder()
	handler.HandleCodexImportToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Success    bool `json:"success"`
		Connection struct {
			ID        string  `json:"id"`
			Provider  string  `json:"provider"`
			Email     *string `json:"email"`
			Name      string  `json:"name"`
			Workspace *string `json:"workspace"`
			Plan      *string `json:"plan"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success || resp.Connection.ID == "" {
		t.Fatalf("unexpected response shape: %s", rec.Body.String())
	}
	if resp.Connection.Email == nil || *resp.Connection.Email != "ada@example.com" {
		t.Errorf("response email = %v, want ada@example.com", resp.Connection.Email)
	}
	if resp.Connection.Name != "ada@example.com" {
		t.Errorf("response name = %q, want the account email as the default name", resp.Connection.Name)
	}
	if resp.Connection.Workspace == nil || *resp.Connection.Workspace != "acct-42" {
		t.Errorf("response workspace = %v, want acct-42", resp.Connection.Workspace)
	}
	if resp.Connection.Plan == nil || *resp.Connection.Plan != "pro" {
		t.Errorf("response plan = %v, want pro", resp.Connection.Plan)
	}

	provider, authType, name, data := storedConnectionData(t, repo, resp.Connection.ID)
	if provider != "codex" || authType != "access_token" {
		t.Errorf("stored provider/authType = %s/%s, want codex/access_token", provider, authType)
	}
	if name != "ada@example.com" {
		t.Errorf("stored name = %q, want ada@example.com", name)
	}
	if data["accessToken"] != token {
		t.Errorf("stored accessToken = %v, want the imported token", data["accessToken"])
	}
	if data["testStatus"] != "active" {
		t.Errorf("stored testStatus = %v, want active", data["testStatus"])
	}
	if data["email"] != "ada@example.com" {
		t.Errorf("stored email = %v, want ada@example.com", data["email"])
	}
	psd, _ := data["providerSpecificData"].(map[string]any)
	if psd["authMethod"] != "access_token" {
		t.Errorf("stored authMethod = %v, want access_token", psd["authMethod"])
	}
	if psd["chatgptAccountId"] != "acct-42" {
		t.Errorf("stored chatgptAccountId = %v, want acct-42", psd["chatgptAccountId"])
	}
	if psd["chatgptPlanType"] != "pro" {
		t.Errorf("stored chatgptPlanType = %v, want pro", psd["chatgptPlanType"])
	}
	if _, ok := psd["jwtExp"]; !ok {
		t.Errorf("stored providerSpecificData is missing jwtExp: %v", psd)
	}
}

// TestHandleCodexImportToken_RefusesMalformedPayload pins upstream's refusal:
// a missing or non-string token is a 400 carrying upstream's own sentence, and
// nothing is written.
func TestHandleCodexImportToken_RefusesMalformedPayload(t *testing.T) {
	database, cleanup := setupOAuthTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	handler := NewOAuthHandler(repo)

	for _, body := range []string{`{}`, `{"accessToken":""}`, `{"accessToken":42}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/oauth/codex/import-token", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.HandleCodexImportToken(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400: %s", body, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Access token is required") {
			t.Errorf("body %s: response %s is missing upstream's message", body, rec.Body.String())
		}
	}
	if got := connectionCount(t, repo); got != 0 {
		t.Errorf("stored %d connections after refusals, want 0", got)
	}
}

// TestHandleCodexImportToken_KeepsExplicitName pins that a supplied name wins
// over the derived account email.
func TestHandleCodexImportToken_KeepsExplicitName(t *testing.T) {
	database, cleanup := setupOAuthTestDB(t)
	defer cleanup()
	handler := NewOAuthHandler(db.NewRepo(database))

	token := codexImportTokenJWT(t, map[string]any{"email": "ada@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/codex/import-token",
		strings.NewReader(`{"accessToken":"`+token+`","name":"Work Codex"}`))
	rec := httptest.NewRecorder()
	handler.HandleCodexImportToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	_, _, name, _ := storedConnectionData(t, db.NewRepo(database), jsonField(t, rec.Body.Bytes(), "connection", "id"))
	if name != "Work Codex" {
		t.Errorf("stored name = %q, want Work Codex", name)
	}
}

// jsonField reads a nested string field out of a response body.
func jsonField(t *testing.T, body []byte, path ...string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	var cur any = doc
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("field %v: not an object at %q", path, key)
		}
		cur = m[key]
	}
	s, _ := cur.(string)
	return s
}
