package oauth

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"panrouter/proxy/internal/db"
)

// TestHandleMimoAPIKey_RefusesUnusableRequests pins the two 400s upstream
// answers before it touches the network.
func TestHandleMimoAPIKey_RefusesUnusableRequests(t *testing.T) {
	handler := NewOAuthHandler(nil)
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "no credential at all", body: `{}`, want: "API key is required"},
		{name: "wrong key prefix", body: `{"apiKey":"abc-123"}`, want: "Invalid key format — expected sk- prefix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/oauth/xiaomi-mimo/api-key", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			handler.HandleMimoAPIKey(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.want) {
				t.Errorf("response %s is missing %q", rec.Body.String(), tt.want)
			}
		})
	}
}

// TestHandleMimoAPIKey_ValidatesAndStoresKey pins the key import contract: the
// key is validated against the models endpoint, the model count is recorded,
// the base URL is normalised, and the row is stored as an api_key connection.
func TestHandleMimoAPIKey_ValidatesAndStoresKey(t *testing.T) {
	var sawAuth, sawSource string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("validation path = %q, want /v1/models", r.URL.Path)
		}
		sawAuth = r.Header.Get("Authorization")
		sawSource = r.Header.Get("X-Mimo-Source")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"mimo-a"},{"id":"mimo-b"}]}`))
	}))
	defer upstream.Close()

	database, cleanup := setupOAuthTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	handler := NewOAuthHandler(repo)

	body := `{"apiKey":"  sk-live-1  ","uid":"42","baseUrl":"` + upstream.URL + `/v1///"}`
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/xiaomi-mimo/api-key", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.HandleMimoAPIKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if sawAuth != "Bearer sk-live-1" {
		t.Errorf("validation Authorization = %q, want the trimmed bearer key", sawAuth)
	}
	if sawSource != mimoModelsSource {
		t.Errorf("validation X-Mimo-Source = %q, want %q", sawSource, mimoModelsSource)
	}

	var resp struct {
		Success    bool `json:"success"`
		Validated  bool `json:"validated"`
		ModelCount int  `json:"modelCount"`
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
	if !resp.Success || !resp.Validated || resp.ModelCount != 2 {
		t.Errorf("response = %s, want success/validated with 2 models", rec.Body.String())
	}
	if resp.Connection.Provider != "xiaomi-mimo" || resp.Connection.DisplayName != "Xiaomi 42" {
		t.Errorf("connection = %+v, want xiaomi-mimo/Xiaomi 42", resp.Connection)
	}
	if resp.Connection.Email == nil || *resp.Connection.Email != "42@xiaomi" {
		t.Errorf("connection email = %v, want 42@xiaomi", resp.Connection.Email)
	}

	provider, authType, name, data := storedConnectionData(t, repo, resp.Connection.ID)
	if provider != "xiaomi-mimo" || authType != "api_key" || name != "Xiaomi 42" {
		t.Errorf("stored row = %s/%s/%s, want xiaomi-mimo/api_key/Xiaomi 42", provider, authType, name)
	}
	if data["accessToken"] != "sk-live-1" || data["apiKey"] != "sk-live-1" {
		t.Errorf("stored credential = %v / %v, want the trimmed key", data["accessToken"], data["apiKey"])
	}
	if data["testStatus"] != "active" {
		t.Errorf("stored testStatus = %v, want active after a validated import", data["testStatus"])
	}
	psd, _ := data["providerSpecificData"].(map[string]any)
	if psd["baseUrl"] != upstream.URL+"/v1" {
		t.Errorf("stored baseUrl = %v, want the trailing slashes trimmed", psd["baseUrl"])
	}
	if psd["modelCount"] != float64(2) {
		t.Errorf("stored modelCount = %v, want 2", psd["modelCount"])
	}
	if psd["uid"] != "42" || psd["region"] != "cn" || psd["authMethod"] != "api_key" {
		t.Errorf("stored psd = %v, want uid 42 / region cn / authMethod api_key", psd)
	}
}

// TestHandleMimoAPIKey_SoftFailsValidation pins the documented soft gate: a key
// the models endpoint refuses still imports, marked untested.
func TestHandleMimoAPIKey_SoftFailsValidation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	database, cleanup := setupOAuthTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	handler := NewOAuthHandler(repo)

	body := `{"apiKey":"sk-unverified","baseUrl":"` + upstream.URL + `/v1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/xiaomi-mimo/api-key", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.HandleMimoAPIKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Validated  bool `json:"validated"`
		Connection struct {
			ID string `json:"id"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Validated {
		t.Error("validated = true for a key the endpoint refused")
	}
	_, _, _, data := storedConnectionData(t, repo, resp.Connection.ID)
	if data["testStatus"] != "untested" {
		t.Errorf("stored testStatus = %v, want untested", data["testStatus"])
	}
	if data["accessToken"] != "sk-unverified" {
		t.Errorf("stored accessToken = %v, want the key to be kept", data["accessToken"])
	}
}

// TestHandleMimoAPIKey_SessionImportDedupes pins the session-only lane and its
// dedup rule: the same Desktop identity in the same region updates the existing
// row instead of writing a second account.
func TestHandleMimoAPIKey_SessionImportDedupes(t *testing.T) {
	database, cleanup := setupOAuthTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	handler := NewOAuthHandler(repo)

	body := `{"mimoPassToken":"pt-1","mimoUserId":"user-7","mimoCUserId":"c-1","region":"cn"}`
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/xiaomi-mimo/api-key", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.HandleMimoAPIKey(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first import status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var first struct {
		Connection struct {
			ID string `json:"id"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	_, authType, _, data := storedConnectionData(t, repo, first.Connection.ID)
	if authType != "oauth" {
		t.Errorf("session import authType = %q, want oauth", authType)
	}
	if _, ok := data["accessToken"]; ok {
		t.Errorf("session-only import stored an accessToken: %v", data["accessToken"])
	}
	psd, _ := data["providerSpecificData"].(map[string]any)
	if psd["authMethod"] != "session" || psd["mimoPassToken"] != "pt-1" || psd["mimoUserId"] != "user-7" {
		t.Errorf("stored psd = %v, want the session identity", psd)
	}
	if data["testStatus"] != "active" {
		t.Errorf("stored testStatus = %v, want active for a session import", data["testStatus"])
	}

	body = `{"mimoPassToken":"pt-2","mimoUserId":"user-7","mimoCUserId":"c-2","region":"cn"}`
	req = httptest.NewRequest(http.MethodPost, "/api/oauth/xiaomi-mimo/api-key", strings.NewReader(body))
	rec = httptest.NewRecorder()
	handler.HandleMimoAPIKey(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second import status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var second struct {
		Updated    bool `json:"updated"`
		Connection struct {
			ID string `json:"id"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if !second.Updated || second.Connection.ID != first.Connection.ID {
		t.Errorf("second import = %+v, want an update of %s", second, first.Connection.ID)
	}
	if got := connectionCount(t, repo); got != 1 {
		t.Errorf("stored %d connections after re-import, want 1", got)
	}
	_, _, _, data = storedConnectionData(t, repo, first.Connection.ID)
	psd, _ = data["providerSpecificData"].(map[string]any)
	if psd["mimoPassToken"] != "pt-2" || psd["mimoCUserId"] != "c-2" {
		t.Errorf("stored psd = %v, want the refreshed session credential", psd)
	}
}
