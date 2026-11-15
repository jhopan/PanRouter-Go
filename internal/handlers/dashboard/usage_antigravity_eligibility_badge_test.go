package dashboard

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"panrouter/proxy/internal/handlers/chat"
)

// TestUsageResultSerialisesEligibility pins the three states the dashboard badge
// distinguishes, in the shape upstream publishes
// (src/app/api/usage/[connectionId]/route.js):
//
//	flagged  → {"eligible":false,"reasonCode":"VALIDATION_REQUIRED","validationUrl":…}
//	healthy  → {"eligible":true}
//	unknown  → the key is absent, because the reading never mentioned eligibility
//
// Upstream's badge renders only on eligible === false, so a missing key must not
// be confused with a healthy one: a payload that said nothing is not evidence
// that the account is fine.
func TestUsageResultSerialisesEligibility(t *testing.T) {
	tests := []struct {
		name       string
		ineligible *chat.AntigravityEligibility
		wantKey    bool
		wantState  string
	}{
		{
			name: "flagged account carries reason and link",
			ineligible: &chat.AntigravityEligibility{
				ReasonCode:    "VALIDATION_REQUIRED",
				ValidationURL: "https://accounts.google.com/v3/signin/continue?authuser=1",
			},
			wantKey:   true,
			wantState: "flagged",
		},
		{name: "healthy reading", ineligible: &chat.AntigravityEligibility{Eligible: true}, wantKey: true, wantState: "healthy"},
		{name: "no opinion", ineligible: nil, wantKey: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Both response shapes carry it: rows present and the message-only
			// form the provider returns when the quota API refuses.
			for _, res := range []usageResult{
				{plan: "free-tier", quotas: map[string]any{"gemini_weekly": map[string]any{}}, ineligible: tt.ineligible},
				{plan: "free-tier", message: "Antigravity quota API access forbidden.", ineligible: tt.ineligible},
				{plan: "free-tier", ineligible: tt.ineligible},
			} {
				out := res.toResponse()
				raw, present := out["ineligible"]
				if present != tt.wantKey {
					t.Fatalf("ineligible key present = %v, want %v (response %v)", present, tt.wantKey, out)
				}
				if !present {
					continue
				}
				entry, ok := raw.(map[string]any)
				if !ok {
					t.Fatalf("ineligible is %T, want a JSON object", raw)
				}
				if got := entry["eligible"]; got != (tt.wantState != "flagged") {
					t.Errorf("eligible = %v, want %v", got, tt.wantState != "flagged")
				}
				if tt.wantState == "flagged" {
					if entry["reasonCode"] != "VALIDATION_REQUIRED" {
						t.Errorf("reasonCode = %v, want VALIDATION_REQUIRED", entry["reasonCode"])
					}
					if entry["validationUrl"] == "" {
						t.Error("validationUrl missing from a flagged verdict")
					}
				}
			}
		})
	}
}

// TestHandleGetConnectionUsage_AntigravityBadgeFallback covers the badge's data
// source end to end: the quota API is not asked about eligibility on every poll,
// so a connection with no reading of its own — here one whose access token is
// missing, which never reaches the provider — must still surface the flag this
// deployment recorded (upstream route.js:216-222).
func TestHandleGetConnectionUsage_AntigravityBadgeFallback(t *testing.T) {
	fastQuotaGate(t)

	rec := &hitRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mark("any")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	prevProd, prevDaily := antigravityDashboardBaseURL, antigravityDailyBaseURL
	antigravityDashboardBaseURL, antigravityDailyBaseURL = srv.URL, srv.URL
	t.Cleanup(func() {
		antigravityDashboardBaseURL, antigravityDailyBaseURL = prevProd, prevDaily
	})

	repo, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)

	const connID = "ag-badge"
	// No accessToken: the live reading is skipped entirely.
	if err := repo.CreateProviderConnectionFull(
		connID, "antigravity", "oauth", connID, nil, `{"projectId":"proj"}`,
	); err != nil {
		t.Fatalf("seed connection: %v", err)
	}

	chat.ClearAntigravityIneligibleCache()
	t.Cleanup(chat.ClearAntigravityIneligibleCache)
	chat.RecordAntigravityEligibility(connID, &chat.AntigravityEligibility{
		ReasonCode:    "VALIDATION_REQUIRED",
		ValidationURL: "https://accounts.google.com/v3/signin/continue?authuser=1",
	})

	router := setupTestRouter(repo)
	req := httptest.NewRequest(http.MethodGet, "/api/usage/"+connID, nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if rec.count("any") != 0 {
		t.Errorf("the provider was called %d times for a token-less connection", rec.count("any"))
	}
	var out struct {
		Ineligible *struct {
			Eligible      bool   `json:"eligible"`
			ReasonCode    string `json:"reasonCode"`
			ValidationURL string `json:"validationUrl"`
		} `json:"ineligible"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode usage response: %v", err)
	}
	if out.Ineligible == nil {
		t.Fatalf("the cached flag did not reach the dashboard: %s", recorder.Body.String())
	}
	if out.Ineligible.Eligible {
		t.Error("eligible = true, want false for a flagged account")
	}
	if out.Ineligible.ReasonCode != "VALIDATION_REQUIRED" {
		t.Errorf("reasonCode = %q, want VALIDATION_REQUIRED", out.Ineligible.ReasonCode)
	}
	if out.Ineligible.ValidationURL == "" {
		t.Error("validationUrl missing; the badge cannot offer the verification link")
	}
}

// TestHandleGetConnectionUsage_NoOpinionReadingClearsTheBadge pins the clearing
// contract the fallback must not break: a reading that looks and finds no
// eligibility fields means "no opinion", which clears the flag the deployment
// had recorded — so the badge disappears instead of being re-asserted from the
// cache for the rest of the TTL (upstream records `result.ineligible || null`).
func TestHandleGetConnectionUsage_NoOpinionReadingClearsTheBadge(t *testing.T) {
	fastQuotaGate(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1internal:loadCodeAssist":
			// Neither ineligibleTiers nor allowedTiers: no opinion.
			_, _ = w.Write([]byte(`{"cloudaicompanionProject":"proj","currentTier":{"name":"Free"}}`))
		case r.URL.Path == "/v1internal:fetchAvailableModels":
			_, _ = w.Write([]byte(`{"models":{"gemini-3.8-flash-high":{"quotaInfo":{"remainingFraction":0.5}}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	prevProd, prevDaily := antigravityDashboardBaseURL, antigravityDailyBaseURL
	antigravityDashboardBaseURL, antigravityDailyBaseURL = srv.URL, srv.URL
	t.Cleanup(func() {
		antigravityDashboardBaseURL, antigravityDailyBaseURL = prevProd, prevDaily
	})

	repo, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)

	const connID = "ag-badge-clear"
	if err := repo.CreateProviderConnectionFull(
		connID, "antigravity", "oauth", connID, nil, `{"accessToken":"tok","projectId":"proj"}`,
	); err != nil {
		t.Fatalf("seed connection: %v", err)
	}

	chat.ClearAntigravityIneligibleCache()
	t.Cleanup(chat.ClearAntigravityIneligibleCache)
	chat.RecordAntigravityEligibility(connID, &chat.AntigravityEligibility{
		ReasonCode:    "VALIDATION_REQUIRED",
		ValidationURL: "https://accounts.google.com/v3/signin/continue?authuser=1",
	})

	router := setupTestRouter(repo)
	req := httptest.NewRequest(http.MethodGet, "/api/usage/"+connID, nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if entry := chat.GetAntigravityIneligibility(connID); entry != nil {
		t.Errorf("a no-opinion reading left the flag in place: %+v", entry)
	}
	var out map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode usage response: %v", err)
	}
	if raw, present := out["ineligible"]; present {
		t.Errorf("ineligible = %v on a no-opinion reading, want the key absent", raw)
	}
}

// TestHandleGetConnectionUsage_AntigravityFreshVerdictWins: a reading that DOES
// carry a verdict is what the dashboard shows, cached or not.
func TestHandleGetConnectionUsage_AntigravityFreshVerdictWins(t *testing.T) {
	fastQuotaGate(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1internal:loadCodeAssist":
			_, _ = w.Write([]byte(`{"cloudaicompanionProject":"proj","currentTier":{"name":"Free"},
				"ineligibleTiers":[{"tierId":"free-tier","reasonCode":"VALIDATION_REQUIRED",
				"validationUrl":"https://accounts.google.com/fresh","reasonMessage":"not eligible"}]}`))
		case r.URL.Path == "/v1internal:fetchAvailableModels":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"reason":"VALIDATION_REQUIRED","metadata":{"validation_url":"https://accounts.google.com/fresh"}}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	prevProd, prevDaily := antigravityDashboardBaseURL, antigravityDailyBaseURL
	antigravityDashboardBaseURL, antigravityDailyBaseURL = srv.URL, srv.URL
	t.Cleanup(func() {
		antigravityDashboardBaseURL, antigravityDailyBaseURL = prevProd, prevDaily
	})

	repo, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)

	const connID = "ag-badge-fresh"
	if err := repo.CreateProviderConnectionFull(
		connID, "antigravity", "oauth", connID, nil, `{"accessToken":"tok","projectId":"proj"}`,
	); err != nil {
		t.Fatalf("seed connection: %v", err)
	}

	chat.ClearAntigravityIneligibleCache()
	t.Cleanup(chat.ClearAntigravityIneligibleCache)

	router := setupTestRouter(repo)
	req := httptest.NewRequest(http.MethodGet, "/api/usage/"+connID, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"ineligible"`) || !strings.Contains(body, `"eligible":false`) || !strings.Contains(body, "VALIDATION_REQUIRED") {
		t.Errorf("the fresh flagged verdict did not reach the dashboard: %s", body)
	}
	if entry := chat.GetAntigravityIneligibility(connID); entry == nil {
		t.Error("a flagged reading did not record the flag")
	}
}
