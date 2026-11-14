package media

import (
	json "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/dbtest"
	"panrouter/proxy/internal/headroom"
)

// TestHeadroomExtrasCRUDRoundTrip drives the three verbs against an injected
// install store, so the contract is pinned without a Python 3.10 install on the
// test box. It is the route the dashboard's compression panel reads.
func TestHeadroomExtrasCRUDRoundTrip(t *testing.T) {
	installed := map[string]bool{}
	h := &HeadroomHandler{DataDir: t.TempDir()}
	h.extrasStatus = func(string) headroom.ExtrasStatus {
		if len(installed) == 0 {
			return headroom.ExtrasStatus{}
		}
		return headroom.ExtrasStatus{
			Installed: true,
			Version:   "1.2.3",
			Extras: headroom.Extras{
				Code: installed["code"],
				ML:   installed["ml"],
			},
		}
	}
	h.installExtras = func(_ string, extras []string) (map[string]any, error) {
		for _, extra := range extras {
			if extra == "code" || extra == "ml" {
				installed[extra] = true
			}
		}
		return map[string]any{"success": true, "spec": "headroom-ai[proxy,code]"}, nil
	}
	h.uninstallExtras = func(_ string, extras []string) (map[string]any, error) {
		removed := 0
		for _, extra := range extras {
			if installed[extra] {
				delete(installed, extra)
				removed++
			}
		}
		if removed == 0 {
			return nil, headroom.ErrInvalidExtras
		}
		return map[string]any{"success": true, "removed": extras}, nil
	}

	// GET before any install: upstream's shape, nothing installed.
	rec := httptest.NewRecorder()
	h.HandleHeadroomExtras(rec, httptest.NewRequest(http.MethodGet, "/api/headroom/extras", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	body := decodeHeadroomBody(t, rec)
	if arr, _ := body["available"].([]any); len(arr) != 2 {
		t.Errorf("available = %v, want the two compression extras", body["available"])
	}
	if body["installed"] != false {
		t.Errorf("installed = %v, want false", body["installed"])
	}

	// POST installs, GET reflects it.
	rec = httptest.NewRecorder()
	h.HandleHeadroomExtras(rec, httptest.NewRequest(http.MethodPost, "/api/headroom/extras",
		jsonBody(`{"extras":["code"]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.HandleHeadroomExtras(rec, httptest.NewRequest(http.MethodGet, "/api/headroom/extras", nil))
	body = decodeHeadroomBody(t, rec)
	if body["installed"] != true || body["version"] != "1.2.3" {
		t.Errorf("installed/version = %v/%v, want true/1.2.3", body["installed"], body["version"])
	}
	extras, _ := body["extras"].(map[string]any)
	if extras["code"] != true || extras["ml"] != false {
		t.Errorf("extras = %v, want code only", extras)
	}

	// DELETE removes it; GET reflects the removal.
	rec = httptest.NewRecorder()
	h.HandleHeadroomExtras(rec, httptest.NewRequest(http.MethodDelete, "/api/headroom/extras",
		jsonBody(`{"extras":["code"]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.HandleHeadroomExtras(rec, httptest.NewRequest(http.MethodGet, "/api/headroom/extras", nil))
	body = decodeHeadroomBody(t, rec)
	if body["installed"] != false {
		t.Errorf("installed = %v after removal, want false", body["installed"])
	}
}

// TestHeadroomExtrasErrorCodes keeps upstream's status/code split: a missing
// interpreter is a 400 NO_PYTHON, an empty uninstall set is a 400
// INVALID_EXTRAS, and anything else stays a 500.
func TestHeadroomExtrasErrorCodes(t *testing.T) {
	cases := []struct {
		name   string
		method string
		body   string
		status int
		code   string
		setup  func(h *HeadroomHandler)
	}{
		{
			name:   "install without python",
			method: http.MethodPost,
			body:   `{"extras":["code"]}`,
			status: http.StatusBadRequest,
			code:   "NO_PYTHON",
			setup: func(h *HeadroomHandler) {
				h.installExtras = func(string, []string) (map[string]any, error) {
					return nil, headroom.ErrNoPython
				}
			},
		},
		{
			name:   "uninstall nothing",
			method: http.MethodDelete,
			body:   `{"extras":["code"]}`,
			status: http.StatusBadRequest,
			code:   "INVALID_EXTRAS",
			setup: func(h *HeadroomHandler) {
				h.uninstallExtras = func(string, []string) (map[string]any, error) {
					return nil, headroom.ErrInvalidExtras
				}
			},
		},
		{
			name:   "install failure",
			method: http.MethodPost,
			body:   `{"extras":["code"]}`,
			status: http.StatusInternalServerError,
			code:   "INSTALL_FAILED",
			setup: func(h *HeadroomHandler) {
				h.installExtras = func(string, []string) (map[string]any, error) {
					return nil, errors.Join(headroom.ErrInstallFailed, errors.New("pip exited 1"))
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &HeadroomHandler{DataDir: t.TempDir()}
			tc.setup(h)
			rec := httptest.NewRecorder()
			h.HandleHeadroomExtras(rec, httptest.NewRequest(tc.method, "/api/headroom/extras", jsonBody(tc.body)))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			body := decodeHeadroomBody(t, rec)
			if body["code"] != tc.code {
				t.Errorf("code = %v, want %s", body["code"], tc.code)
			}
			if _, has := body["error"]; !has {
				t.Errorf("body = %v, want an error message", body)
			}
		})
	}
}

// TestHeadroomProxyForwardsUpstreamPathAndQuery pins the reverse proxy behind
// /api/headroom/proxy/{path}: the sub-path and query reach Headroom, and its
// answer comes back unchanged.
func TestHeadroomProxyForwardsUpstreamPathAndQuery(t *testing.T) {
	var seenPath, seenQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"upstream":true}`))
	}))
	defer srv.Close()

	database, err := db.OpenDatabase(filepath.Join(t.TempDir(), "headroom.sqlite"))
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if err := dbtest.CreateTables(database); err != nil {
		t.Fatalf("CreateTables: %v", err)
	}
	repo := db.NewRepo(database)
	if err := repo.UpdateSettingsRaw(map[string]any{"headroomUrl": srv.URL}); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	h := NewHeadroomHandler(repo)
	r := chi.NewRouter()
	r.HandleFunc("/api/headroom/proxy/*", h.HandleHeadroomProxy)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/headroom/proxy/dashboard/stats?window=5m", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if seenPath != "/dashboard/stats" {
		t.Errorf("upstream path = %q, want /dashboard/stats", seenPath)
	}
	if seenQuery != "window=5m" {
		t.Errorf("upstream query = %q, want window=5m", seenQuery)
	}
	if rec.Body.String() != `{"upstream":true}` {
		t.Errorf("body = %q, want the upstream body", rec.Body.String())
	}
}

func jsonBody(s string) io.Reader { return strings.NewReader(s) }

func decodeHeadroomBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}
