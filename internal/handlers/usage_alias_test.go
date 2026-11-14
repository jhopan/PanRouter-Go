package handlers

import (
	"bytes"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"panrouter/proxy/internal/auth"
	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/middleware"
)

// seedUsageLogRows inserts usage rows in a fixed write order, so the log feed's
// ordering is asserted by row id rather than by a timestamp the fixture happens
// to share.
func seedUsageLogRows(t *testing.T, repo *db.Repo) {
	t.Helper()
	stamp := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC).Format(time.RFC3339)
	rows := []struct {
		model, provider, conn, status string
		prompt, completion            int
	}{
		{"gpt-4o", "openai", "conn-abcdefghijkl", "success", 120, 45},
		{"claude-3-5", "anthropic", "", "error", 7, 0},
	}
	for _, row := range rows {
		if _, err := repo.RawDB().Exec(
			`INSERT INTO usageHistory (timestamp, provider, model, connectionId, promptTokens, completionTokens, status, tokens)
			 VALUES (?, ?, ?, ?, ?, ?, ?, '{}')`,
			stamp, row.provider, row.model, row.conn, row.prompt, row.completion, row.status,
		); err != nil {
			t.Fatalf("seed usage row: %v", err)
		}
	}
	if _, err := repo.RawDB().Exec(
		`INSERT INTO providerConnections (id, provider, authType, name, isActive, data, createdAt, updatedAt)
		 VALUES ('conn-abcdefghijkl', 'openai', 'apikey', 'Primary OpenAI', 1, '{}', ?, ?)`,
		stamp, stamp,
	); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
}

// The log feed renders upstream's pipe-separated lines, newest row first, with
// the connection's friendly name in the account column.
func TestHandleUsageLogsFormatsTheFeed(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	seedUsageLogRows(t, repo)

	rec := httptest.NewRecorder()
	HandleUsageLogs(repo)(rec, httptest.NewRequest(http.MethodGet, "/api/usage/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	var lines []string
	if err := json.Unmarshal(rec.Body.Bytes(), &lines); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2: %v", len(lines), lines)
	}

	// Newest write first: the second row inserted is the error row.
	first := strings.Split(lines[0], " | ")
	second := strings.Split(lines[1], " | ")
	if len(first) != 7 || len(second) != 7 {
		t.Fatalf("expected 7 pipe-separated fields, got %v / %v", lines[0], lines[1])
	}

	stamp := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC).Format("02-01-2006 15:04:05")
	// The timestamp is rendered in the server's local zone, so the expected
	// value is computed the same way the handler does.
	if want := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC).Local().Format("02-01-2006 15:04:05"); first[0] != want {
		t.Errorf("timestamp = %q, want %q (UTC 09:30 rendered locally, %s)", first[0], want, stamp)
	}
	if first[1] != "claude-3-5" || first[2] != "ANTHROPIC" || first[6] != "error" {
		t.Errorf("error row = %v", first)
	}
	// No connection on that row: the account column is the placeholder.
	if first[3] != "-" {
		t.Errorf("account = %q, want - for a row with no connection", first[3])
	}
	if first[4] != "7" || first[5] != "0" {
		t.Errorf("tokens = %q/%q, want 7/0", first[4], first[5])
	}
	if second[1] != "gpt-4o" || second[2] != "OPENAI" || second[3] != "Primary OpenAI" {
		t.Errorf("success row = %v", second)
	}
	if second[4] != "120" || second[5] != "45" || second[6] != "success" {
		t.Errorf("success row tokens/status = %v", second)
	}
}

// Without a connection name the account column falls back to the id prefix,
// which is what tells two unnamed accounts apart in the feed.
func TestHandleUsageLogsAccountFallsBackToIDPrefix(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)

	if _, err := repo.RawDB().Exec(
		`INSERT INTO usageHistory (timestamp, provider, model, connectionId, promptTokens, completionTokens, status, tokens)
		 VALUES ('2026-11-15T09:30:00Z', 'openai', 'gpt-4o', 'abcdefgh12345678', 10, 5, 'ok', '{}')`,
	); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	rec := httptest.NewRecorder()
	HandleUsageLogs(repo)(rec, httptest.NewRequest(http.MethodGet, "/api/usage/request-logs", nil))
	var lines []string
	if err := json.Unmarshal(rec.Body.Bytes(), &lines); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if len(lines) != 1 {
		t.Fatalf("lines = %v, want one", lines)
	}
	if fields := strings.Split(lines[0], " | "); fields[3] != "abcdefgh" {
		t.Errorf("account = %q, want the first 8 characters of the connection id", fields[3])
	}
}

// An empty ledger answers [], not null: the dashboard iterates the value.
func TestHandleUsageLogsEmptyLedger(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)

	rec := httptest.NewRecorder()
	HandleUsageLogs(repo)(rec, httptest.NewRequest(http.MethodGet, "/api/usage/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Errorf("body = %q, want []", body)
	}
}

// /api/usage/history is the lifetime aggregate: upstream's route takes no
// parameters, so a caller's ?period is ignored rather than narrowing the
// window. A daily rollup row proves which window ran — it is only read by the
// daily (lifetime) path, never by `today`'s raw-history scan.
func TestHandleUsageHistoryIgnoresQueryPeriod(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)

	daily := `{"byProvider":{"openai":{"requests":4,"promptTokens":400,"completionTokens":40,"cost":0.01}}}`
	if _, err := repo.RawDB().Exec(
		`INSERT INTO usageDaily (dateKey, data) VALUES ('2026-10-01', ?)`, daily,
	); err != nil {
		t.Fatalf("seed usageDaily: %v", err)
	}

	for _, target := range []string{
		"/api/usage/history",
		"/api/usage/history?period=today",
		"/api/usage/history?period=24h&views=none",
	} {
		rec := httptest.NewRecorder()
		HandleUsageHistory(repo)(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %q)", target, rec.Code, rec.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: decode %q: %v", target, rec.Body.String(), err)
		}
		for _, key := range []string{"totalRequests", "totalPromptTokens", "byProvider", "byModel", "byApiKey", "recentRequests", "pending"} {
			if _, present := resp[key]; !present {
				t.Errorf("%s: response is missing %q", target, key)
			}
		}
		if got := resp["totalRequests"]; got != float64(4) {
			t.Errorf("%s: totalRequests = %v, want 4 from the daily rollup (the lifetime window)", target, got)
		}
		if got := resp["totalPromptTokens"]; got != float64(400) {
			t.Errorf("%s: totalPromptTokens = %v, want 400", target, got)
		}
		byProvider, _ := resp["byProvider"].(map[string]any)
		if _, present := byProvider["openai"]; !present {
			t.Errorf("%s: byProvider = %v, want openai", target, byProvider)
		}
	}
}

// The real server table must register every new route. The dashboard table is
// mounted by tests only, so a route added there alone answers "404 page not
// found" in production — the trap the smart-combo and per-node limit pairs were
// both caught by. The handler's own 404 (a JSON body) is what distinguishes
// "registered" from chi's plain-text 404.
func TestDashboardRouteTableServesTheNewRoutes(t *testing.T) {
	t.Setenv("JWT_SECRET", "dashboard-routes-test-secret")
	t.Setenv("DATA_DIR", t.TempDir())
	database, cleanup := setupTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)

	router := chi.NewRouter()
	SetupDashboardRoutes(router, repo, nil)

	cliToken := auth.CLIToken()
	cases := []struct {
		method, target string
		body           []byte
	}{
		{http.MethodGet, "/api/usage/history", nil},
		{http.MethodGet, "/api/usage/logs", nil},
		{http.MethodGet, "/api/usage/request-logs", nil},
		{http.MethodPost, "/api/usage/absent-connection/check-release", nil},
		{http.MethodGet, "/api/backup/config", nil},
		{http.MethodPost, "/api/backup/config", []byte(`{}`)},
		{http.MethodPost, "/api/backup/run", nil},
		{http.MethodGet, "/api/backup/tokens", nil},
		{http.MethodPost, "/api/backup/tokens", []byte(`{}`)},
		{http.MethodPatch, "/api/backup/tokens", []byte(`{}`)},
		{http.MethodDelete, "/api/backup/tokens?id=x", nil},
	}
	for _, tc := range cases {
		var reader *bytes.Reader
		if tc.body == nil {
			reader = bytes.NewReader(nil)
		} else {
			reader = bytes.NewReader(tc.body)
		}
		req := httptest.NewRequest(tc.method, tc.target, reader)
		req.Header.Set(auth.CLITokenHeader, cliToken)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "404 page not found") {
			t.Errorf("%s %s is not registered: %d %q", tc.method, tc.target, rec.Code, rec.Body.String())
		}
	}
}

// The download route is reachable without dashboard credentials: it verifies
// its own token. With no token at all it must answer the handler's 401, not the
// router's.
func TestDashboardRouteTableExposesBackupDownload(t *testing.T) {
	t.Setenv("JWT_SECRET", "dashboard-routes-test-secret")
	t.Setenv("DATA_DIR", t.TempDir())
	database, cleanup := setupTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)

	router := chi.NewRouter()
	SetupDashboardRoutes(router, repo, nil)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/backup/download", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the handler's 401 (body %q)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if resp["error"] != "Malformed token" {
		t.Errorf("error = %v, want Malformed token", resp["error"])
	}
}

// The new usage routes are dashboard reads and writes: behind the dashboard
// gate they answer 401 to an anonymous caller, exactly like their neighbours.
// The backup download is the exception and reaches its own token check.
func TestNewRoutesRefuseAnonymousCallers(t *testing.T) {
	t.Setenv("JWT_SECRET", "dashboard-routes-test-secret")
	t.Setenv("DATA_DIR", t.TempDir())
	database, cleanup := setupTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)

	router := chi.NewRouter()
	router.Group(func(r chi.Router) {
		r.Use(middleware.RequireDashboardAuth(repo))
		SetupDashboardRoutes(r, repo, nil)
	})

	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/api/usage/history"},
		{http.MethodGet, "/api/usage/logs"},
		{http.MethodGet, "/api/usage/request-logs"},
		{http.MethodPost, "/api/usage/absent/check-release"},
		{http.MethodGet, "/api/backup/config"},
		{http.MethodPost, "/api/backup/run"},
		{http.MethodGet, "/api/backup/tokens"},
		{http.MethodDelete, "/api/backup/tokens?id=x"},
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 without a dashboard credential", tc.method, tc.target, rec.Code)
		}
	}

	// The mirror route authenticates itself, so the gate admits it and the
	// handler answers.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/backup/download", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("download = %d, want the handler's 401 (body %q)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if resp["error"] != "Malformed token" {
		t.Errorf("download error = %v, want the token verdict", resp["error"])
	}
}
