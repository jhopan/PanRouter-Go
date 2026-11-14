package dashboard

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type kiloModelsBody struct {
	Models []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		IsFree        bool   `json:"isFree"`
		ContextLength int64  `json:"context_length"`
	} `json:"models"`
	Cached  bool   `json:"cached"`
	Warning string `json:"warning"`
	Error   string `json:"error"`
}

// Port-of-upstream test for src/app/api/providers/kilo/free-models/route.js.
func TestKiloFreeModels(t *testing.T) {
	origURL, origClient := kiloFreeModelsURL, kiloFreeModelsHTTPClient
	t.Cleanup(func() {
		kiloFreeModelsURL, kiloFreeModelsHTTPClient = origURL, origClient
		resetKiloFreeModelsCache()
	})
	resetKiloFreeModelsCache()

	var status atomic.Int64
	status.Store(http.StatusOK)
	calls := atomic.Int64{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		if status.Load() != http.StatusOK {
			w.WriteHeader(int(status.Load()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"free/model-a","name":"Model A","isFree":true,"context_length":200000},
			{"id":"paid/model-b","name":"Model B","isFree":false,"context_length":400000},
			{"id":"paid/model-c","name":"Model C","context_length":1000}
		]}`))
	}))
	t.Cleanup(srv.Close)
	kiloFreeModelsURL = srv.URL
	kiloFreeModelsHTTPClient = srv.Client()

	h := NewDashboardHandler(nil)

	t.Run("first read fetches and filters to isFree", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleGetKiloFreeModels(rec, httptest.NewRequest(http.MethodGet, "/api/providers/kilo/free-models", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var body kiloModelsBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
		}
		if body.Cached {
			t.Error("cached = true on a cold cache")
		}
		if len(body.Models) != 1 {
			t.Fatalf("models = %d, want 1: %s", len(body.Models), rec.Body.String())
		}
		got := body.Models[0]
		if got.ID != "free/model-a" || got.Name != "Model A" || !got.IsFree || got.ContextLength != 200000 {
			t.Errorf("model = %+v, want the isFree entry with its context_length", got)
		}
		if calls.Load() != 1 {
			t.Errorf("upstream calls = %d, want 1", calls.Load())
		}
	})

	t.Run("second read answers from the cache", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleGetKiloFreeModels(rec, httptest.NewRequest(http.MethodGet, "/api/providers/kilo/free-models", nil))

		var body kiloModelsBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !body.Cached {
			t.Error("cached = false on the second read")
		}
		if len(body.Models) != 1 {
			t.Errorf("models = %d, want the cached 1", len(body.Models))
		}
		if calls.Load() != 1 {
			t.Errorf("upstream calls = %d, want 1 (no refetch inside the TTL)", calls.Load())
		}
	})

	t.Run("a failed refetch serves the expired cache with a warning", func(t *testing.T) {
		status.Store(http.StatusInternalServerError)
		// Expire the entry without waiting an hour: the route only compares the
		// stored timestamp against the TTL.
		kiloFreeModelsCache.mu.Lock()
		kiloFreeModelsCache.fetched = kiloFreeModelsCache.fetched.Add(-2 * kiloFreeModelsTTL)
		kiloFreeModelsCache.mu.Unlock()

		rec := httptest.NewRecorder()
		h.HandleGetKiloFreeModels(rec, httptest.NewRequest(http.MethodGet, "/api/providers/kilo/free-models", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var body kiloModelsBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !body.Cached || len(body.Models) != 1 {
			t.Errorf("cached = %v, models = %d, want the stale cache", body.Cached, len(body.Models))
		}
		if body.Warning == "" {
			t.Error("warning is empty, want the upstream failure")
		}
	})

	t.Run("no cache and a failing upstream is a 502", func(t *testing.T) {
		resetKiloFreeModelsCache()
		rec := httptest.NewRecorder()
		h.HandleGetKiloFreeModels(rec, httptest.NewRequest(http.MethodGet, "/api/providers/kilo/free-models", nil))

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
		}
		var body kiloModelsBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if body.Error == "" {
			t.Error("error is empty, want the fetch failure")
		}
		if body.Models == nil {
			t.Error("models is null, want an empty array")
		}
	})
}