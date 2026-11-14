package oauth

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"panrouter/proxy/internal/handlerutil"
)

// zedUser is the subset of cloud.zed.dev's /client/users/me the Zed lanes use.
type zedUser struct {
	Email             string            `json:"email"`
	Name              string            `json:"name"`
	DisplayName       string            `json:"display_name"`
	Username          string            `json:"username"`
	DefaultOrgID      string            `json:"default_organization_id"`
	DefaultOrgIDCamel string            `json:"defaultOrganizationId"`
	Organizations     []zedOrganization `json:"organizations"`
}

// zedOrganization is one organization entry in that response.
type zedOrganization struct {
	ID         string `json:"id"`
	IsPersonal bool   `json:"is_personal"`
}

// organizationID resolves the organization a Zed connection bills against:
// the account's default, then its personal organization, then the first one.
func (u *zedUser) organizationID() string {
	if u == nil {
		return ""
	}
	if id := firstNonEmpty(u.DefaultOrgID, u.DefaultOrgIDCamel); id != "" {
		return id
	}
	first := ""
	for _, org := range u.Organizations {
		if org.ID == "" {
			continue
		}
		if org.IsPersonal {
			return org.ID
		}
		if first == "" {
			first = org.ID
		}
	}
	return first
}

// fetchZedUser validates a Zed session against cloud.zed.dev.
//
// The authorization header is Zed's own shape — "<userId> <accessToken>" —
// and the system id travels as a header of its own, so a wrong pair and a
// wrong machine both surface here instead of at first use.
func (h *OAuthHandler) fetchZedUser(ctx context.Context, userID, accessToken, systemID string) (*zedUser, error) {
	base := strings.TrimRight(h.ZedCloudBase, "/")
	if base == "" {
		base = zedCloudBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+zedMePath, nil)
	if err != nil {
		return nil, fmt.Errorf("Zed token validation failed: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", userID+" "+accessToken)
	if systemID != "" {
		req.Header.Set(zedSystemHeader, systemID)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("Zed token validation failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("Zed token validation failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(zedErrorMessage(raw, resp.StatusCode))
	}
	var user zedUser
	if err := json.Unmarshal(raw, &user); err != nil {
		return nil, fmt.Errorf("Zed token validation failed: %v", err)
	}
	return &user, nil
}

// zedErrorMessage picks the message out of a refused Zed response, in the
// order the upstream helper tries: message, error.message, error, body text.
func zedErrorMessage(raw []byte, status int) string {
	var envelope struct {
		Message string         `json:"message"`
		Error   jsontext.Value `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil {
		if msg := strings.TrimSpace(envelope.Message); msg != "" {
			return msg
		}
		if len(envelope.Error) > 0 {
			var msg string
			if json.Unmarshal(envelope.Error, &msg) == nil && strings.TrimSpace(msg) != "" {
				return strings.TrimSpace(msg)
			}
			var wrapped struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(envelope.Error, &wrapped) == nil && strings.TrimSpace(wrapped.Message) != "" {
				return strings.TrimSpace(wrapped.Message)
			}
		}
	}
	if text := strings.TrimSpace(string(raw)); text != "" {
		return truncateForError(raw)
	}
	return fmt.Sprintf("HTTP %d", status)
}

// HandleZedImport validates a Zed session and stores the connection.
// POST /api/oauth/zed/import — body {accessToken, userId, systemId?}.
// Upstream parity (zed/import/route.js): the session is checked against
// cloud.zed.dev, and a token that does not check out is a 401, not a saved row.
func (h *OAuthHandler) HandleZedImport(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	defer r.Body.Close()

	var body struct {
		AccessToken string         `json:"accessToken"`
		UserID      jsontext.Value `json:"userId"`
		SystemID    string         `json:"systemId"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			handlerutil.WriteJSONError(w, http.StatusInternalServerError, "invalid JSON body")
			return
		}
	}

	accessToken := strings.TrimSpace(body.AccessToken)
	userID := zedUserIDString(body.UserID)
	systemID := strings.TrimSpace(body.SystemID)
	if accessToken == "" {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "Access token is required")
		return
	}
	if userID == "" {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "User id is required")
		return
	}
	if systemID == "" {
		systemID = randomUUID()
	}

	user, err := h.fetchZedUser(r.Context(), userID, accessToken, systemID)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusUnauthorized, err.Error())
		return
	}

	orgID := user.organizationID()
	email := user.Email
	displayName := firstNonEmpty(user.Name, user.DisplayName, user.Username, "Zed "+userID)
	connID := "zed-" + shortHash(accessToken)
	psd := map[string]any{
		"authMethod":     "imported",
		"userId":         userID,
		"systemId":       systemID,
		"organizationId": orgID,
	}
	dataMap := map[string]any{
		"apiKey":               accessToken,
		"accessToken":          accessToken,
		"providerSpecificData": psd,
	}
	if email != "" {
		dataMap["email"] = email
	}
	if err := h.upsertOAuthConnection(connID, "zed", displayName, dataMap); err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save connection: %v", err))
		return
	}

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"connection": map[string]any{
			"id":          connID,
			"provider":    "zed",
			"email":       nullableStr(email),
			"displayName": displayName,
		},
	})
}

// HandleZedAutoImport reports the signed-in Zed IDE session found on this
// machine. GET (upstream) and POST (same-origin dashboard call).
//
// It writes nothing: upstream hands the session to the page, which then calls
// /api/oauth/zed/import to validate and save it. A machine with no Zed session
// answers found:false with the reason, which is a successful read of "nothing
// here", not an error.
func (h *OAuthHandler) HandleZedAutoImport(w http.ResponseWriter, r *http.Request) {
	reader := h.ZedCredentials
	if reader == nil {
		reader = platformZedCredentials{}
	}
	creds, err := reader.ReadZedCredentials(r.Context())
	if err != nil {
		handlerutil.WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"found": false,
			"error": err.Error(),
		})
		return
	}
	if !creds.Found {
		message := creds.Error
		if message == "" {
			message = "Zed IDE credentials not found"
		}
		handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
			"found":          false,
			"error":          message,
			"credentialsUrl": nullableStr(creds.CredentialsURL),
		})
		return
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"found":          true,
		"userId":         creds.UserID,
		"accessToken":    creds.AccessToken,
		"systemId":       creds.SystemID,
		"credentialsUrl": nullableStr(creds.CredentialsURL),
	})
}
