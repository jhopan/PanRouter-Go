package dashboard

import (
	"context"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/models"
)

// batchTestBody is the decoded response shape the assertions read.
type batchTestBody struct {
	Mode       string `json:"mode"`
	ProviderID any    `json:"providerId"`
	Results    []struct {
		Provider       string `json:"provider"`
		ConnectionID   string `json:"connectionId"`
		ConnectionName string `json:"connectionName"`
		AuthType       string `json:"authType"`
		Valid          bool   `json:"valid"`
		LatencyMs      int64  `json:"latencyMs"`
		Error          any    `json:"error"`
		Diagnosis      any    `json:"diagnosis"`
		StatusCode     any    `json:"statusCode"`
		TestedAt       string `json:"testedAt"`
	} `json:"results"`
	TestedAt string `json:"testedAt"`
	Summary  struct {
		Total  int `json:"total"`
		Passed int `json:"passed"`
		Failed int `json:"failed"`
	} `json:"summary"`
}

// fakeBatchSender counts one attempt per connection id and answers from a
// table, so the test can assert "exactly one upstream attempt per connection"
// without a network, a goroutine or a sleep.
func fakeBatchSender(t *testing.T, repo *db.Repo, valid map[string]bool) (*DashboardHandler, map[string]int) {
	t.Helper()
	attempts := map[string]int{}
	h := NewDashboardHandler(repo)
	h.batchTest = func(ctx context.Context, conn *models.ProviderConnection) batchTestOutcome {
		attempts[conn.ID]++
		if valid[conn.ID] {
			return batchTestOutcome{valid: true, latencyMs: 7, testedAt: "2026-11-15T00:00:00Z"}
		}
		return batchTestOutcome{
			valid:     false,
			latencyMs: 3,
			message:   "no key",
			testedAt:  "2026-11-15T00:00:00Z",
		}
	}
	return h, attempts
}

func TestProvidersTestBatch(t *testing.T) {
	repo, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)

	create := func(id, provider, authType, name string, active bool) {
		t.Helper()
		data := `{"apiKey":"k"}`
		if err := repo.CreateProviderConnectionFull(id, provider, authType, name, nil, data); err != nil {
			t.Fatalf("create connection %s: %v", id, err)
		}
		if !active {
			conn, err := repo.GetProviderConnectionByID(id)
			if err != nil || conn == nil {
				t.Fatalf("reload connection %s: %v", id, err)
			}
			if err := repo.SetConnectionStatus(id, false); err != nil {
				t.Fatalf("disable connection %s: %v", id, err)
			}
		}
	}

	create("c1", "openai", "apikey", "OpenAI Primary", true)
	create("c2", "kiro", "oauth", "", true) // registry category "free"
	create("c3", "openai-compatible-mine", "compatible", "Mine", true)
	create("c4", "anthropic", "apikey", "Disabled", false)

	valid := map[string]bool{"c1": true}

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantError  string
		wantConns  []string
		wantPassed int
		wantFailed int
	}{
		{
			name:       "all active connections, one attempt each",
			body:       `{"mode":"all"}`,
			wantStatus: http.StatusOK,
			wantConns:  []string{"c1", "c2", "c3"},
			wantPassed: 1,
			wantFailed: 2,
		},
		{
			name:       "single provider",
			body:       `{"mode":"provider","providerId":"openai"}`,
			wantStatus: http.StatusOK,
			wantConns:  []string{"c1"},
			wantPassed: 1,
		},
		{
			name:       "oauth group",
			body:       `{"mode":"compatible"}`,
			wantStatus: http.StatusOK,
			wantConns:  []string{"c3"},
			wantFailed: 1,
		},
		{
			// kiro is categoryFree and its connection says oauth; upstream
			// upgrades that pair to the "free" group.
			name:       "free group",
			body:       `{"mode":"free"}`,
			wantStatus: http.StatusOK,
			wantConns:  []string{"c2"},
			wantFailed: 1,
		},
		{
			name:       "apikey group skips oauth and compatible rows",
			body:       `{"mode":"apikey"}`,
			wantStatus: http.StatusOK,
			wantConns:  []string{"c1"},
			wantPassed: 1,
		},
		{
			name:       "provider mode needs an id",
			body:       `{"mode":"provider"}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "Invalid mode. Use: provider, oauth, free, apikey, compatible, all",
		},
		{
			name:       "unknown mode is refused",
			body:       `{"mode":"whatever"}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "Invalid mode. Use: provider, oauth, free, apikey, compatible, all",
		},
		{
			name:       "missing mode is refused",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "mode is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, attempts := fakeBatchSender(t, repo, valid)
			router := setupTestRouterWith(repo, h)

			req := httptest.NewRequest(http.MethodPost, "/api/providers/test-batch", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantError != "" {
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if body["error"] != tt.wantError {
					t.Errorf("error = %v, want %q", body["error"], tt.wantError)
				}
				if len(attempts) != 0 {
					t.Errorf("refused request ran %d probes, want 0", len(attempts))
				}
				return
			}

			var body batchTestBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
			}
			got := make([]string, 0, len(body.Results))
			for _, result := range body.Results {
				got = append(got, result.ConnectionID)
				if result.TestedAt == "" {
					t.Errorf("connection %s: testedAt is empty", result.ConnectionID)
				}
			}
			if strings.Join(got, ",") != strings.Join(tt.wantConns, ",") {
				t.Errorf("connections = %v, want %v", got, tt.wantConns)
			}
			// "One upstream attempt per connection": no retries, no duplicates.
			for _, id := range tt.wantConns {
				if attempts[id] != 1 {
					t.Errorf("connection %s probed %d times, want exactly 1", id, attempts[id])
				}
			}
			if len(attempts) != len(tt.wantConns) {
				t.Errorf("probed %d connections, want %d (%v)", len(attempts), len(tt.wantConns), attempts)
			}
			if body.Summary.Total != len(tt.wantConns) {
				t.Errorf("summary.total = %d, want %d", body.Summary.Total, len(tt.wantConns))
			}
			if body.Summary.Passed != tt.wantPassed {
				t.Errorf("summary.passed = %d, want %d", body.Summary.Passed, tt.wantPassed)
			}
			if body.Summary.Failed != tt.wantFailed {
				t.Errorf("summary.failed = %d, want %d", body.Summary.Failed, tt.wantFailed)
			}
			if body.TestedAt == "" {
				t.Error("testedAt is empty")
			}
		})
	}

	t.Run("empty selection answers an empty report", func(t *testing.T) {
		h, attempts := fakeBatchSender(t, repo, valid)
		router := setupTestRouterWith(repo, h)

		req := httptest.NewRequest(http.MethodPost, "/api/providers/test-batch",
			strings.NewReader(`{"mode":"provider","providerId":"nowhere"}`))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var body batchTestBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(body.Results) != 0 || body.Summary.Total != 0 {
			t.Errorf("results = %d, summary.total = %d, want 0/0", len(body.Results), body.Summary.Total)
		}
		if body.ProviderID != "nowhere" {
			t.Errorf("providerId = %v, want the requested id", body.ProviderID)
		}
		if len(attempts) != 0 {
			t.Errorf("empty selection ran %d probes, want 0", len(attempts))
		}
	})
}

// TestBatchAuthGroup pins the classification order upstream uses: the
// connection's own authType wins, then the registry category, then the custom
// endpoint prefix, then the apikey default.
func TestBatchAuthGroup(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		authType string
		want     string
	}{
		{name: "oauth connection on a free provider is free", provider: "kiro", authType: "oauth", want: "free"},
		{name: "oauth connection on an oauth provider", provider: "anthropic", authType: "oauth", want: "oauth"},
		{name: "explicit authType passes through", provider: "openai", authType: "cookie", want: "cookie"},
		{name: "registry oauth category", provider: "claude", want: "oauth"},
		{name: "registry apikey category", provider: "openai", want: "apikey"},
		{name: "compatible prefix", provider: "anthropic-compatible-edge", want: "compatible"},
		{name: "unknown provider defaults to apikey", provider: "some-private-gateway", want: "apikey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := batchAuthGroup(tt.provider, tt.authType); got != tt.want {
				t.Errorf("batchAuthGroup(%q, %q) = %q, want %q", tt.provider, tt.authType, got, tt.want)
			}
		})
	}
}
