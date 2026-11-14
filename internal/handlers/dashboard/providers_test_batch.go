package dashboard

import (
	"context"
	json "encoding/json/v2"
	"io"
	"net/http"
	"time"

	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/models"
	"panrouter/proxy/internal/providers"
)

// Port of upstream src/app/api/providers/test-batch/route.js.
//
// The dashboard's "test all" buttons ask for one slice of the connection table
// at a time (a provider, an auth group, every compatible endpoint), so this
// route is a selector over the same per-connection probe the single-row Test
// button runs. Upstream awaits testSingleConnection in a plain for-loop, one
// connection at a time; the loop here is sequential for the same reason — a
// batch of OAuth probes fired concurrently opens one socket per account and
// trips provider-side per-IP limits, turning a working account into a "failed"
// row in the report.

// batchTestOutcome is what one per-connection probe reports back to the batch.
type batchTestOutcome struct {
	valid      bool
	latencyMs  int64
	message    string
	diagnosis  any
	statusCode any
	testedAt   string
}

// batchTestResult is one element of the response's `results` array. Fields
// upstream always emits (error, diagnosis, statusCode) are `any` so an absent
// value serializes as null instead of disappearing from the object.
type batchTestResult struct {
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
}

type batchTestSummary struct {
	Total  int `json:"total"`
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type batchTestResponse struct {
	Mode       string            `json:"mode"`
	ProviderID any               `json:"providerId"`
	Results    []batchTestResult `json:"results"`
	TestedAt   string            `json:"testedAt"`
	Summary    batchTestSummary  `json:"summary"`
}

type batchTestRequest struct {
	Mode       string `json:"mode"`
	ProviderID string `json:"providerId"`
}

// batchAuthGroup mirrors upstream getAuthGroup: the connection's own authType
// wins, then the registry's category, then the custom-endpoint prefix, and
// finally the apikey default.
func batchAuthGroup(providerID, authType string) string {
	if authType != "" {
		if authType == "oauth" {
			if meta, ok := providers.GetProviderMeta(providerID); ok && meta.Category == providers.CategoryFree {
				return "free"
			}
			return "oauth"
		}
		return authType
	}
	if meta, ok := providers.GetProviderMeta(providerID); ok {
		switch meta.Category {
		case providers.CategoryFree:
			return "free"
		case providers.CategoryOAuth:
			return "oauth"
		case providers.CategoryAPIKey:
			return "apikey"
		}
	}
	if providers.IsCustomCompatible(providerID) {
		return "compatible"
	}
	return "apikey"
}

// defaultBatchTest is the production probe: the same testSingleConnection +
// persistProbeResult pair POST /api/providers/{id}/test runs, so a batch and a
// single test can never disagree about the same connection.
func (h *DashboardHandler) defaultBatchTest(ctx context.Context, conn *models.ProviderConnection) batchTestOutcome {
	start := time.Now()
	raw := connectionDataMap(conn.Data)
	data := parseConnectionProbeData(raw)
	out := h.testSingleConnection(ctx, conn, data, raw)
	h.persistProbeResult(conn, raw, out)
	return batchTestOutcome{
		valid:     out.valid,
		latencyMs: time.Since(start).Milliseconds(),
		message:   out.message,
		testedAt:  time.Now().UTC().Format(time.RFC3339),
	}
}

// HandleTestProvidersBatch handles POST /api/providers/test-batch.
//
// Body: {"mode":"provider|oauth|free|apikey|compatible|all","providerId":"..."}.
func (h *DashboardHandler) HandleTestProvidersBatch(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		handlerutil.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read body"})
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req batchTestRequest
	_ = json.Unmarshal(body, &req)
	if req.Mode == "" {
		handlerutil.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "mode is required"})
		return
	}

	allConnections, err := h.Repo.GetProviderConnections("", true)
	if err != nil {
		handlerutil.WriteJSON(w, http.StatusInternalServerError, map[string]any{"error": "Batch test failed"})
		return
	}

	selected := make([]*models.ProviderConnection, 0, len(allConnections))
	switch req.Mode {
	case "provider":
		if req.ProviderID == "" {
			writeInvalidBatchMode(w)
			return
		}
		for _, conn := range allConnections {
			if conn.Provider == req.ProviderID {
				selected = append(selected, conn)
			}
		}
	case "oauth", "free", "apikey":
		for _, conn := range allConnections {
			if batchAuthGroup(conn.Provider, conn.AuthType) == req.Mode {
				selected = append(selected, conn)
			}
		}
	case "compatible":
		for _, conn := range allConnections {
			if providers.IsCustomCompatible(conn.Provider) {
				selected = append(selected, conn)
			}
		}
	case "all":
		selected = append(selected, allConnections...)
	default:
		writeInvalidBatchMode(w)
		return
	}

	tester := h.batchTest
	if tester == nil {
		tester = h.defaultBatchTest
	}

	results := make([]batchTestResult, 0, len(selected))
	passed := 0
	for _, conn := range selected {
		out := tester(r.Context(), conn)
		if out.valid {
			passed++
		}
		name := conn.Provider
		if conn.Name != nil && *conn.Name != "" {
			name = *conn.Name
		} else if conn.Email != nil && *conn.Email != "" {
			name = *conn.Email
		}
		authType := conn.AuthType
		if authType == "" {
			authType = batchAuthGroup(conn.Provider, conn.AuthType)
		}
		var errValue any
		if out.message != "" {
			errValue = out.message
		}
		testedAt := out.testedAt
		if testedAt == "" {
			testedAt = time.Now().UTC().Format(time.RFC3339)
		}
		results = append(results, batchTestResult{
			Provider:       conn.Provider,
			ConnectionID:   conn.ID,
			ConnectionName: name,
			AuthType:       authType,
			Valid:          out.valid,
			LatencyMs:      out.latencyMs,
			Error:          errValue,
			Diagnosis:      out.diagnosis,
			StatusCode:     out.statusCode,
			TestedAt:       testedAt,
		})
	}

	var providerID any
	if req.ProviderID != "" {
		providerID = req.ProviderID
	}
	handlerutil.WriteJSON(w, http.StatusOK, batchTestResponse{
		Mode:       req.Mode,
		ProviderID: providerID,
		Results:    results,
		TestedAt:   time.Now().UTC().Format(time.RFC3339),
		Summary: batchTestSummary{
			Total:  len(results),
			Passed: passed,
			Failed: len(results) - passed,
		},
	})
}

func writeInvalidBatchMode(w http.ResponseWriter) {
	handlerutil.WriteJSON(w, http.StatusBadRequest, map[string]any{
		"error": "Invalid mode. Use: provider, oauth, free, apikey, compatible, all",
	})
}
