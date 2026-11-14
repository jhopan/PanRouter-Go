package dashboard

import (
	json "encoding/json/v2"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/models"
)

// Port of upstream src/app/api/models/availability/route.js.
//
// The dashboard's model-health view answers "which models are parked right now,
// and until when". Upstream reads every connection row flat — the model locks
// live as `modelLock_<model>` fields on the connection itself — so the Go
// equivalent decodes the connection's data blob and reads the same keys, which
// is exactly where the chat path writes them (db.LockConnectionModel).
//
// A connection whose own test reported "unavailable" and that holds no live
// model lock is reported once under the pseudo-model "__all": the account is
// dead for every model, not for one of them.

const availableAllModels = "__all"

// availabilityRow is one entry of the response's `models` array. `until` is
// present only for a cooldown row, matching upstream's object literals; the
// value is echoed as stored (an ISO string, normally) rather than reformatted.
type availabilityRow struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Status         string `json:"status"`
	Until          any    `json:"until,omitempty"`
	ConnectionID   string `json:"connectionId"`
	ConnectionName string `json:"connectionName"`
	LastError      any    `json:"lastError"`
}

type availabilityResponse struct {
	Models           []availabilityRow `json:"models"`
	UnavailableCount int               `json:"unavailableCount"`
}

type clearCooldownRequest struct {
	Action   string `json:"action"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// modelLockUntil reads the timestamp a modelLock_* value carries. Upstream
// parses it with `new Date(value)`, which accepts both the ISO string this
// gateway writes and the epoch-millisecond numbers a shared upstream database
// may hold. An unparseable value is treated as "no lock", never as a lock that
// never expires.
func modelLockUntil(value any) (time.Time, bool) {
	switch v := value.(type) {
	case string:
		if s := strings.TrimSpace(v); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t, true
			}
			if ms, err := strconv.ParseFloat(s, 64); err == nil {
				return time.UnixMilli(int64(ms)), true
			}
		}
	case float64:
		return time.UnixMilli(int64(v)), true
	case int64:
		return time.UnixMilli(v), true
	case int:
		return time.UnixMilli(int64(v)), true
	}
	return time.Time{}, false
}

// jsTruthy mirrors the truthiness test upstream's `.filter(([, value]) => value)`
// applies to a lock value: a JSON null, false, empty string and zero are all
// "no lock".
func jsTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case int:
		return v != 0
	case int64:
		return v != 0
	default:
		return true
	}
}

// connectionDataMap decodes a connection's data blob. A malformed blob is an
// empty map: a single corrupt row must not take the whole availability list
// down, which is the same tolerance the other dashboard readers apply.
func connectionDataMap(dataJSON string) map[string]any {
	if strings.TrimSpace(dataJSON) == "" {
		return map[string]any{}
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(dataJSON), &raw); err != nil || raw == nil {
		return map[string]any{}
	}
	return raw
}

// availabilityConnectionName is upstream's `name || email || id` fallback for
// this route. It differs from connectionDisplayName (which labels a provider
// card) on purpose: with no name and no email the operator has to see the row
// id, because that is what identifies the parked account.
func availabilityConnectionName(conn *models.ProviderConnection) string {
	if conn.Name != nil && strings.TrimSpace(*conn.Name) != "" {
		return *conn.Name
	}
	if conn.Email != nil && strings.TrimSpace(*conn.Email) != "" {
		return *conn.Email
	}
	return conn.ID
}

// activeModelLocks returns the connection's live modelLock_* entries, sorted by
// model id so the response is stable across map iteration order (upstream emits
// insertion order; a deterministic order is the same information, minus the
// churn).
func activeModelLocks(data map[string]any, now time.Time) []availabilityRow {
	const prefix = "modelLock_"
	locks := make([]availabilityRow, 0, len(data))
	for key, value := range data {
		if !strings.HasPrefix(key, prefix) || !jsTruthy(value) {
			continue
		}
		until, ok := modelLockUntil(value)
		if !ok || !until.After(now) {
			continue
		}
		model := strings.TrimPrefix(key, prefix)
		if model == "" {
			model = availableAllModels
		}
		locks = append(locks, availabilityRow{Model: model, Status: "cooldown", Until: value})
	}
	sort.Slice(locks, func(i, j int) bool { return locks[i].Model < locks[j].Model })
	return locks
}

// HandleGetModelAvailability handles GET /api/models/availability.
func (h *DashboardHandler) HandleGetModelAvailability(w http.ResponseWriter, r *http.Request) {
	connections, err := h.Repo.GetProviderConnections("", false)
	if err != nil {
		log.Error("dashboard", "failed to get model availability", "error", err)
		handlerutil.WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "Failed to fetch model availability",
		})
		return
	}

	now := time.Now()
	rows := make([]availabilityRow, 0, len(connections))
	for _, conn := range connections {
		if conn == nil {
			continue
		}
		data := connectionDataMap(conn.Data)
		testStatus, _ := data["testStatus"].(string)
		lastError := data["lastError"]
		displayName := availabilityConnectionName(conn)

		locks := activeModelLocks(data, now)
		for _, lock := range locks {
			lock.Provider = conn.Provider
			lock.ConnectionID = conn.ID
			lock.ConnectionName = displayName
			lock.LastError = lastError
			rows = append(rows, lock)
		}

		// A connection with any live lock is already represented; the "__all"
		// row exists to surface an account that is dead without a per-model
		// lock to say why (upstream's `locks.length === 0` guard).
		if len(locks) == 0 && testStatus == "unavailable" {
			rows = append(rows, availabilityRow{
				Provider:       conn.Provider,
				Model:          availableAllModels,
				Status:         "unavailable",
				ConnectionID:   conn.ID,
				ConnectionName: displayName,
				LastError:      lastError,
			})
		}
	}

	handlerutil.WriteJSON(w, http.StatusOK, availabilityResponse{
		Models:           rows,
		UnavailableCount: len(rows),
	})
}

// HandleClearModelCooldown handles POST /api/models/availability.
//
// Body: {"action":"clearCooldown","provider":"<id>","model":"<id>"}. Only the
// connections that actually hold that lock are written, and a connection whose
// own status reads "unavailable" is also brought back to "active" — clearing
// the lock alone would leave the dashboard showing a dead account whose reason
// was the lock just removed.
func (h *DashboardHandler) HandleClearModelCooldown(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		handlerutil.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "Invalid request"})
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req clearCooldownRequest
	_ = json.Unmarshal(body, &req)
	if req.Action != "clearCooldown" || req.Provider == "" || req.Model == "" {
		handlerutil.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "Invalid request"})
		return
	}

	connections, err := h.Repo.GetProviderConnections(req.Provider, false)
	if err != nil {
		log.Error("dashboard", "failed to clear model cooldown", "error", err)
		handlerutil.WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "Failed to clear cooldown",
		})
		return
	}

	lockKey := "modelLock_" + req.Model
	for _, conn := range connections {
		if conn == nil {
			continue
		}
		data := connectionDataMap(conn.Data)
		if !jsTruthy(data[lockKey]) {
			continue
		}
		data[lockKey] = nil
		if status, _ := data["testStatus"].(string); status == "unavailable" {
			data["testStatus"] = "active"
			data["lastError"] = nil
			data["lastErrorAt"] = nil
			data["backoffLevel"] = 0
		}
		encoded, marshalErr := json.Marshal(data)
		if marshalErr != nil {
			log.Error("dashboard", "failed to encode connection data", "connection", conn.ID, "error", marshalErr)
			handlerutil.WriteJSON(w, http.StatusInternalServerError, map[string]any{
				"error": "Failed to clear cooldown",
			})
			return
		}
		if err := h.Repo.UpdateConnectionData(conn.ID, string(encoded)); err != nil {
			log.Error("dashboard", "failed to clear model cooldown", "connection", conn.ID, "error", err)
			handlerutil.WriteJSON(w, http.StatusInternalServerError, map[string]any{
				"error": "Failed to clear cooldown",
			})
			return
		}
	}

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
