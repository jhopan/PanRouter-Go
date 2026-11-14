package dashboard

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/proxy"
)

// Port of upstream src/app/api/providers/kilo/free-models/route.js.
//
// This is the Kilo registry lane's modelsFetcher (`kilocode` in
// internal/providers carries the same feed URL), served as a route because the
// dashboard's Kilo card lists the free models before any key is saved. Upstream
// keeps the result in a module-level cache with a one-hour TTL, answers
// `cached: true` on a hit and 502s only when it has nothing to fall back on.

const kiloFreeModelsTTL = time.Hour

// kiloFreeModelsURL is the gateway catalogue. A var so a test can aim it at an
// httptest fake instead of the live Kilo API.
var kiloFreeModelsURL = "https://api.kilo.ai/api/gateway/models"

// kiloFreeModelsHTTPClient is the fetch seam. The upstream route gives the
// request a 10s AbortSignal budget; the client carries the same ceiling so a
// hung gateway cannot hold the dashboard's card.
var kiloFreeModelsHTTPClient = &http.Client{
	Timeout:   10 * time.Second,
	Transport: proxy.NewFallbackTransport(http.DefaultTransport),
}

type kiloFreeModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	IsFree        bool   `json:"isFree"`
	ContextLength int64  `json:"context_length"`
}

var kiloFreeModelsCache struct {
	mu      sync.Mutex
	models  []kiloFreeModel
	fetched time.Time
}

// resetKiloFreeModelsCache drops the process-wide cache. Tests call it so one
// case's answer cannot be served to the next.
func resetKiloFreeModelsCache() {
	kiloFreeModelsCache.mu.Lock()
	kiloFreeModelsCache.models = nil
	kiloFreeModelsCache.fetched = time.Time{}
	kiloFreeModelsCache.mu.Unlock()
}

// fetchKiloFreeModels reads the catalogue and keeps the entries the payload
// itself marks free. Upstream tests `m.isFree === true`, so a missing or
// truthy-but-not-true flag is not free; the price-based heuristics other lanes
// use would report models Kilo itself bills for.
func fetchKiloFreeModels(ctx context.Context, url string) ([]kiloFreeModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := kiloFreeModelsHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Kilo API returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			IsFree        bool   `json:"isFree"`
			ContextLength int64  `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	models := make([]kiloFreeModel, 0, len(payload.Data))
	for _, m := range payload.Data {
		if !m.IsFree {
			continue
		}
		models = append(models, kiloFreeModel{
			ID:            m.ID,
			Name:          m.Name,
			IsFree:        true,
			ContextLength: m.ContextLength,
		})
	}
	return models, nil
}

// HandleGetKiloFreeModels handles GET /api/providers/kilo/free-models.
func (h *DashboardHandler) HandleGetKiloFreeModels(w http.ResponseWriter, r *http.Request) {
	now := time.Now()

	kiloFreeModelsCache.mu.Lock()
	cached := kiloFreeModelsCache.models
	fetched := kiloFreeModelsCache.fetched
	kiloFreeModelsCache.mu.Unlock()
	if cached != nil && now.Sub(fetched) < kiloFreeModelsTTL {
		handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
			"models": cached,
			"cached": true,
		})
		return
	}

	models, err := fetchKiloFreeModels(r.Context(), kiloFreeModelsURL)
	if err != nil {
		if cached != nil {
			// An expired cache beats an error: the models did not stop
			// existing, the fetch did.
			handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
				"models":  cached,
				"cached":  true,
				"warning": err.Error(),
			})
			return
		}
		handlerutil.WriteJSON(w, http.StatusBadGateway, map[string]any{
			"models": []kiloFreeModel{},
			"error":  "Failed to fetch Kilo models: " + err.Error(),
		})
		return
	}

	kiloFreeModelsCache.mu.Lock()
	kiloFreeModelsCache.models = models
	kiloFreeModelsCache.fetched = now
	kiloFreeModelsCache.mu.Unlock()

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"models": models,
		"cached": false,
	})
}
