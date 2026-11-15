package dashboard

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// usageQuotaRow decodes one quota row out of a usage response, so the assertion
// is on the reading the dashboard renders rather than on a raw JSON substring.
func usageQuotaRow(t *testing.T, body []byte, name string) (map[string]any, bool) {
	t.Helper()
	var payload struct {
		Quotas map[string]map[string]any `json:"quotas"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode usage response: %v (%s)", err, body)
	}
	row, ok := payload.Quotas[name]
	return row, ok
}

// The dashboard's "parked until" has to be the quota-exhaustion date when the
// chat path recorded one, not the generic cooldown written beside it. A quota
// refusal need not write a model lock, so the quota field can be the only record
// of the park — upstream's cooldown helper reads it for exactly that reason
// (src/shared/utils/cooldown.js:36-46).
func TestHandleGetConnectionUsage_ReportsTheQuotaParkDeadline(t *testing.T) {
	repo, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)

	park := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Second)
	cooldown := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	payload := `{"apiKey":"sk-cline","rateLimitedUntil":"` + cooldown.Format(time.RFC3339) +
		`","quotaExhaustedUntil":"` + park.Format(time.RFC3339) + `","testStatus":"unavailable"}`
	if err := repo.CreateProviderConnectionFull("conn-park", "cline", "apikey", "Parked", nil, payload); err != nil {
		t.Fatalf("create connection: %v", err)
	}

	rec := httptest.NewRecorder()
	setupTestRouter(repo).ServeHTTP(rec, httptest.NewRequest("GET", "/api/usage/conn-park", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	row, ok := usageQuotaRow(t, rec.Body.Bytes(), "default")
	if !ok {
		t.Fatalf("no cooldown row rendered for a parked account: %s", rec.Body.String())
	}
	if got := row["resetAt"]; got != park.Format(time.RFC3339) {
		t.Errorf("resetAt = %v, want the quota park %s (the generic cooldown was %s)",
			got, park.Format(time.RFC3339), cooldown.Format(time.RFC3339))
	}
}

// A lapsed quota park parks nothing: the account is usable again, and the panel
// must not count down to a date that has already gone. The generic cooldown
// beside it still answers, so the quota field cannot hide a live park either.
func TestHandleGetConnectionUsage_IgnoresALapsedQuotaPark(t *testing.T) {
	lapsed := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	live := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)

	tests := []struct {
		name      string
		data      string
		wantRow   bool
		wantReset string
	}{
		{
			name: "lapsed quota park alone renders nothing",
			data: `{"apiKey":"sk-cline","quotaExhaustedUntil":"` + lapsed.Format(time.RFC3339) + `"}`,
		},
		{
			name:      "lapsed quota park leaves the live cooldown to answer",
			data:      `{"apiKey":"sk-cline","quotaExhaustedUntil":"` + lapsed.Format(time.RFC3339) + `","rateLimitedUntil":"` + live.Format(time.RFC3339) + `"}`,
			wantRow:   true,
			wantReset: live.Format(time.RFC3339),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, cleanup := setupTestDB(t)
			t.Cleanup(cleanup)
			if err := repo.CreateProviderConnectionFull("conn-stale", "cline", "apikey", "Stale", nil, tt.data); err != nil {
				t.Fatalf("create connection: %v", err)
			}

			rec := httptest.NewRecorder()
			setupTestRouter(repo).ServeHTTP(rec, httptest.NewRequest("GET", "/api/usage/conn-stale", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			row, ok := usageQuotaRow(t, rec.Body.Bytes(), "default")
			if ok != tt.wantRow {
				t.Fatalf("cooldown row rendered = %v, want %v: %s", ok, tt.wantRow, rec.Body.String())
			}
			if !tt.wantRow {
				return
			}
			if got := row["resetAt"]; got != tt.wantReset {
				t.Errorf("resetAt = %v, want %s", got, tt.wantReset)
			}
		})
	}
}
