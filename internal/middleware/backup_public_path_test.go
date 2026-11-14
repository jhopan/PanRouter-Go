package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"panrouter/proxy/internal/db"
)

// The database mirror holds a backup token, never a dashboard session, so the
// gate has to let GET /api/backup/download reach the handler that verifies that
// token. The exception is scoped to that one path: every sibling backup route
// still needs the dashboard credential, so a machine with a mirror token cannot
// read the config, mint another token, or start a backup.
func TestBackupDownloadIsPublicAndSiblingsAreNot(t *testing.T) {
	t.Setenv("JWT_SECRET", "middleware-test-secret")
	t.Setenv("DATA_DIR", t.TempDir())
	database, cleanup := setupTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)

	gate := RequireDashboardAuth(repo)(okHandler())
	serve := func(method, path string) int {
		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec.Code
	}

	// Login required (the default).
	if code := serve(http.MethodGet, PublicBackupDownloadPath); code != http.StatusOK {
		t.Errorf("GET %s = %d, want 200 — the handler verifies the backup token itself", PublicBackupDownloadPath, code)
	}
	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, "/api/backup/config"},
		{http.MethodPost, "/api/backup/config"},
		{http.MethodPost, "/api/backup/run"},
		{http.MethodGet, "/api/backup/tokens"},
		{http.MethodPost, "/api/backup/tokens"},
		{http.MethodDelete, "/api/backup/tokens"},
	} {
		if code := serve(tt.method, tt.path); code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 without the dashboard credential", tt.method, tt.path, code)
		}
	}

	// An open install (login disabled) still reaches the download, and the rest
	// follows the ordinary rule for that mode.
	if err := repo.UpdateSettingsRaw(map[string]any{"requireLogin": false}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	if code := serve(http.MethodGet, PublicBackupDownloadPath); code != http.StatusOK {
		t.Errorf("open install: GET %s = %d, want 200", PublicBackupDownloadPath, code)
	}
	if code := serve(http.MethodGet, "/api/backup/config"); code != http.StatusOK {
		t.Errorf("open install: GET /api/backup/config = %d, want 200", code)
	}
}