package dashboard

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
)

// upstream: GET /api/pricing answers getPricing() — the provider → model →
// rate table, merged with the operator's overrides. This gateway stores no
// overrides, so the table itself is the answer.
func TestHandleGetPricing(t *testing.T) {
	h := &DashboardHandler{}
	rec := httptest.NewRecorder()
	h.HandleGetPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var table map[string]map[string]map[string]float64
	if err := json.Unmarshal(rec.Body.Bytes(), &table); err != nil {
		t.Fatalf("body is not a provider → model → rate object: %v", err)
	}
	if len(table) == 0 {
		t.Fatal("pricing table is empty")
	}

	for _, provider := range []string{"gh", "tokenrouter", "mistral"} {
		if _, ok := table[provider]; !ok {
			t.Errorf("provider %q is missing from the payload", provider)
		}
	}

	// A fixed row pins both the key set and the unit ($/1M, not per token).
	got, ok := table["gh"]["gpt-5.3-codex"]
	if !ok {
		t.Fatal("gh/gpt-5.3-codex is missing from the payload")
	}
	want := map[string]float64{
		"input":          1.75,
		"output":         14,
		"cached":         0.175,
		"reasoning":      14,
		"cache_creation": 1.75,
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("gh/gpt-5.3-codex.%s = %v, want %v", key, got[key], value)
		}
	}
	for _, key := range []string{"input", "output", "cached", "reasoning", "cache_creation"} {
		if _, ok := got[key]; !ok {
			t.Errorf("rate object is missing the %q key", key)
		}
	}
}

// A caller must not be able to reach the process-wide tables through the
// response: mutating it changes nothing for the next request.
func TestHandleGetPricingReturnsACopy(t *testing.T) {
	h := &DashboardHandler{}

	first := httptest.NewRecorder()
	h.HandleGetPricing(first, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
	second := httptest.NewRecorder()
	h.HandleGetPricing(second, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))

	if first.Body.String() != second.Body.String() {
		t.Error("two calls answered differently; the handler is not reading a stable table")
	}
}
