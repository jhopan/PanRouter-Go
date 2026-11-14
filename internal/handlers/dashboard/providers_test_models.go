package dashboard

import (
	"context"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"

	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/providers"
)

// Port of upstream src/app/api/providers/[id]/test-models/route.js.
//
// The connection id is used only to resolve the provider and its catalogue; the
// probes themselves run through the per-model probe the dashboard's Test button
// uses (media.MediaHandler.ProbeModel), which dispatches each model through the
// endpoint its kind serves. That is what keeps this route honest: a model it
// reports as failing is a model real traffic also fails, and every probe is
// marked with handlerutil.WithProbeContext so none of it reaches the usage
// ledger or the account's health counters.

// providerModelTestItem is one element of the response's `results` array.
// `error` is `any` so a passing model serializes it as null instead of dropping
// the key, and `note` only appears when the probe produced one.
type providerModelTestItem struct {
	ModelID   string `json:"modelId"`
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latencyMs"`
	Error     any    `json:"error"`
	Status    int    `json:"status"`
	Note      string `json:"note,omitempty"`
}

type providerTestModelsResponse struct {
	Provider     string                  `json:"provider"`
	ConnectionID string                  `json:"connectionId"`
	Results      []providerModelTestItem `json:"results"`
}

// providerTestModel is one catalogue entry as the registry (or a live fetch)
// describes it.
type providerTestModel struct {
	id   string
	name string
}

// HandleTestProviderModels probes every model of the connection's provider and
// reports one verdict per model.
//
// POST /api/providers/{id}/test-models
func (h *DashboardHandler) HandleTestProviderModels(w http.ResponseWriter, r *http.Request) {
	id := getURLParam(r, "id")
	conn, err := h.Repo.GetProviderConnectionByID(id)
	if err != nil || conn == nil {
		handlerutil.WriteJSONError(w, http.StatusNotFound, "Connection not found")
		return
	}

	alias := providerModelAlias(conn.Provider)
	models := providerTestModels(alias, conn.Provider)

	// Compatible providers publish no static catalogue: ask the models route
	// for the live one, exactly as upstream calls its own endpoint.
	if len(models) == 0 && providers.IsCustomCompatible(conn.Provider) {
		models = h.liveProviderModels(r.Context(), id)
	}
	if len(models) == 0 {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "No models configured for this provider")
		return
	}

	probe := h.modelProbe
	if probe == nil {
		// A handler built outside a boot has no probe wired. Refusing beats
		// inventing verdicts that look like measurements.
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "Test failed")
		return
	}

	results := make([]providerModelTestItem, 0, len(models))
	for _, model := range models {
		kind := providers.GetProviderModelKind(alias, model.id)
		if kind == "" {
			kind = "llm"
		}
		outcome := probe(r.Context(), alias+"/"+model.id, kind)
		var errValue any
		if outcome.Error != "" {
			errValue = outcome.Error
		}
		results = append(results, providerModelTestItem{
			ModelID:   model.id,
			Name:      model.name,
			OK:        outcome.OK,
			LatencyMs: outcome.LatencyMs,
			Error:     errValue,
			Status:    outcome.Status,
			Note:      outcome.Note,
		})
	}

	handlerutil.WriteJSON(w, http.StatusOK, providerTestModelsResponse{
		Provider:     conn.Provider,
		ConnectionID: id,
		Results:      results,
	})
}

// providerModelAlias is the routing prefix a model id is published under:
// the registry alias, else the provider id itself.
func providerModelAlias(providerID string) string {
	if alias := providers.RegistryAliases[providerID]; alias != "" {
		return alias
	}
	return providerID
}

// providerTestModels reads the catalogue for a provider and returns one entry
// per distinct model, so a registry that lists an id twice cannot make the
// route send two probes for it.
func providerTestModels(alias, providerID string) []providerTestModel {
	ids := providers.GetProviderModels(alias)
	if len(ids) == 0 && providerID != alias {
		ids = providers.GetProviderModels(providerID)
	}
	seen := make(map[string]bool, len(ids))
	out := make([]providerTestModel, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, providerTestModel{id: id, name: id})
	}
	return out
}

// liveProviderModels runs the existing GET /api/providers/{id}/models handler
// in-process and reads its model list. Reusing the handler keeps one discovery
// chain for compatible nodes instead of a second copy that could drift from the
// one the dashboard imports models with.
func (h *DashboardHandler) liveProviderModels(ctx context.Context, connectionID string) []providerTestModel {
	req := httptest.NewRequest(http.MethodGet, "/api/providers/"+connectionID+"/models", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.writeConnectionModels(rec, req, connectionID)
	if rec.Code != http.StatusOK {
		return nil
	}
	var payload struct {
		Models []any `json:"models"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &payload) != nil {
		return nil
	}
	out := make([]providerTestModel, 0, len(payload.Models))
	seen := make(map[string]bool, len(payload.Models))
	for _, raw := range payload.Models {
		id, name := liveModelEntry(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, providerTestModel{id: id, name: name})
	}
	return out
}

// liveModelEntry reads one entry of a `models` array. The discovery handlers
// answer objects with id/name, but a passthrough provider's list is forwarded
// verbatim and may hold bare strings.
func liveModelEntry(raw any) (string, string) {
	switch v := raw.(type) {
	case string:
		return v, v
	case map[string]any:
		id, _ := v["id"].(string)
		if id == "" {
			id, _ = v["name"].(string)
		}
		name, _ := v["name"].(string)
		if name == "" {
			name, _ = v["id"].(string)
		}
		return id, name
	}
	return "", ""
}

// SetModelProbe wires the per-model probe this route and its tests use.
func (h *DashboardHandler) SetModelProbe(probe func(ctx context.Context, model, kind string) handlerutil.ModelProbeOutcome) {
	h.modelProbe = probe
}
