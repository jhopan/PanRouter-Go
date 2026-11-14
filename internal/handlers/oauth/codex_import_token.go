package oauth

import (
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"panrouter/proxy/internal/handlerutil"
)

// codexProfileClaimKey is the namespaced claim block OpenAI puts the signed-in
// user's profile in. The access-token import path reads the email from there
// first, exactly as upstream does.
const codexProfileClaimKey = "https://api.openai.com/profile"

// HandleCodexImportToken imports a ChatGPT access token minted from
// chatgpt.com settings as a codex connection, bypassing the OAuth refresh flow.
//
// POST /api/oauth/codex/import-token
// Body: {accessToken: string, name?: string}
//
// Port of upstream src/app/api/oauth/codex/import-token/route.js: the token is
// decoded (unverified) for the account email, workspace and plan, then stored
// as an access_token connection — a lane that answers with a flat
// {success, connection} object rather than the oauth family's redirect shape.
func (h *OAuthHandler) HandleCodexImportToken(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "failed to read body")
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req struct {
		// Any, not string: upstream refuses a non-string token with its own
		// 400 instead of failing the whole body parse.
		AccessToken any    `json:"accessToken"`
		Name        string `json:"name"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		// Upstream's `await request.json()` throws here — an empty or unparseable
		// body included — and the catch answers 500 with the parser's message.
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	rawToken, isString := req.AccessToken.(string)
	token := strings.TrimSpace(rawToken)
	if !isString || token == "" {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "Access token is required")
		return
	}

	providerSpecificData := map[string]any{"authMethod": "access_token"}
	email := ""

	// Decoding is best-effort: a token that is not a JWT is still importable,
	// it simply carries no account metadata.
	if claims, ok := decodeJWTClaims(token); ok {
		profile, _ := claims[codexProfileClaimKey].(map[string]any)
		auth, _ := claims[codexAuthClaimKey].(map[string]any)
		email = firstNonEmpty(
			stringClaim(profile, "email"),
			stringClaim(claims, "email"),
			stringClaim(claims, "preferred_username"),
		)
		if v := stringClaim(auth, "chatgpt_account_id"); v != "" {
			providerSpecificData["chatgptAccountId"] = v
		}
		if v := stringClaim(auth, "chatgpt_plan_type"); v != "" {
			providerSpecificData["chatgptPlanType"] = v
		}
		// The expiry rides along as the raw claim, like upstream: the dashboard
		// renders it, nothing here interprets it.
		if exp, ok := claims["exp"]; ok {
			providerSpecificData["jwtExp"] = exp
		}
	}

	// Same fallback as upstream: the access token may carry the id_token's
	// claims under the shared extractor.
	if email == "" {
		info := codexAccountInfo(token)
		email = info.Email
		if info.AccountID != "" {
			providerSpecificData["chatgptAccountId"] = info.AccountID
		}
		if info.PlanType != "" {
			providerSpecificData["chatgptPlanType"] = info.PlanType
		}
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = firstNonEmpty(email, "ChatGPT Access Token")
	}

	connID := uuid.NewString()
	dataMap := map[string]any{
		"accessToken":          token,
		"providerSpecificData": providerSpecificData,
		"testStatus":           "active",
	}
	if email != "" {
		dataMap["email"] = email
	}
	if err := h.createImportedConnection(connID, "codex", "access_token", name, dataMap); err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	workspace, _ := providerSpecificData["chatgptAccountId"].(string)
	plan, _ := providerSpecificData["chatgptPlanType"].(string)
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"connection": map[string]any{
			"id":        connID,
			"provider":  "codex",
			"email":     nullableStr(email),
			"name":      name,
			"workspace": nullableStr(workspace),
			"plan":      nullableStr(plan),
		},
	})
}

// createImportedConnection inserts a connection row with its data payload as
// given. A nil repo (a handler built outside a boot, as the validation tests
// do) stores nothing and is not an error: the caller's answer describes the
// connection it would have written.
func (h *OAuthHandler) createImportedConnection(connID, provider, authType, name string, dataMap map[string]any) error {
	encoded, err := json.Marshal(dataMap)
	if err != nil {
		return fmt.Errorf("failed to marshal connection data: %w", err)
	}
	if h.Repo == nil {
		return nil
	}
	return h.Repo.CreateProviderConnectionFull(connID, provider, authType, name, nil, string(encoded))
}
