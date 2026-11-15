// Kiro enterprise (external IdP) auth — the Microsoft side of the flow, shared by
// the import handler and the token refresher. Port of
// src/lib/oauth/kiroExternalIdp.js validateMicrosoftTokenEndpoint / normalizeScope
// / buildExternalIdpRefreshParams.
package oauth

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"panrouter/proxy/internal/providers"
)

// microsoftTokenEndpointHosts are the only hosts a stored external-IdP endpoint may
// name: it receives the account's refresh token.
var microsoftTokenEndpointHosts = map[string]bool{
	"login.microsoftonline.com": true,
	"login.microsoft.com":       true,
	"login.windows.net":         true,
}

// ValidateMicrosoftTokenEndpoint requires an https Microsoft login endpoint.
func ValidateMicrosoftTokenEndpoint(raw string) (string, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		return "", fmt.Errorf("token_endpoint is required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("token_endpoint must be a valid URL")
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", fmt.Errorf("token_endpoint must use https")
	}
	if !microsoftTokenEndpointHosts[strings.ToLower(parsed.Hostname())] {
		return "", fmt.Errorf("token_endpoint must be a Microsoft login endpoint")
	}
	return parsed.String(), nil
}

// NormalizeKiroIDPScope folds a scope value — a space-separated string or a list —
// into the single space-separated string the token grant carries.
func NormalizeKiroIDPScope(v any) string {
	switch typed := v.(type) {
	case string:
		return strings.Join(strings.Fields(typed), " ")
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				parts = append(parts, strings.TrimSpace(s))
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// KiroIDPString reads the first non-empty string under any of the spellings the
// import accepts (camelCase from the JSON, snake_case from the token endpoint).
func KiroIDPString(psd map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(psd[key]); value != "" {
			return value
		}
	}
	return ""
}

// refreshKiroExternalIDP refreshes an enterprise (Microsoft Entra) Kiro token at
// the account's own IdP endpoint — upstream buildExternalIdpRefreshParams: an
// OAuth refresh_token grant with client_id and scope, form-encoded, against a
// Microsoft login host only.
//
// The endpoint is validated rather than trusted: it is whatever the import stored,
// and it receives the refresh token.
func refreshKiroExternalIDP(ctx context.Context, client *http.Client, refreshToken string, psd map[string]string) (*TokenResult, error) {
	endpoint, err := ValidateMicrosoftTokenEndpoint(KiroIDPString(psd, "tokenEndpoint", "token_endpoint"))
	if err != nil {
		return nil, fmt.Errorf("kiro: external_idp refresh: %w", err)
	}
	clientID := KiroIDPString(psd, "clientId", "client_id")
	if clientID == "" {
		return nil, fmt.Errorf("kiro: clientId is required for external_idp refresh")
	}
	scope := NormalizeKiroIDPScope(psd["scope"])
	if scope == "" {
		scope = NormalizeKiroIDPScope(psd["scopes"])
	}
	if scope == "" {
		return nil, fmt.Errorf("kiro: scope is required for external_idp refresh")
	}
	return refreshKiroExternalIDPGrant(ctx, client, refreshToken, clientID, scope, endpoint)
}

// refreshKiroExternalIDPGrant posts the refresh_token grant. Split from the
// validation so the exchange can be exercised against a test endpoint while the
// endpoint rule keeps its own test.
func refreshKiroExternalIDPGrant(ctx context.Context, client *http.Client, refreshToken, clientID, scope, endpoint string) (*TokenResult, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
		"scope":         {scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("kiro: create external_idp refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kiro: external_idp refresh failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("kiro: read external_idp refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kiro: %w", &providers.OAuthRefreshError{Status: resp.StatusCode, Body: truncateBody(body)})
	}

	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("kiro: parse external_idp refresh response: %w", err)
	}
	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("kiro: empty access token in external_idp refresh response")
	}
	out := &TokenResult{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresIn:    tokens.ExpiresIn,
	}
	if out.RefreshToken == "" {
		out.RefreshToken = refreshToken
	}
	return out, nil
}
