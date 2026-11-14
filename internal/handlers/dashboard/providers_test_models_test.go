package dashboard

import (
	"context"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/providers"
)

// seedProviderTestConnection stores one connection row the models route can
// resolve. Only the provider matters to the handler: the id selects the
// catalogue, the connection row carries no credentials on this path.
func seedProviderTestConnection(t *testing.T, h *DashboardHandler, id, provider string) {
	t.Helper()
	if _, err := h.Repo.RawDB().Exec(
		`INSERT INTO providerConnections (id, provider, authType, name, isActive, data, createdAt, updatedAt)
		 VALUES (?, ?, 'apikey', ?, 1, '{}', '2026-11-15T00:00:00Z', '2026-11-15T00:00:00Z')`,
		id, provider, provider+" test",
	); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
}

// TestHandleTestProviderModels_ReportsPerModelVerdicts pins the route contract:
// every model of the connection's provider gets exactly one probe, the verdicts
// come back in catalogue order with null errors for the passes, and each model
// is probed through the service kind its endpoint serves.
func TestHandleTestProviderModels_ReportsPerModelVerdicts(t *testing.T) {
	repo, cleanup := setupTestDB(t)
	defer cleanup()

	h := NewDashboardHandler(repo)
	seedProviderTestConnection(t, h, "conn-mimo", "xiaomi-mimo")

	calls := map[string]int{}
	kinds := map[string]string{}
	h.SetModelProbe(func(_ context.Context, model, kind string) handlerutil.ModelProbeOutcome {
		calls[model]++
		kinds[model] = kind
		if strings.HasSuffix(model, "/mimo-v2.5") {
			return handlerutil.ModelProbeOutcome{OK: true, LatencyMs: 12, Status: http.StatusOK}
		}
		return handlerutil.ModelProbeOutcome{
			OK:        false,
			LatencyMs: 30,
			Status:    http.StatusBadGateway,
			Error:     "HTTP 502: upstream refused the call",
		}
	})

	router := setupTestRouterWith(repo, h)
	req := httptest.NewRequest(http.MethodPost, "/api/providers/conn-mimo/test-models", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Provider     string `json:"provider"`
		ConnectionID string `json:"connectionId"`
		Results      []struct {
			ModelID   string  `json:"modelId"`
			Name      string  `json:"name"`
			OK        bool    `json:"ok"`
			LatencyMs int64   `json:"latencyMs"`
			Error     *string `json:"error"`
			Status    int     `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Provider != "xiaomi-mimo" || resp.ConnectionID != "conn-mimo" {
		t.Errorf("provider/connectionId = %s/%s, want xiaomi-mimo/conn-mimo", resp.Provider, resp.ConnectionID)
	}

	catalogue := providers.GetProviderModels("mimo")
	if len(catalogue) == 0 {
		t.Fatal("provider catalogue is empty; the fixture no longer exercises the route")
	}
	if len(resp.Results) != len(catalogue) {
		t.Fatalf("results = %d, want one per catalogue model (%d)", len(resp.Results), len(catalogue))
	}
	for i, result := range resp.Results {
		if result.ModelID != catalogue[i] {
			t.Errorf("results[%d].modelId = %q, want %q", i, result.ModelID, catalogue[i])
		}
		if result.Name != result.ModelID {
			t.Errorf("results[%d].name = %q, want the model id", i, result.Name)
		}
		if got := calls["mimo/"+result.ModelID]; got != 1 {
			t.Errorf("%s probed %d times, want exactly 1", result.ModelID, got)
		}
		if result.OK && result.Error != nil {
			t.Errorf("%s passed but carries error %q", result.ModelID, *result.Error)
		}
		if !result.OK && (result.Error == nil || !strings.Contains(*result.Error, "HTTP 502")) {
			t.Errorf("%s failed without the probe's error: %+v", result.ModelID, result)
		}
	}
	if got := kinds["mimo/mimo-v2.5-tts"]; got != "tts" {
		t.Errorf("mimo-v2.5-tts probed as kind %q, want tts", got)
	}
	if got := kinds["mimo/mimo-v2.5"]; got != "llm" {
		t.Errorf("mimo-v2.5 probed as kind %q, want llm", got)
	}
}

// TestHandleTestProviderModels_Refusals pins the two refusals: an unknown
// connection is a 404, and a provider with no catalogue is a 400 — neither may
// answer an empty result list that looks like a measurement.
func TestHandleTestProviderModels_Refusals(t *testing.T) {
	repo, cleanup := setupTestDB(t)
	defer cleanup()

	h := NewDashboardHandler(repo)
	h.SetModelProbe(func(context.Context, string, string) handlerutil.ModelProbeOutcome {
		t.Fatal("no probe may run for a refused request")
		return handlerutil.ModelProbeOutcome{}
	})
	seedProviderTestConnection(t, h, "conn-empty", "no-such-provider")

	router := setupTestRouterWith(repo, h)
	tests := []struct {
		name     string
		path     string
		want     int
		wantText string
	}{
		{
			name:     "unknown connection",
			path:     "/api/providers/conn-missing/test-models",
			want:     http.StatusNotFound,
			wantText: "Connection not found",
		},
		{
			name:     "no catalogue",
			path:     "/api/providers/conn-empty/test-models",
			want:     http.StatusBadRequest,
			wantText: "No models configured for this provider",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tt.path, nil))
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantText) {
				t.Errorf("response %s is missing %q", rec.Body.String(), tt.wantText)
			}
		})
	}
}
