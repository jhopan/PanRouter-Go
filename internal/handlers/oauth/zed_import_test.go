package oauth

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"panrouter/proxy/internal/browsercookies"
	"panrouter/proxy/internal/db"
)

// stubZedCredentials is a reader that answers without touching this machine.
type stubZedCredentials struct {
	creds ZedIdeCredentials
	err   error
}

func (s stubZedCredentials) ReadZedCredentials(context.Context) (ZedIdeCredentials, error) {
	return s.creds, s.err
}

// connectionCount is how many rows the lane wrote, which is what the refusal
// cases assert: a read that found nothing must not store anything.
func connectionCount(t *testing.T, database *db.Repo) int {
	t.Helper()
	var count int
	if err := database.RawDB().QueryRow("SELECT COUNT(*) FROM providerConnections").Scan(&count); err != nil {
		t.Fatalf("count connections: %v", err)
	}
	return count
}

// TestHandleZedImport_storesValidatedSession pins the upstream import contract:
// the session is validated against cloud.zed.dev, the row carries the imported
// auth method and organization, and the answer nests the connection.
func TestHandleZedImport_storesValidatedSession(t *testing.T) {
	var authHeader, systemHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != zedMePath {
			t.Errorf("upstream path = %q, want %q", r.URL.Path, zedMePath)
		}
		authHeader = r.Header.Get("Authorization")
		systemHeader = r.Header.Get(zedSystemHeader)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"email":"ada@example.com","name":"Ada Lovelace",
			"default_organization_id":"",
			"organizations":[{"id":"org-personal","is_personal":true},{"id":"org-other"}]}`))
	}))
	defer upstream.Close()

	database, cleanup := setupOAuthTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	handler := NewOAuthHandler(repo)
	handler.ZedCloudBase = upstream.URL

	const accessToken = "zed-access-token-1"
	body := `{"accessToken":"` + accessToken + `","userId":123,"systemId":"sys-1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/zed/import", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.HandleZedImport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if want := "123 " + accessToken; authHeader != want {
		t.Errorf("Authorization = %q, want %q", authHeader, want)
	}
	if systemHeader != "sys-1" {
		t.Errorf("%s = %q, want %q", zedSystemHeader, systemHeader, "sys-1")
	}

	var resp struct {
		Success    bool `json:"success"`
		Connection struct {
			ID          string  `json:"id"`
			Provider    string  `json:"provider"`
			Email       *string `json:"email"`
			DisplayName string  `json:"displayName"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Errorf("success = false, want true: %s", rec.Body.String())
	}
	if want := "zed-" + shortHash(accessToken); resp.Connection.ID != want {
		t.Errorf("connection.id = %q, want %q", resp.Connection.ID, want)
	}
	if resp.Connection.Provider != "zed" {
		t.Errorf("connection.provider = %q, want zed", resp.Connection.Provider)
	}
	if resp.Connection.Email == nil || *resp.Connection.Email != "ada@example.com" {
		t.Errorf("connection.email = %v, want ada@example.com", resp.Connection.Email)
	}
	if resp.Connection.DisplayName != "Ada Lovelace" {
		t.Errorf("connection.displayName = %q, want %q", resp.Connection.DisplayName, "Ada Lovelace")
	}

	var provider, authType, name, data string
	if err := database.QueryRow(
		"SELECT provider, authType, name, data FROM providerConnections WHERE id = ?",
		resp.Connection.ID,
	).Scan(&provider, &authType, &name, &data); err != nil {
		t.Fatalf("read stored connection: %v", err)
	}
	if provider != "zed" || authType != "oauth" {
		t.Errorf("stored provider/authType = %q/%q, want zed/oauth", provider, authType)
	}
	if name != "Ada Lovelace" {
		t.Errorf("stored name = %q, want %q", name, "Ada Lovelace")
	}
	var stored struct {
		AccessToken string `json:"accessToken"`
		Email       string `json:"email"`
		PSD         struct {
			AuthMethod     string `json:"authMethod"`
			UserID         string `json:"userId"`
			SystemID       string `json:"systemId"`
			OrganizationID string `json:"organizationId"`
		} `json:"providerSpecificData"`
	}
	if err := json.Unmarshal([]byte(data), &stored); err != nil {
		t.Fatalf("decode stored data: %v", err)
	}
	if stored.AccessToken != accessToken || stored.Email != "ada@example.com" {
		t.Errorf("stored credentials = %+v", stored)
	}
	if stored.PSD.AuthMethod != "imported" {
		t.Errorf("stored authMethod = %q, want imported", stored.PSD.AuthMethod)
	}
	if stored.PSD.UserID != "123" || stored.PSD.SystemID != "sys-1" {
		t.Errorf("stored identity = %+v", stored.PSD)
	}
	if stored.PSD.OrganizationID != "org-personal" {
		t.Errorf("stored organizationId = %q, want org-personal", stored.PSD.OrganizationID)
	}
}

// TestHandleZedImport_refusedTokenIsUnauthorized pins the upstream status for a
// token that does not validate, and that nothing is written.
func TestHandleZedImport_refusedTokenIsUnauthorized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"token is not valid"}`))
	}))
	defer upstream.Close()

	database, cleanup := setupOAuthTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	handler := NewOAuthHandler(repo)
	handler.ZedCloudBase = upstream.URL

	req := httptest.NewRequest(http.MethodPost, "/api/oauth/zed/import",
		strings.NewReader(`{"accessToken":"bad","userId":"42"}`))
	rec := httptest.NewRecorder()
	handler.HandleZedImport(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "token is not valid") {
		t.Errorf("response does not carry the upstream message: %s", body)
	}
	if got := connectionCount(t, repo); got != 0 {
		t.Errorf("wrote %d connections for a refused token, want 0", got)
	}
}

// TestHandleZedImport_requiresFields pins the two 400s upstream answers before
// any validation request is made.
func TestHandleZedImport_requiresFields(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "no access token", body: `{"userId":"7"}`, want: "Access token is required"},
		{name: "no user id", body: `{"accessToken":"tok"}`, want: "User id is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewOAuthHandler(nil)
			// A base that would fail loudly if the handler reached the network.
			handler.ZedCloudBase = "http://127.0.0.1:1"
			req := httptest.NewRequest(http.MethodPost, "/api/oauth/zed/import", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			handler.HandleZedImport(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); !strings.Contains(body, tt.want) {
				t.Errorf("response = %s, want message %q", body, tt.want)
			}
		})
	}
}

// TestHandleZedAutoImport_reportsAMachineWithoutZed pins the refusal: the
// reader's reason is passed through with 200, a reasonless read answers with
// upstream's own wording, credentialsUrl is null when unknown, and no
// connection is written.
func TestHandleZedAutoImport_reportsAMachineWithoutZed(t *testing.T) {
	tests := []struct {
		name            string
		creds           ZedIdeCredentials
		wantError       string
		wantCredentials any
	}{
		{
			name:      "reader's own reason",
			creds:     ZedIdeCredentials{Error: "Zed is not installed on this machine"},
			wantError: "Zed is not installed on this machine",
		},
		{
			name:            "upstream's default wording",
			creds:           ZedIdeCredentials{CredentialsURL: "https://zed.dev"},
			wantError:       "Zed IDE credentials not found",
			wantCredentials: "https://zed.dev",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, cleanup := setupOAuthTestDB(t)
			defer cleanup()
			repo := db.NewRepo(database)
			handler := NewOAuthHandler(repo)
			handler.ZedCredentials = stubZedCredentials{creds: tt.creds}

			req := httptest.NewRequest(http.MethodGet, "/api/oauth/zed/auto-import", nil)
			rec := httptest.NewRecorder()
			handler.HandleZedAutoImport(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp["found"] != false {
				t.Errorf("found = %v, want false", resp["found"])
			}
			if resp["error"] != tt.wantError {
				t.Errorf("error = %v, want %q", resp["error"], tt.wantError)
			}
			if value, ok := resp["credentialsUrl"]; !ok || value != tt.wantCredentials {
				t.Errorf("credentialsUrl = %v (present=%v), want %v", value, ok, tt.wantCredentials)
			}
			if _, ok := resp["accessToken"]; ok {
				t.Errorf("refusal carries an accessToken: %s", rec.Body.String())
			}
			if got := connectionCount(t, repo); got != 0 {
				t.Errorf("wrote %d connections for a refusal, want 0", got)
			}
		})
	}
}

// TestHandleZedAutoImport_reportsTheFoundSession pins the found shape, which is
// what the page hands to /api/oauth/zed/import.
func TestHandleZedAutoImport_reportsTheFoundSession(t *testing.T) {
	handler := NewOAuthHandler(nil)
	handler.ZedCredentials = stubZedCredentials{creds: ZedIdeCredentials{
		Found:          true,
		UserID:         "user-9",
		AccessToken:    "token-9",
		SystemID:       "system-9",
		CredentialsURL: "https://zed.dev",
	}}

	req := httptest.NewRequest(http.MethodGet, "/api/oauth/zed/auto-import", nil)
	rec := httptest.NewRecorder()
	handler.HandleZedAutoImport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := map[string]any{
		"found":          true,
		"userId":         "user-9",
		"accessToken":    "token-9",
		"systemId":       "system-9",
		"credentialsUrl": "https://zed.dev",
	}
	for key, value := range want {
		if resp[key] != value {
			t.Errorf("%s = %v, want %v", key, resp[key], value)
		}
	}
}

// TestHandleZedAutoImport_readerFailureIsServerError pins the 500 branch: a
// reader that could not run at all is not "nothing found".
func TestHandleZedAutoImport_readerFailureIsServerError(t *testing.T) {
	handler := NewOAuthHandler(nil)
	handler.ZedCredentials = stubZedCredentials{err: errors.New("keyring is locked")}

	req := httptest.NewRequest(http.MethodGet, "/api/oauth/zed/auto-import", nil)
	rec := httptest.NewRecorder()
	handler.HandleZedAutoImport(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "keyring is locked") {
		t.Errorf("response = %s, want the reader's reason", body)
	}
}

// TestPickZedSecret pins the item choice: Zed's own label wins, and an item
// with an account and a secret is still usable when no label matches.
func TestPickZedSecret(t *testing.T) {
	tests := []struct {
		name    string
		secrets []browsercookies.Secret
		wantID  string
		wantOK  bool
	}{
		{
			name: "prefers zed's own label",
			secrets: []browsercookies.Secret{
				{Username: "other", Value: "other-token"},
				{Label: zedKeyringLabel, Username: "zed-user", Value: "zed-token"},
			},
			wantID: "zed-user",
			wantOK: true,
		},
		{
			name:    "falls back to any complete item",
			secrets: []browsercookies.Secret{{Label: "renamed", Username: "u", Value: "t"}},
			wantID:  "u",
			wantOK:  true,
		},
		{
			name:    "ignores an item without an account",
			secrets: []browsercookies.Secret{{Label: zedKeyringLabel, Value: "t"}},
		},
		{
			name:    "ignores an item without a secret",
			secrets: []browsercookies.Secret{{Label: zedKeyringLabel, Username: "u"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pickZedSecret(tt.secrets)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got.Username != tt.wantID {
				t.Errorf("Username = %q, want %q", got.Username, tt.wantID)
			}
		})
	}
}

// TestZedUserIDString pins the id conversion: Zed sends the id as a number in
// some payloads and as a string in others, and both name the same account.
func TestZedUserIDString(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "number", raw: `12345`, want: "12345"},
		{name: "string", raw: `" 12345 "`, want: "12345"},
		{name: "absent", raw: ``, want: ""},
		{name: "null", raw: `null`, want: ""},
		{name: "object", raw: `{}`, want: "{}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := zedUserIDString(jsontext.Value(tt.raw)); got != tt.want {
				t.Errorf("zedUserIDString(%s) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
