package dashboard

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"panrouter/proxy/internal/models"
)

// Port-of-upstream test for src/app/api/models/availability/route.js.
func TestModelAvailability(t *testing.T) {
	repo, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)

	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	create := func(id, provider, authType, name, data string) {
		t.Helper()
		if err := repo.CreateProviderConnectionFull(id, provider, authType, name, nil, data); err != nil {
			t.Fatalf("create connection %s: %v", id, err)
		}
	}

	create("c1", "openai", "apikey", "Primary", `{"modelLock_gpt-5.5":"`+future+`","testStatus":"active","lastError":"boom"}`)
	create("c2", "anthropic", "apikey", "", `{"testStatus":"unavailable","lastError":"no key"}`)
	// An expired lock is not a lock: upstream filters it out, and the
	// connection's own "unavailable" status then reports the account instead.
	create("c3", "google", "apikey", "Expired", `{"modelLock_old":"2000-01-01T00:00:00Z","testStatus":"unavailable"}`)
	// A null lock with a healthy status reports nothing at all.
	create("c4", "groq", "apikey", "Healthy", `{"modelLock_x":null,"testStatus":"active"}`)

	router := setupTestRouter(repo)

	t.Run("GET lists live cooldowns and unavailable accounts", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/models/availability", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Models []struct {
				Provider       string `json:"provider"`
				Model          string `json:"model"`
				Status         string `json:"status"`
				Until          any    `json:"until"`
				ConnectionID   string `json:"connectionId"`
				ConnectionName string `json:"connectionName"`
				LastError      any    `json:"lastError"`
			} `json:"models"`
			UnavailableCount int `json:"unavailableCount"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
		}
		if body.UnavailableCount != 3 || len(body.Models) != 3 {
			t.Fatalf("models = %d, unavailableCount = %d, want 3/3: %s",
				len(body.Models), body.UnavailableCount, rec.Body.String())
		}

		tests := []struct {
			name           string
			connection     string
			wantModel      string
			wantStatus     string
			wantUntil      bool
			wantName       string
			wantLastError  string
		}{
			{name: "live lock", connection: "c1", wantModel: "gpt-5.5", wantStatus: "cooldown",
				wantUntil: true, wantName: "Primary", wantLastError: "boom"},
			// No name and no email: the row id is the only label the operator
			// has (upstream `name || email || id`).
			{name: "unavailable with no lock", connection: "c2", wantModel: "__all",
				wantStatus: "unavailable", wantName: "c2", wantLastError: "no key"},
			{name: "expired lock falls back to unavailable", connection: "c3", wantModel: "__all",
				wantStatus: "unavailable", wantUntil: false, wantName: "Expired"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				var found *struct {
					Provider       string `json:"provider"`
					Model          string `json:"model"`
					Status         string `json:"status"`
					Until          any    `json:"until"`
					ConnectionID   string `json:"connectionId"`
					ConnectionName string `json:"connectionName"`
					LastError      any    `json:"lastError"`
				}
				for i := range body.Models {
					if body.Models[i].ConnectionID == tt.connection {
						found = &body.Models[i]
						break
					}
				}
				if found == nil {
					t.Fatalf("connection %s missing from response", tt.connection)
				}
				if found.Model != tt.wantModel {
					t.Errorf("model = %q, want %q", found.Model, tt.wantModel)
				}
				if found.Status != tt.wantStatus {
					t.Errorf("status = %q, want %q", found.Status, tt.wantStatus)
				}
				if got := found.Until != nil; got != tt.wantUntil {
					t.Errorf("until present = %v, want %v (%v)", got, tt.wantUntil, found.Until)
				}
				if found.ConnectionName != tt.wantName {
					t.Errorf("connectionName = %q, want %q", found.ConnectionName, tt.wantName)
				}
				if tt.wantLastError != "" {
					if got, _ := found.LastError.(string); got != tt.wantLastError {
						t.Errorf("lastError = %v, want %q", found.LastError, tt.wantLastError)
					}
				}
			})
		}
	})

	t.Run("POST rejects an unknown action", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/models/availability",
			strings.NewReader(`{"action":"nope","provider":"openai","model":"gpt-5.5"}`))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if body["error"] != "Invalid request" {
			t.Errorf("error = %v, want %q", body["error"], "Invalid request")
		}
	})

	t.Run("POST clearCooldown clears the lock and revives the row", func(t *testing.T) {
		if err := repo.CreateProviderConnectionFull("c5", "openai", "apikey", "Second", nil,
			`{"modelLock_gpt-5.5":"`+future+`","testStatus":"unavailable","lastError":"429","lastErrorAt":"2026-01-01T00:00:00Z","backoffLevel":3}`); err != nil {
			t.Fatalf("create connection: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/api/models/availability",
			strings.NewReader(`{"action":"clearCooldown","provider":"openai","model":"gpt-5.5"}`))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if body["ok"] != true {
			t.Errorf("ok = %v, want true", body["ok"])
		}

		conn, err := repo.GetProviderConnectionByID("c5")
		if err != nil || conn == nil {
			t.Fatalf("reload connection: %v", err)
		}
		data := connectionDataMap(conn.Data)
		if data["modelLock_gpt-5.5"] != nil {
			t.Errorf("modelLock_gpt-5.5 = %v, want null", data["modelLock_gpt-5.5"])
		}
		if data["testStatus"] != "active" {
			t.Errorf("testStatus = %v, want active", data["testStatus"])
		}
		if data["lastError"] != nil {
			t.Errorf("lastError = %v, want null", data["lastError"])
		}
		if data["lastErrorAt"] != nil {
			t.Errorf("lastErrorAt = %v, want null", data["lastErrorAt"])
		}
		if backoff, ok := data["backoffLevel"].(float64); !ok || backoff != 0 {
			t.Errorf("backoffLevel = %v, want 0", data["backoffLevel"])
		}
	})
}

// TestAvailabilityConnectionName pins the id fallback, which is the one label
// an operator has when a parked row carries neither name nor email.
func TestAvailabilityConnectionName(t *testing.T) {
	name := "Named"
	email := "a@example.com"
	tests := []struct {
		name string
		conn *models.ProviderConnection
		want string
	}{
		{name: "name wins", conn: &models.ProviderConnection{ID: "c1", Name: &name, Email: &email}, want: name},
		{name: "email second", conn: &models.ProviderConnection{ID: "c1", Email: &email}, want: email},
		{name: "id last", conn: &models.ProviderConnection{ID: "c1"}, want: "c1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := availabilityConnectionName(tt.conn); got != tt.want {
				t.Errorf("name = %q, want %q", got, tt.want)
			}
		})
	}
}