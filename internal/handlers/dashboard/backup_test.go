package dashboard

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"panrouter/proxy/internal/auth"
	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/dbbackup"
	"panrouter/proxy/internal/middleware"
)

// authorizedRequest performs a request with the local CLI token, which is the
// credential a scripted operator (and every dashboard test) has without a
// session cookie.
func authorizedRequest(t *testing.T, handler http.HandlerFunc, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set(cliTokenHeader, auth.CLIToken())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func decodeBodyMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return out
}

// seedBackupSettings stores a dbBackup block the way the POST route does.
func seedBackupSettings(t *testing.T, repo *db.Repo, block map[string]any) {
	t.Helper()
	if err := repo.UpdateSettingsRaw(map[string]any{"dbBackup": block}); err != nil {
		t.Fatalf("seed dbBackup: %v", err)
	}
}

func storedBackupBlock(t *testing.T, repo *db.Repo) map[string]any {
	t.Helper()
	raw, err := repo.GetSettingsRaw()
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	block, _ := raw["dbBackup"].(map[string]any)
	return block
}

// The config read reports the stored setup and never its secret.
func TestBackupConfigGET(t *testing.T) {
	authTestEnv(t)
	repo, cleanup := setupTestDB(t)
	defer cleanup()
	h := NewDashboardHandler(repo)

	// No credential at all: the upstream gate answers 401 "Invalid password".
	rec := httptest.NewRecorder()
	h.HandleGetBackupConfig(rec, httptest.NewRequest(http.MethodGet, "/api/backup/config", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}
	if got := decodeBodyMap(t, rec)["error"]; got != "Invalid password" {
		t.Errorf("error = %v, want Invalid password", got)
	}

	seedBackupSettings(t, repo, map[string]any{
		"enabled":         true,
		"intervalHours":   float64(6),
		"githubUser":      "alice",
		"githubRepo":      "backups",
		"githubRepoOwner": "alice",
		"githubToken":     "ghp_secret_value",
	})

	rec = authorizedRequest(t, h.HandleGetBackupConfig, http.MethodGet, "/api/backup/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	got := decodeBodyMap(t, rec)
	want := map[string]any{
		"enabled":               true,
		"intervalHours":         float64(6),
		"githubUser":            "alice",
		"githubRepo":            "backups",
		"githubRepoOwner":       "alice",
		"githubRepoUrl":         "https://github.com/alice/backups",
		"githubTokenConfigured": true,
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %v, want %v", key, got[key], value)
		}
	}
	if _, present := got["githubToken"]; present {
		t.Error("GET /api/backup/config returned the token field")
	}
	if strings.Contains(rec.Body.String(), "ghp_secret_value") {
		t.Error("GET /api/backup/config leaked the GitHub token")
	}
}

// The legacy "owner/name" shape still reports an owner, so the dashboard's URL
// column is filled for a config written by an older build.
func TestBackupConfigGETLegacyRepoShape(t *testing.T) {
	authTestEnv(t)
	repo, cleanup := setupTestDB(t)
	defer cleanup()
	h := NewDashboardHandler(repo)

	seedBackupSettings(t, repo, map[string]any{
		"githubUser": "bob",
		"githubRepo": "bob/repo-from-url",
	})

	rec := authorizedRequest(t, h.HandleGetBackupConfig, http.MethodGet, "/api/backup/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	got := decodeBodyMap(t, rec)
	if got["githubRepoOwner"] != "bob" || got["githubRepo"] != "repo-from-url" {
		t.Errorf("owner/repo = %v/%v, want bob/repo-from-url", got["githubRepoOwner"], got["githubRepo"])
	}
	if got["githubRepoUrl"] != "https://github.com/bob/repo-from-url" {
		t.Errorf("githubRepoUrl = %v", got["githubRepoUrl"])
	}
	if got["githubTokenConfigured"] != false {
		t.Errorf("githubTokenConfigured = %v, want false", got["githubTokenConfigured"])
	}
}

// The config write validates, checks the repo is private, and stores the token.
func TestBackupConfigPOST(t *testing.T) {
	authTestEnv(t)
	repo, cleanup := setupTestDB(t)
	defer cleanup()
	h := NewDashboardHandler(repo)
	defer h.Backup.Stop()

	var checked struct {
		owner, repo, token string
		calls              int
	}
	h.Backup.CheckRepoPrivate = func(_ context.Context, owner, repo, token string) error {
		checked.calls++
		checked.owner, checked.repo, checked.token = owner, repo, token
		return nil
	}

	body, _ := json.Marshal(map[string]any{
		"enabled":       true,
		"intervalHours": 3,
		"githubUser":    "alice",
		"githubRepo":    "https://github.com/alice/backups.git",
		"githubToken":   "ghp_new_value",
	})

	rec := authorizedRequest(t, h.HandlePostBackupConfig, http.MethodPost, "/api/backup/config", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	got := decodeBodyMap(t, rec)
	want := map[string]any{
		"success":               true,
		"enabled":               true,
		"intervalHours":         float64(3),
		"githubUser":            "alice",
		"githubRepo":            "backups",
		"githubRepoOwner":       "alice",
		"githubTokenConfigured": true,
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %v, want %v", key, got[key], value)
		}
	}
	if strings.Contains(rec.Body.String(), "ghp_new_value") {
		t.Error("POST /api/backup/config echoed the token")
	}

	if checked.calls != 1 || checked.owner != "alice" || checked.repo != "backups" || checked.token != "ghp_new_value" {
		t.Errorf("privacy check got %+v, want one call for alice/backups with the submitted token", checked)
	}

	stored := storedBackupBlock(t, repo)
	if stored["githubToken"] != "ghp_new_value" || stored["githubRepo"] != "backups" || stored["enabled"] != true {
		t.Errorf("stored dbBackup = %v, want the submitted config", stored)
	}

	// An empty token means "keep the stored one" — the form never shows it.
	body, _ = json.Marshal(map[string]any{
		"enabled":    false,
		"githubUser": "alice",
		"githubRepo": "backups",
	})
	rec = authorizedRequest(t, h.HandlePostBackupConfig, http.MethodPost, "/api/backup/config", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST without token status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if stored := storedBackupBlock(t, repo); stored["githubToken"] != "ghp_new_value" {
		t.Errorf("stored token = %v, want the previous one kept", stored["githubToken"])
	}
}

// Every refusal in the config write answers upstream's status and message.
func TestBackupConfigPOSTRejections(t *testing.T) {
	authTestEnv(t)
	repo, cleanup := setupTestDB(t)
	defer cleanup()
	h := NewDashboardHandler(repo)
	defer h.Backup.Stop()

	private := true
	h.Backup.CheckRepoPrivate = func(_ context.Context, owner, repo, token string) error {
		if !private {
			return errors.New("GitHub: repo " + owner + "/" + repo + " is NOT private — refusing to upload the database")
		}
		return nil
	}

	tests := []struct {
		name       string
		body       map[string]any
		wantStatus int
		wantError  string
	}{
		{
			name:       "no credential",
			body:       nil,
			wantStatus: http.StatusUnauthorized,
			wantError:  "Invalid password",
		},
		{
			name:       "missing username and repo",
			body:       map[string]any{"githubToken": "ghp_x"},
			wantStatus: http.StatusBadRequest,
			wantError:  "GitHub username and repository are required",
		},
		{
			name:       "missing token",
			body:       map[string]any{"githubUser": "alice", "githubRepo": "backups"},
			wantStatus: http.StatusBadRequest,
			wantError:  "GitHub token is required",
		},
		{
			name:       "invalid repo name",
			body:       map[string]any{"githubUser": "alice", "githubRepo": "owner/bad name", "githubToken": "ghp_x"},
			wantStatus: http.StatusBadRequest,
			wantError:  "Invalid repository name",
		},
		{
			name:       "public repo",
			body:       map[string]any{"githubUser": "alice", "githubRepo": "backups", "githubToken": "ghp_x"},
			wantStatus: http.StatusBadRequest,
			wantError:  "is NOT private",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "no credential" {
				rec := httptest.NewRecorder()
				h.HandlePostBackupConfig(rec, httptest.NewRequest(http.MethodPost, "/api/backup/config", nil))
				if rec.Code != tt.wantStatus {
					t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
				}
				if got := decodeBodyMap(t, rec)["error"]; !strings.Contains(jsonText(got), tt.wantError) {
					t.Errorf("error = %v, want %q", got, tt.wantError)
				}
				return
			}

			// The public-repo case is the only one that reaches the check.
			private = tt.name != "public repo"
			body, _ := json.Marshal(tt.body)
			rec := authorizedRequest(t, h.HandlePostBackupConfig, http.MethodPost, "/api/backup/config", body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := decodeBodyMap(t, rec)["error"]; !strings.Contains(jsonText(got), tt.wantError) {
				t.Errorf("error = %v, want %q", got, tt.wantError)
			}
		})
	}
}

// jsonText renders an untrusted decoded value for a substring assertion.
func jsonText(v any) string {
	s, _ := v.(string)
	return s
}

// "Back up now" answers the pushed snapshot, and a refused setup is a 400 with
// the reason.
func TestBackupRun(t *testing.T) {
	authTestEnv(t)
	repo, cleanup := setupTestDB(t)
	defer cleanup()
	h := NewDashboardHandler(repo)
	defer h.Backup.Stop()

	// No credential.
	rec := httptest.NewRecorder()
	h.HandleRunBackup(rec, httptest.NewRequest(http.MethodPost, "/api/backup/run", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: status = %d, want 401", rec.Code)
	}

	// Nothing configured yet.
	rec = authorizedRequest(t, h.HandleRunBackup, http.MethodPost, "/api/backup/run", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unconfigured: status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if got := decodeBodyMap(t, rec)["error"]; got != "GitHub username, token, or repo is not configured" {
		t.Errorf("error = %v", got)
	}

	seedBackupSettings(t, repo, map[string]any{
		"enabled":     false,
		"githubUser":  "alice",
		"githubRepo":  "alice/backups",
		"githubToken": "ghp_run",
	})
	h.Backup.Now = func() time.Time { return time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC) }
	h.Backup.CheckRepoPrivate = func(_ context.Context, owner, repo, token string) error { return nil }
	h.Backup.Snapshot = func(_ context.Context, dest string) error {
		return os.WriteFile(dest, []byte("snapshot-bytes"), 0o600)
	}
	var pushed dbbackup.PushRequest
	h.Backup.Push = func(_ context.Context, req dbbackup.PushRequest) (int, error) {
		pushed = req
		return len(req.Content), nil
	}

	rec = authorizedRequest(t, h.HandleRunBackup, http.MethodPost, "/api/backup/run", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("run: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	got := decodeBodyMap(t, rec)
	if got["success"] != true || got["fileName"] != "panrouter-db-2026-11-15.sqlite" {
		t.Errorf("body = %v, want success with the dated snapshot name", got)
	}
	if got["bytes"] != float64(len("snapshot-bytes")) {
		t.Errorf("bytes = %v, want %d", got["bytes"], len("snapshot-bytes"))
	}
	if pushed.Owner != "alice" || pushed.Repo != "backups" || pushed.FileName != "panrouter-db-2026-11-15.sqlite" {
		t.Errorf("pushed %+v, want alice/backups with the dated name", pushed)
	}
	if string(pushed.Content) != "snapshot-bytes" {
		t.Errorf("pushed content = %q", pushed.Content)
	}

	// A refused setup is a 400 carrying the reason, not a 500.
	h.Backup.CheckRepoPrivate = func(_ context.Context, _, _, _ string) error {
		return errors.New("GitHub: cannot read repo alice/backups (HTTP 404) — check the token and the repo name")
	}
	rec = authorizedRequest(t, h.HandleRunBackup, http.MethodPost, "/api/backup/run", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("refused run: status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if got := jsonText(decodeBodyMap(t, rec)["error"]); !strings.Contains(got, "HTTP 404") {
		t.Errorf("error = %q, want the GitHub reason", got)
	}
}

// The mirror download authenticates with its own token, not the dashboard
// credential, and serves the raw database file.
func TestBackupDownload(t *testing.T) {
	authTestEnv(t)
	repo, cleanup := setupTestDB(t)
	defer cleanup()
	h := NewDashboardHandler(repo)

	dbDir := t.TempDir()
	dbFile := filepath.Join(dbDir, "data.sqlite")
	if err := os.WriteFile(dbFile, []byte("SQLite format 3\x00fixture"), 0o600); err != nil {
		t.Fatalf("write fixture db: %v", err)
	}
	h.DatabasePath = dbFile

	record, token, err := repo.CreateBackupToken("mirror", "any")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	// Header credential.
	req := httptest.NewRequest(http.MethodGet, "/api/backup/download", nil)
	req.Header.Set(BackupTokenHeader, token)
	rec := httptest.NewRecorder()
	h.HandleDownloadBackup(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if want := "SQLite format 3\x00fixture"; rec.Body.String() != want {
		t.Errorf("body = %q, want the database bytes", rec.Body.String())
	}
	for header, want := range map[string]string{
		"Content-Type":        "application/vnd.sqlite3",
		"Content-Disposition": `attachment; filename="data.sqlite"`,
		"Cache-Control":       "no-store",
		"Pragma":              "no-cache",
		"Content-Length":      "23",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	// The touch is recorded, so the dashboard can show activity.
	refreshed, err := repo.GetBackupTokenByKeyID(record.KeyID)
	if err != nil {
		t.Fatalf("reload token: %v", err)
	}
	if refreshed.LastUsedAt == nil || *refreshed.LastUsedAt == "" {
		t.Error("lastUsedAt was not recorded on download")
	}

	// Query credential (the shape a simple curl/cron uses).
	req = httptest.NewRequest(http.MethodGet, "/api/backup/download?token="+token, nil)
	rec = httptest.NewRecorder()
	h.HandleDownloadBackup(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("query token: status = %d, want 200", rec.Code)
	}

	// Refusals carry the upstream reason and a 401.
	tests := []struct {
		name      string
		token     string
		wantError string
	}{
		{"missing", "", "Malformed token"},
		{"wrong crc", "bk-any-abcdef-00000000", "Invalid token"},
		{"unknown", "bk-any-zzzzzz-01234567", "Invalid token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/backup/download", nil)
			if tt.token != "" {
				req.Header.Set(BackupTokenHeader, tt.token)
			}
			rec := httptest.NewRecorder()
			h.HandleDownloadBackup(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
			}
			if got := jsonText(decodeBodyMap(t, rec)["error"]); got != tt.wantError {
				t.Errorf("error = %q, want %q", got, tt.wantError)
			}
		})
	}

	// A revoked token is refused with its own reason.
	if changed, err := repo.SetBackupTokenActive(record.ID, false); err != nil || !changed {
		t.Fatalf("revoke: changed=%v err=%v", changed, err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/backup/download", nil)
	req.Header.Set(BackupTokenHeader, token)
	rec = httptest.NewRecorder()
	h.HandleDownloadBackup(rec, req)
	if rec.Code != http.StatusUnauthorized || jsonText(decodeBodyMap(t, rec)["error"]) != "Token has been revoked" {
		t.Errorf("revoked token: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// A missing database file is a 404, after the token is accepted.
	h.DatabasePath = filepath.Join(dbDir, "absent.sqlite")
	req = httptest.NewRequest(http.MethodGet, "/api/backup/download", nil)
	req.Header.Set(BackupTokenHeader, token)
	rec = httptest.NewRecorder()
	h.HandleDownloadBackup(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token still reached the file: status = %d", rec.Code)
	}
	if _, err := repo.SetBackupTokenActive(record.ID, true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/backup/download", nil)
	req.Header.Set(BackupTokenHeader, token)
	rec = httptest.NewRecorder()
	h.HandleDownloadBackup(rec, req)
	if rec.Code != http.StatusNotFound || jsonText(decodeBodyMap(t, rec)["error"]) != "Database file not found" {
		t.Errorf("missing file: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// The token CRUD: create returns the token once, and the list never echoes it.
func TestBackupTokensRoutes(t *testing.T) {
	authTestEnv(t)
	repo, cleanup := setupTestDB(t)
	defer cleanup()
	h := NewDashboardHandler(repo)

	// No credential, on every verb: the token list is what hands out mirror
	// access, so the gate is the same one the config read uses.
	for _, tt := range []struct {
		name    string
		handler http.HandlerFunc
		method  string
		target  string
	}{
		{"list", h.HandleListBackupTokens, http.MethodGet, "/api/backup/tokens"},
		{"create", h.HandleCreateBackupToken, http.MethodPost, "/api/backup/tokens"},
		{"update", h.HandleUpdateBackupToken, http.MethodPatch, "/api/backup/tokens"},
		{"delete", h.HandleDeleteBackupToken, http.MethodDelete, "/api/backup/tokens?id=x"},
	} {
		t.Run("anonymous "+tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.handler(rec, httptest.NewRequest(tt.method, tt.target, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
			}
			if got := decodeBodyMap(t, rec)["error"]; got != "Invalid password" {
				t.Errorf("error = %v, want Invalid password", got)
			}
		})
	}

	var rec *httptest.ResponseRecorder

	// Create with an explicit "any machine" token.
	body, _ := json.Marshal(map[string]any{"name": "debian-mirror", "machineId": ""})
	rec = authorizedRequest(t, h.HandleCreateBackupToken, http.MethodPost, "/api/backup/tokens", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	created := decodeBodyMap(t, rec)
	fullToken, _ := created["token"].(string)
	if !strings.HasPrefix(fullToken, "bk-any-") {
		t.Fatalf("token = %q, want a bk-any-… token", fullToken)
	}
	for _, key := range []string{"success", "token", "id", "keyId", "name", "machineId", "createdAt"} {
		if _, present := created[key]; !present {
			t.Errorf("create response is missing %q: %v", key, created)
		}
	}
	if created["success"] != true || created["name"] != "debian-mirror" || created["machineId"] != "any" {
		t.Errorf("create body = %v", created)
	}

	// The list is metadata only: the full token never comes back.
	rec = authorizedRequest(t, h.HandleListBackupTokens, http.MethodGet, "/api/backup/tokens", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), fullToken) {
		t.Error("GET /api/backup/tokens echoed the full token")
	}
	listBody := decodeBodyMap(t, rec)
	tokens, _ := listBody["tokens"].([]any)
	if len(tokens) != 1 {
		t.Fatalf("tokens = %v, want one", listBody["tokens"])
	}
	row, _ := tokens[0].(map[string]any)
	if _, present := row["token"]; present {
		t.Error("a listed token carries a token field")
	}
	for _, key := range []string{"id", "keyId", "name", "machineId", "crc", "isActive", "lastUsedAt", "createdAt"} {
		if _, present := row[key]; !present {
			t.Errorf("listed token is missing %q: %v", key, row)
		}
	}
	if row["isActive"] != true {
		t.Errorf("isActive = %v, want true", row["isActive"])
	}

	// The default machine id is this server's, matching upstream's
	// getConsistentMachineId default.
	body, _ = json.Marshal(map[string]any{"name": "default-machine"})
	rec = authorizedRequest(t, h.HandleCreateBackupToken, http.MethodPost, "/api/backup/tokens", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create default: status = %d (body %q)", rec.Code, rec.Body.String())
	}
	if got := decodeBodyMap(t, rec)["machineId"]; got != auth.MachineID() {
		t.Errorf("machineId = %v, want this machine's id %q", got, auth.MachineID())
	}

	// Revoke, then re-enable.
	id, _ := created["id"].(string)
	body, _ = json.Marshal(map[string]any{"id": id, "isActive": false})
	rec = authorizedRequest(t, h.HandleUpdateBackupToken, http.MethodPatch, "/api/backup/tokens", body)
	if rec.Code != http.StatusOK || decodeBodyMap(t, rec)["isActive"] != false {
		t.Fatalf("revoke: status=%d body=%q", rec.Code, rec.Body.String())
	}
	body, _ = json.Marshal(map[string]any{"id": id, "isActive": true})
	rec = authorizedRequest(t, h.HandleUpdateBackupToken, http.MethodPatch, "/api/backup/tokens", body)
	if rec.Code != http.StatusOK || decodeBodyMap(t, rec)["isActive"] != true {
		t.Fatalf("enable: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Refusals.
	rec = authorizedRequest(t, h.HandleUpdateBackupToken, http.MethodPatch, "/api/backup/tokens", []byte(`{}`))
	if rec.Code != http.StatusBadRequest || jsonText(decodeBodyMap(t, rec)["error"]) != "id is required" {
		t.Errorf("patch without id: status=%d body=%q", rec.Code, rec.Body.String())
	}
	body, _ = json.Marshal(map[string]any{"id": "nope", "isActive": false})
	rec = authorizedRequest(t, h.HandleUpdateBackupToken, http.MethodPatch, "/api/backup/tokens", body)
	if rec.Code != http.StatusNotFound || jsonText(decodeBodyMap(t, rec)["error"]) != "Token not found" {
		t.Errorf("patch unknown id: status=%d body=%q", rec.Code, rec.Body.String())
	}
	rec = authorizedRequest(t, h.HandleDeleteBackupToken, http.MethodDelete, "/api/backup/tokens?id="+id, nil)
	if rec.Code != http.StatusOK || decodeBodyMap(t, rec)["success"] != true {
		t.Fatalf("delete: status=%d body=%q", rec.Code, rec.Body.String())
	}
	rec = authorizedRequest(t, h.HandleDeleteBackupToken, http.MethodDelete, "/api/backup/tokens?id="+id, nil)
	if rec.Code != http.StatusNotFound || jsonText(decodeBodyMap(t, rec)["error"]) != "Token not found" {
		t.Errorf("second delete: status=%d body=%q", rec.Code, rec.Body.String())
	}
	rec = authorizedRequest(t, h.HandleDeleteBackupToken, http.MethodDelete, "/api/backup/tokens", nil)
	if rec.Code != http.StatusBadRequest || jsonText(decodeBodyMap(t, rec)["error"]) != "id is required" {
		t.Errorf("delete without id: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// The GitHub token never leaves through the settings read, and a settings
// write that does not carry it keeps the stored one.
func TestSettingsHidesAndPreservesBackupToken(t *testing.T) {
	authTestEnv(t)
	repo, cleanup := setupTestDB(t)
	defer cleanup()
	h := NewDashboardHandler(repo)

	seedBackupSettings(t, repo, map[string]any{
		"githubUser": "alice", "githubRepo": "backups", "githubRepoOwner": "alice",
		"githubToken": "ghp_should_not_leak",
	})

	rec := httptest.NewRecorder()
	h.HandleGetSettings(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "ghp_should_not_leak") {
		t.Error("GET /api/settings leaked dbBackup.githubToken")
	}
	block, _ := decodeBodyMap(t, rec)["dbBackup"].(map[string]any)
	if block["githubTokenConfigured"] != true {
		t.Errorf("githubTokenConfigured = %v, want true", block["githubTokenConfigured"])
	}

	// A settings write carrying the block without the token keeps it.
	body, _ := json.Marshal(map[string]any{
		"dbBackup": map[string]any{
			"enabled":         true,
			"intervalHours":   24,
			"githubUser":      "alice",
			"githubRepo":      "backups",
			"githubRepoOwner": "alice",
		},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	h.HandleUpdateSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT settings status = %d (body %q)", rec.Code, rec.Body.String())
	}
	if stored := storedBackupBlock(t, repo); stored["githubToken"] != "ghp_should_not_leak" {
		t.Errorf("stored token = %v, want the previous one preserved", stored["githubToken"])
	}
}

// Check & release answers the provider's verdict and never invents one.
func TestCheckRelease(t *testing.T) {
	authTestEnv(t)
	repo, cleanup := setupTestDB(t)
	defer cleanup()
	h := NewDashboardHandler(repo)
	router := setupTestRouterWith(repo, h)

	// Unknown connection.
	req := httptest.NewRequest(http.MethodPost, "/api/usage/absent/check-release", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown connection: status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if jsonText(decodeBodyMap(t, rec)["error"]) != "Connection not found" {
		t.Errorf("error = %q", rec.Body.String())
	}

	// A provider with no quota API: the block stays, and the reason says so.
	if err := repo.CreateProviderConnectionFull("conn-mystery", "mystery-provider", "apikey", "Mystery", nil, `{"apiKey":"sk-x"}`); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/usage/conn-mystery/check-release", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("check-release status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	got := decodeBodyMap(t, rec)
	if got["released"] != false {
		t.Errorf("released = %v, want false", got["released"])
	}
	if got["reason"] != "Usage API not implemented for this provider" {
		t.Errorf("reason = %v", got["reason"])
	}
	if quotas, present := got["quotas"]; !present || quotas != nil {
		t.Errorf("quotas = %v (present=%v), want null", quotas, present)
	}

	// A lock that the provider could not disprove stays in place.
	if _, err := repo.RawDB().Exec(
		`UPDATE providerConnections SET data = ? WHERE id = 'conn-mystery'`,
		`{"apiKey":"sk-x","modelLock_gpt-4o":"2099-01-01T00:00:00Z"}`,
	); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/usage/conn-mystery/check-release", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if raw["released"] != false {
		t.Fatalf("an unverified provider released the block: %v", raw)
	}
	var data string
	if err := repo.RawDB().QueryRow(`SELECT data FROM providerConnections WHERE id = 'conn-mystery'`).Scan(&data); err != nil {
		t.Fatalf("read connection: %v", err)
	}
	if !strings.Contains(data, "modelLock_gpt-4o") {
		t.Errorf("the model lock was dropped without a provider verdict: %s", data)
	}
}

// The dashboard auth gate is what refuses an anonymous check-release; the
// handler itself is only mounted behind it on the real server.
func TestCheckReleaseRequiresDashboardAuth(t *testing.T) {
	authTestEnv(t)
	repo, cleanup := setupTestDB(t)
	defer cleanup()

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireDashboardAuth(repo))
		RegisterRoutes(r, NewDashboardHandler(repo))
	})

	req := httptest.NewRequest(http.MethodPost, "/api/usage/anything/check-release", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}
}
