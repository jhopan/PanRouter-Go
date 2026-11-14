package oauth

import (
	"context"
	"database/sql"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"

	"panrouter/proxy/internal/handlerutil"
)

// The Xiaomi MiMo import lanes: a key pasted by the operator, and one discovered
// in MiMo Desktop's own state. Ports of upstream
// src/app/api/oauth/xiaomi-mimo/{api-key,auto-import}/route.js.

const (
	mimoDefaultBaseURL = "https://api.xiaomimimo.com/v1"
	mimoModelsSource   = "mimocode-cli"
	// mimoKeyHorizon is how far out an imported key's expiry sits. A key does
	// not expire on a fixed schedule, and a NULL expiry would trip the dashboard
	// into "renew now" for every imported account.
	mimoKeyHorizon = 365 * 24 * time.Hour
)

// mimoAPIKeyRequest is the api-key route's body. Every field is optional
// upstream: a session-only import carries no key at all.
type mimoAPIKeyRequest struct {
	APIKey        any    `json:"apiKey"`
	UID           string `json:"uid"`
	BaseURL       string `json:"baseUrl"`
	MimoPassToken string `json:"mimoPassToken"`
	MimoUserID    string `json:"mimoUserId"`
	MimoCUserID   string `json:"mimoCUserId"`
	Region        string `json:"region"`
}

// mimoStoredConnection is the slice of a stored xiaomi-mimo connection the
// import's dedup rule compares.
type mimoStoredConnection struct {
	id      string
	name    string
	email   string
	token   string
	psd     map[string]any
	rawData map[string]any
}

// HandleMimoAPIKey imports a Xiaomi MiMo API key (or a Desktop session token),
// validating the key against the models endpoint before storing it.
//
// POST /api/oauth/xiaomi-mimo/api-key
// Body: {apiKey, uid?, baseUrl?, mimoPassToken?, mimoUserId?, mimoCUserId?, region?}
func (h *OAuthHandler) HandleMimoAPIKey(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req mimoAPIKeyRequest
	if err := json.Unmarshal(body, &req); err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "API key import failed")
		return
	}

	rawKey, _ := req.APIKey.(string)
	key := strings.TrimSpace(rawKey)
	sessionOnly := key == "" && req.MimoPassToken != ""

	if key == "" && req.MimoPassToken == "" {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "API key is required")
		return
	}
	if key != "" && !strings.HasPrefix(key, "sk-") {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "Invalid key format — expected sk- prefix")
		return
	}

	baseURL := strings.TrimRight(req.BaseURL, "/")
	if baseURL == "" {
		baseURL = mimoDefaultBaseURL
	}

	// Validation is a soft gate: a network failure still stores the key as
	// untested, because a blocked egress is not evidence about the credential.
	validated := false
	modelCount := 0
	if key != "" {
		modelCount, validated = h.validateMimoKey(r.Context(), baseURL, key)
	}

	region := strings.TrimSpace(req.Region)
	authMethod := "api_key"
	if sessionOnly {
		authMethod = "session"
	}

	if existing := h.findMimoConnection(mimoAPIKeyRequest{
		UID: req.UID, MimoPassToken: req.MimoPassToken, MimoUserID: req.MimoUserID,
	}, key, sessionOnly, region); existing != nil {
		psd := existing.psd
		if psd == nil {
			psd = map[string]any{}
		}
		rawData := existing.rawData
		if key != "" {
			rawData["accessToken"] = key
			rawData["apiKey"] = key
		}
		psd["uid"] = firstNonEmpty(req.UID, stringField(psd, "uid"))
		if key != "" {
			psd["baseUrl"] = baseURL
		} else {
			psd["baseUrl"] = firstNonEmpty(stringField(psd, "baseUrl"), baseURL)
		}
		psd["region"] = firstNonEmpty(region, stringField(psd, "region"), "cn")
		if sessionOnly {
			psd["authMethod"] = "session"
		} else {
			psd["authMethod"] = firstNonEmpty(stringField(psd, "authMethod"), "api_key")
		}
		psd["mimoPassToken"] = firstNonEmpty(req.MimoPassToken, stringField(psd, "mimoPassToken"))
		psd["mimoUserId"] = firstNonEmpty(req.MimoUserID, stringField(psd, "mimoUserId"))
		psd["mimoCUserId"] = firstNonEmpty(req.MimoCUserID, stringField(psd, "mimoCUserId"))
		psd["modelCount"] = modelCount
		rawData["providerSpecificData"] = psd
		if validated {
			rawData["testStatus"] = "active"
		}
		if err := h.updateImportedConnection(existing.id, rawData); err != nil {
			handlerutil.WriteJSONError(w, http.StatusInternalServerError, "API key import failed")
			return
		}
		handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
			"success":    true,
			"validated":  validated,
			"modelCount": modelCount,
			"updated":    true,
			"connection": map[string]any{
				"id":          existing.id,
				"provider":    "xiaomi-mimo",
				"email":       nullableStr(existing.email),
				"displayName": nullableStr(existing.name),
			},
		})
		return
	}

	testStatus := "untested"
	if validated || sessionOnly {
		testStatus = "active"
	}
	authType := "api_key"
	if sessionOnly {
		authType = "oauth"
	}
	name := "Xiaomi MiMo"
	email := ""
	if req.UID != "" {
		email = req.UID + "@xiaomi"
		name = "Xiaomi " + req.UID
		if sessionOnly {
			name += " (Session)"
		}
	}
	providerLabel := "API Key"
	if sessionOnly {
		providerLabel = "Session Login"
	}
	dataMap := map[string]any{
		"expiresAt":  time.Now().Add(mimoKeyHorizon).UTC().Format(time.RFC3339),
		"testStatus": testStatus,
		"providerSpecificData": map[string]any{
			"uid":           nullableStr(req.UID),
			"baseUrl":       baseURL,
			"authMethod":    authMethod,
			"provider":      providerLabel,
			"region":        firstNonEmpty(region, "cn"),
			"modelCount":    modelCount,
			"mimoPassToken": nullableStr(req.MimoPassToken),
			"mimoUserId":    nullableStr(req.MimoUserID),
			"mimoCUserId":   nullableStr(req.MimoCUserID),
		},
	}
	if key != "" {
		dataMap["apiKey"] = key
		dataMap["accessToken"] = key
	}
	if email != "" {
		dataMap["email"] = email
	}
	connID := uuid.NewString()
	if err := h.createImportedConnection(connID, "xiaomi-mimo", authType, name, dataMap); err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "API key import failed")
		return
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"validated":  validated,
		"modelCount": modelCount,
		"connection": map[string]any{
			"id":          connID,
			"provider":    "xiaomi-mimo",
			"email":       nullableStr(email),
			"displayName": name,
		},
	})
}

// validateMimoKey asks the models endpoint whether the key works and how many
// models it can reach. A non-200 or a transport error leaves validated false.
func (h *OAuthHandler) validateMimoKey(ctx context.Context, baseURL, key string) (int, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Mimo-Source", mimoModelsSource)
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var payload struct {
		Data []any `json:"data"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return 0, true
	}
	return len(payload.Data), true
}

// findMimoConnection applies the import's dedup rule: same uid, same key, or
// the same Desktop session identity within the same region.
func (h *OAuthHandler) findMimoConnection(req mimoAPIKeyRequest, key string, sessionOnly bool, region string) *mimoStoredConnection {
	if h.Repo == nil {
		return nil
	}
	conns, err := h.Repo.GetProviderConnections("xiaomi-mimo", false)
	if err != nil {
		return nil
	}
	for _, conn := range conns {
		if conn == nil || conn.Provider != "xiaomi-mimo" {
			continue
		}
		stored := decodeMimoStoredConnection(conn.ID, conn.Name, conn.Email, conn.Data)
		switch {
		case req.UID != "" && stored.email == req.UID+"@xiaomi":
			return stored
		case key != "" && stored.token == key:
			return stored
		case sessionOnly && req.MimoUserID != "" &&
			stringField(stored.psd, "mimoUserId") == req.MimoUserID &&
			(region == "" || firstNonEmpty(stringField(stored.psd, "region"), "cn") == region):
			return stored
		}
	}
	return nil
}

// decodeMimoStoredConnection reads the fields the dedup rule needs out of a
// connection row. Called with the row's own name/email/columns so an older row
// that kept the email in the column still matches.
func decodeMimoStoredConnection(id string, name *string, email *string, data string) *mimoStoredConnection {
	raw := map[string]any{}
	if data != "" {
		_ = json.Unmarshal([]byte(data), &raw)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	psd, _ := raw["providerSpecificData"].(map[string]any)
	out := &mimoStoredConnection{
		id:      id,
		name:    derefString(name),
		email:   derefString(email),
		psd:     psd,
		rawData: raw,
	}
	out.token = firstNonEmpty(stringField(raw, "accessToken"), stringField(raw, "apiKey"))
	out.email = firstNonEmpty(out.email, stringField(raw, "email"))
	return out
}

// updateImportedConnection rewrites a connection's data payload in place.
func (h *OAuthHandler) updateImportedConnection(id string, dataMap map[string]any) error {
	encoded, err := json.Marshal(dataMap)
	if err != nil {
		return fmt.Errorf("marshal connection data: %w", err)
	}
	return h.Repo.UpdateConnectionData(id, string(encoded))
}

func stringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// mimoAuthCandidatePaths lists the files MiMo Desktop / MiMoCode write their
// Xiaomi credentials to, in upstream's priority order.
func mimoAuthCandidatePaths() []string {
	home, _ := os.UserHomeDir()
	paths := []string{filepath.Join(home, ".local", "share", "mimocode", "auth.json")}
	switch runtime.GOOS {
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		paths = append(paths, filepath.Join(appData, "Xiaomi MiMo", "auth.json"))
	case "darwin":
		paths = append(paths, filepath.Join(home, "Library", "Application Support", "mimocode", "auth.json"))
	}
	return paths
}

// mimoDesktopPassToken is the per-account Desktop session credential and the
// identity cookies stored beside it. Persisting them per connection is what
// lets several Xiaomi accounts rotate independently.
type mimoDesktopPassToken struct {
	passToken string
	userID    string
	cUserID   string
}

// mimoDesktopCookiePath is the Chromium cookie DB of MiMo Desktop's account
// partition.
func mimoDesktopCookiePath() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Xiaomi MiMo", "Partitions", "xiaomi-account", "Network", "Cookies")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Xiaomi MiMo", "Partitions", "xiaomi-account", "Network", "Cookies")
	default:
		return filepath.Join(home, ".config", "Xiaomi MiMo", "Partitions", "xiaomi-account", "Network", "Cookies")
	}
}

// readMimoDesktopPassToken reads the account cookies out of Desktop's profile.
// Desktop holds the DB with an exclusive lock while it runs, so the file is
// copied first; a lock failure (or no profile) answers nil, which the caller
// treats as "no session credential", never as an error.
func readMimoDesktopPassToken() *mimoDesktopPassToken {
	src := mimoDesktopCookiePath()
	raw, err := os.ReadFile(src)
	if err != nil {
		return nil
	}
	tmp, err := os.CreateTemp("", "9r-mimo-cookies-*.db")
	if err != nil {
		return nil
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(raw)
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpName) }()
	if werr != nil {
		return nil
	}

	db, err := sql.Open("sqlite", "file:"+tmpName+"?mode=ro&immutable=1")
	if err != nil {
		return nil
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query("SELECT name, value FROM cookies WHERE host_key = ?", ".account.xiaomi.com")
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	jar := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			continue
		}
		jar[name] = value
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	if jar["passToken"] == "" {
		return nil
	}
	return &mimoDesktopPassToken{
		passToken: jar["passToken"],
		userID:    jar["userId"],
		cUserID:   jar["cUserId"],
	}
}

// HandleMimoAutoImport discovers Xiaomi credentials in MiMo Desktop's local
// auth.json and reports them for the dashboard to import.
//
// GET|POST /api/oauth/xiaomi-mimo/auto-import
func (h *OAuthHandler) HandleMimoAutoImport(w http.ResponseWriter, r *http.Request) {
	candidates := mimoAuthCandidatePaths()

	authPath := ""
	for _, candidate := range candidates {
		if file, err := os.Open(candidate); err == nil {
			_ = file.Close()
			authPath = candidate
			break
		}
	}
	if authPath == "" {
		handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
			"found": false,
			"error": "Xiaomi MiMo Desktop auth file not found. Checked:\n" + strings.Join(candidates, "\n") +
				"\n\nMake sure Xiaomi MiMo Desktop is installed and you are signed in.",
		})
		return
	}

	raw, err := os.ReadFile(authPath)
	if err != nil {
		handlerutil.WriteJSON(w, http.StatusInternalServerError, map[string]any{"found": false, "error": err.Error()})
		return
	}
	var auth struct {
		Xiaomi struct {
			Key      any `json:"key"`
			Metadata struct {
				UID     string `json:"uid"`
				BaseURL string `json:"base_url"`
			} `json:"metadata"`
		} `json:"xiaomi"`
	}
	if json.Unmarshal(raw, &auth) != nil {
		handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
			"found": false,
			"error": "auth.json is not valid JSON. Please sign in to Xiaomi MiMo Desktop again.",
		})
		return
	}
	rawKey, _ := auth.Xiaomi.Key.(string)
	key := strings.TrimSpace(rawKey)
	if key == "" {
		handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
			"found": false,
			"error": "No Xiaomi credentials found in auth.json. Please sign in to Xiaomi MiMo Desktop.",
		})
		return
	}
	if !strings.HasPrefix(key, "sk-") {
		handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
			"found": false,
			"error": "Xiaomi key does not appear to be a valid API key (expected sk- prefix).",
		})
		return
	}

	baseURL := auth.Xiaomi.Metadata.BaseURL
	if baseURL == "" {
		baseURL = mimoDefaultBaseURL
	}
	pass := readMimoDesktopPassToken()

	payload := map[string]any{
		"found":         true,
		"apiKey":        key,
		"uid":           nullableStr(auth.Xiaomi.Metadata.UID),
		"baseUrl":       baseURL,
		"source":        authPath,
		"mimoPassToken": nil,
		"mimoUserId":    nil,
		"mimoCUserId":   nil,
	}
	if pass != nil {
		payload["mimoPassToken"] = nullableStr(pass.passToken)
		payload["mimoUserId"] = nullableStr(pass.userID)
		payload["mimoCUserId"] = nullableStr(pass.cUserID)
	}
	handlerutil.WriteJSON(w, http.StatusOK, payload)
}
