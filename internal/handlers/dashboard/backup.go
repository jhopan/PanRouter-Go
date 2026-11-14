package dashboard

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	json "encoding/json/v2"

	"panrouter/proxy/internal/auth"
	"panrouter/proxy/internal/config"
	"panrouter/proxy/internal/dbbackup"
	"panrouter/proxy/internal/handlerutil"
)

// Automatic database backup and the mirror download tokens.
//
// Port of src/app/api/backup/{config,run,download,tokens}/route.js. The config
// and tokens routes are ordinary dashboard endpoints; the download is the one
// route in this group that does NOT take the dashboard credential — see
// HandleDownloadBackup.

const (
	// BackupTokenHeader carries the mirror credential. It is a backup token
	// (bk-…), never a dashboard password or an API key.
	BackupTokenHeader = "x-backup-token"

	// backupBodyLimit bounds the config/token bodies. They hold a handful of
	// short strings; anything larger is not one of these requests.
	backupBodyLimit = 1 << 20
)

// backupAuthorized is the gate every /api/backup route except the download
// applies. Upstream writes it inline: the local CLI token, or the re-typed
// dashboard password. A dashboard session is accepted here too, because it is
// what the SPA presents — the same step-up shape the database export uses
// (issue #47). An install with login disabled has nothing to authenticate
// against and is admitted, exactly like the rest of the dashboard API.
func (h *DashboardHandler) backupAuthorized(r *http.Request) bool {
	return h.exportAuthorized(r)
}

// preserveBackupToken keeps the stored GitHub token across a settings write.
//
// The token is a secret the dashboard never receives back, so any settings
// payload it builds carries the dbBackup block without one. Writing that block
// through would erase a configured backup on an unrelated save. An explicit
// githubToken in the payload still wins (upstream's `githubToken !== undefined`
// arm), including an explicit empty string, which is how the token is cleared.
func (h *DashboardHandler) preserveBackupToken(updates map[string]any) {
	block, ok := updates["dbBackup"].(map[string]any)
	if !ok || block == nil {
		return
	}
	if _, present := block["githubToken"]; present {
		return
	}
	stored := ""
	if raw, err := h.Repo.GetSettingsRaw(); err == nil {
		if cur, ok := raw["dbBackup"].(map[string]any); ok {
			stored, _ = cur["githubToken"].(string)
		}
	}
	block["githubToken"] = stored
}

// backupConfigBlock reads the stored dbBackup block.
func (h *DashboardHandler) backupConfigBlock() (dbbackup.Config, error) {
	raw, err := h.Repo.GetSettingsRaw()
	if err != nil {
		return dbbackup.Config{}, err
	}
	return dbbackup.ConfigFromSettings(raw), nil
}

// HandleGetBackupConfig handles GET /api/backup/config.
//
// The GitHub token is a secret — it grants write access to the repo that holds
// every OAuth token on this box — so it is never in this response. The
// dashboard renders `githubTokenConfigured` instead, which is all the form
// needs to show "saved — leave blank to keep".
func (h *DashboardHandler) HandleGetBackupConfig(w http.ResponseWriter, r *http.Request) {
	if !h.backupAuthorized(r) {
		writePlainError(w, http.StatusUnauthorized, "Invalid password")
		return
	}
	cfg, err := h.backupConfigBlock()
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, err.Error())
		return
	}
	owner, repo := cfg.Reported()
	url := ""
	if owner != "" && repo != "" {
		url = "https://github.com/" + owner + "/" + repo
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"enabled":               cfg.Enabled,
		"intervalHours":         cfg.IntervalHours,
		"githubUser":            cfg.GithubUser,
		"githubRepo":            repo,
		"githubRepoOwner":       owner,
		"githubRepoUrl":         url,
		"githubTokenConfigured": cfg.GithubToken != "",
	})
}

// HandlePostBackupConfig handles POST /api/backup/config.
//
// The target repo MUST be private: a public repo would publish every OAuth
// token on the box. That is checked against the GitHub API before the
// configuration is accepted, so a typo that points at a public repo is caught
// at setup time rather than at the first backup.
func (h *DashboardHandler) HandlePostBackupConfig(w http.ResponseWriter, r *http.Request) {
	if !h.backupAuthorized(r) {
		writePlainError(w, http.StatusUnauthorized, "Invalid password")
		return
	}

	body, err := decodeBackupBody(r)
	if err != nil {
		// Upstream's `await request.json()` throws into the route's catch, which
		// answers the parser's message with a 500.
		writePlainError(w, http.StatusInternalServerError, err.Error())
		return
	}

	enabled := body["enabled"] == true
	intervalHours := dbbackup.ResolveIntervalHours(body["intervalHours"])
	githubUser := strings.TrimSpace(jsonString(body["githubUser"]))
	rawRepo := strings.TrimSpace(jsonString(body["githubRepo"]))
	// The repository may be pasted as a full URL, an ssh remote, "owner/name"
	// or a bare repo name. The owner comes from the URL when it carries one,
	// otherwise from the username field.
	ref := dbbackup.ParseRepoRef(rawRepo, githubUser)
	repoOwner, githubRepo := "", ""
	if ref != nil {
		repoOwner, githubRepo = ref.Owner, ref.Repo
	}
	// An empty token means "keep the stored one" — the dashboard never shows it.
	incomingToken := strings.TrimSpace(jsonString(body["githubToken"]))
	current, err := h.backupConfigBlock()
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, err.Error())
		return
	}
	githubToken := incomingToken
	if githubToken == "" {
		githubToken = current.GithubToken
	}

	if githubUser == "" || githubRepo == "" {
		writePlainError(w, http.StatusBadRequest, "GitHub username and repository are required")
		return
	}
	if githubToken == "" {
		writePlainError(w, http.StatusBadRequest, "GitHub token is required")
		return
	}
	if !dbbackup.RepoNameLooksValid(githubRepo) {
		writePlainError(w, http.StatusBadRequest, "Invalid repository name")
		return
	}

	// Hard gate: a public repo would publish the whole database.
	if h.Backup != nil {
		if err := h.Backup.CheckRepoPrivate(r.Context(), repoOwner, githubRepo, githubToken); err != nil {
			writePlainError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if err := h.Repo.UpdateSettingsRaw(map[string]any{
		"dbBackup": map[string]any{
			"enabled":         enabled,
			"intervalHours":   intervalHours,
			"githubUser":      githubUser,
			"githubRepo":      githubRepo,
			"githubRepoOwner": repoOwner,
			"githubToken":     githubToken,
		},
	}); err != nil {
		writePlainError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Start or stop the scheduler to match.
	if h.Backup != nil {
		h.Backup.Configure()
	}

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"success":               true,
		"enabled":               enabled,
		"intervalHours":         intervalHours,
		"githubUser":            githubUser,
		"githubRepo":            githubRepo,
		"githubRepoOwner":       repoOwner,
		"githubTokenConfigured": true,
	})
}

// HandleRunBackup handles POST /api/backup/run: one backup immediately,
// ignoring the schedule. Used by the dashboard's "Back up now" button so the
// operator can verify the setup works without waiting for the night.
func (h *DashboardHandler) HandleRunBackup(w http.ResponseWriter, r *http.Request) {
	if !h.backupAuthorized(r) {
		writePlainError(w, http.StatusUnauthorized, "Invalid password")
		return
	}
	if h.Backup == nil {
		writePlainError(w, http.StatusInternalServerError, "The backup runner is not configured")
		return
	}

	result := h.Backup.Run(r.Context(), true)
	if !result.OK {
		reason := result.Reason
		if reason == "" {
			reason = "The backup failed"
		}
		writePlainError(w, http.StatusBadRequest, reason)
		return
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"fileName": result.FileName,
		"bytes":    result.Bytes,
	})
}

// HandleDownloadBackup handles GET /api/backup/download: the raw SQLite file,
// served to a trusted machine that mirrors the backup off the box.
//
// Why a dedicated token instead of the dashboard password: the mirror runs
// unattended, so it needs a credential it can hold without a human present, and
// it must not be able to reach anything else on this server. A backup token is
// scoped to exactly this route — an API key would reach the whole LLM surface,
// which is far too much for a mirror. The token is checked against the stored
// records, so it can be revoked from the dashboard without touching the file on
// the machine.
func (h *DashboardHandler) HandleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	provided := r.Header.Get(BackupTokenHeader)
	if provided == "" {
		provided = r.URL.Query().Get("token")
	}

	ok, reason, record := h.Repo.CheckBackupToken(provided)
	if !ok {
		if reason == "" {
			reason = "Invalid token"
		}
		writePlainError(w, http.StatusUnauthorized, reason)
		return
	}
	// Best effort — a failed touch must not break the download.
	_ = h.Repo.TouchBackupToken(record.KeyID)

	path := h.databaseFilePath()
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		writePlainError(w, http.StatusNotFound, "Database file not found")
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = file.Close() }()

	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Content-Disposition", `attachment; filename="data.sqlite"`)
	// Never let a proxy keep a copy of the database.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

// databaseFilePath is the SQLite file this process is serving. Defaulting to
// the configured path keeps the download in step with the database the gateway
// actually opened.
func (h *DashboardHandler) databaseFilePath() string {
	if h.DatabasePath != "" {
		return h.DatabasePath
	}
	return config.LoadConfig().DatabasePath
}

// HandleListBackupTokens handles GET /api/backup/tokens.
//
// No secret material: only the key id, the name, the machine id, whether it is
// active, and when it was last used. The full token exists only in the response
// to the POST that created it.
func (h *DashboardHandler) HandleListBackupTokens(w http.ResponseWriter, r *http.Request) {
	if !h.backupAuthorized(r) {
		writePlainError(w, http.StatusUnauthorized, "Invalid password")
		return
	}
	tokens, err := h.Repo.GetBackupTokens()
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, err.Error())
		return
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// HandleCreateBackupToken handles POST /api/backup/tokens.
//
// A token can be pinned to a machine id, or left free so any machine can use
// it. The default is this server's own machine id, which makes the token
// useless on the mirror — so the operator has to pass the mirror's id
// explicitly, or send machineId:"" to allow any machine.
func (h *DashboardHandler) HandleCreateBackupToken(w http.ResponseWriter, r *http.Request) {
	if !h.backupAuthorized(r) {
		writePlainError(w, http.StatusUnauthorized, "Invalid password")
		return
	}
	// Upstream tolerates a missing or malformed body here (`.catch(() => ({}))`)
	// because every field has a default.
	body, _ := decodeBackupBody(r)

	name := strings.TrimSpace(jsonString(body["name"]))
	if name == "" {
		name = "backup mirror"
	}
	machineID := ""
	if raw, present := body["machineId"]; present {
		if s, isString := raw.(string); isString {
			if s == "" {
				machineID = "any"
			} else {
				machineID = strings.TrimSpace(s)
			}
		}
	}
	if machineID == "" {
		machineID = strings.TrimSpace(auth.MachineID())
	}
	if machineID == "" {
		machineID = "any"
	}

	token, fullToken, err := h.Repo.CreateBackupToken(name, machineID)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The token is returned here and nowhere else.
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"token":     fullToken,
		"id":        token.ID,
		"keyId":     token.KeyID,
		"name":      token.Name,
		"machineId": token.MachineID,
		"createdAt": token.CreatedAt,
	})
}

// HandleUpdateBackupToken handles PATCH /api/backup/tokens: revoke
// (isActive:false) or re-enable (isActive:true) a token.
func (h *DashboardHandler) HandleUpdateBackupToken(w http.ResponseWriter, r *http.Request) {
	if !h.backupAuthorized(r) {
		writePlainError(w, http.StatusUnauthorized, "Invalid password")
		return
	}
	body, err := decodeBackupBody(r)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, err.Error())
		return
	}
	id := jsonString(body["id"])
	if id == "" {
		writePlainError(w, http.StatusBadRequest, "id is required")
		return
	}
	active := body["isActive"] == true
	changed, err := h.Repo.SetBackupTokenActive(id, active)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !changed {
		writePlainError(w, http.StatusNotFound, "Token not found")
		return
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"id":       id,
		"isActive": active,
	})
}

// HandleDeleteBackupToken handles DELETE /api/backup/tokens?id=…: remove a
// token permanently.
func (h *DashboardHandler) HandleDeleteBackupToken(w http.ResponseWriter, r *http.Request) {
	if !h.backupAuthorized(r) {
		writePlainError(w, http.StatusUnauthorized, "Invalid password")
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writePlainError(w, http.StatusBadRequest, "id is required")
		return
	}
	changed, err := h.Repo.DeleteBackupToken(id)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !changed {
		writePlainError(w, http.StatusNotFound, "Token not found")
		return
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"id":      id,
	})
}

// decodeBackupBody reads a JSON object body. An empty body decodes to an empty
// map, which is what both the config and the token routes want: every field has
// a stored or defaulted value behind it.
func decodeBackupBody(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, backupBodyLimit))
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return body, nil
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if body == nil {
		body = map[string]any{}
	}
	return body, nil
}

// jsonString reads a string field, treating a missing or non-string value as
// the empty string — the shape upstream gets from `String(body.x || "")` for
// everything except numbers.
func jsonString(v any) string {
	s, _ := v.(string)
	return s
}
