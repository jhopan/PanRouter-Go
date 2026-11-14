package oauth

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"panrouter/proxy/internal/providers"
)

// codebuddyIntlRefreshURLDefault is the CLI's token refresh endpoint.
// PanRouter's registry carries it as oauth.refreshUrl (open-sse/providers/
// registry/codebuddy-intl.js).
const codebuddyIntlRefreshURLDefault = "https://www.codebuddy.ai/v2/plugin/auth/token/refresh"

// codebuddyIntlRefreshURL is where the CLI refresh is posted. The registry entry
// is the source of truth and wins when it carries a URL; this var stays
// overridable so a test can point the flow at a local server.
var codebuddyIntlRefreshURL = codebuddyIntlRefreshURLDefault

func init() {
	if cfg, ok := providers.KnownOAuthConfigs["codebuddy-intl"]; ok && cfg.TokenURL != "" {
		codebuddyIntlRefreshURL = cfg.TokenURL
	}
	Register("codebuddy-intl", refreshCodeBuddyIntl)
}

// refreshCodeBuddyIntl exchanges the refresh token for a new CodeBuddy Intl
// access token.
//
// Port of upstream `open-sse/services/tokenRefresh/providers.js`
// (refreshCodebuddyIntlToken). The refresh token rides in the X-Refresh-Token
// header, not a form body: the official CLI never posts a grant_type to this
// endpoint.
//
// The CLI refreshes with the same identity it chats with — the account id (the
// token's "sub" claim), a per-request id, and the CLI user agent. A refresh
// that looks like a different client is a refresh that gets treated
// differently, which is how one account ended up presenting two identities
// (IDE/2.160.0 here, CLI/2.178.0 on the chat path) and getting restricted.
func refreshCodeBuddyIntl(ctx context.Context, p *Params) (*TokenResult, error) {
	if p.RefreshToken == "" {
		return nil, fmt.Errorf("codebuddy-intl: no refresh token available")
	}

	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}

	headers := map[string]string{
		"Content-Type":     "application/json",
		"Accept":           "application/json, text/plain, */*",
		"User-Agent":       providers.CodeBuddyUserAgent,
		"X-Requested-With": "XMLHttpRequest",
		"X-Domain":         providers.CodeBuddyIntlDomain,
		"X-Refresh-Token":  p.RefreshToken,
		// The plugin source is what the CLI reports for a refresh issued by the
		// CodeBuddy plugin rather than by an interactive login.
		"X-Auth-Refresh-Source": "plugin",
		"X-Product":             "SaaS",
		"X-Request-ID":          strings.ReplaceAll(uuid.NewString(), "-", ""),
	}
	if uid := providers.CodeBuddyUserIDFromToken(p.AccessToken); uid != "" {
		headers["X-User-Id"] = uid
	}
	// The CLI also sends the (still valid) access token as a bearer on the
	// refresh call. Without it the refresh looks like it comes from a client
	// that never talks to the chat API.
	if p.AccessToken != "" {
		headers["Authorization"] = "Bearer " + p.AccessToken
	}

	tokenURL := codebuddyIntlRefreshURL

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("codebuddy-intl create refresh request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codebuddy-intl refresh: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("codebuddy-intl read refresh response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codebuddy-intl %w", &providers.OAuthRefreshError{
			Status: resp.StatusCode,
			Body:   truncateBody(body),
		})
	}

	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int    `json:"expiresIn"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("codebuddy-intl parse refresh response: %w", err)
	}
	if payload.Code != 0 || payload.Data.AccessToken == "" {
		msg := payload.Msg
		if msg == "" {
			msg = "no token"
		}
		return nil, fmt.Errorf("codebuddy-intl token refresh returned no token (code %d: %s)", payload.Code, msg)
	}

	refreshToken := payload.Data.RefreshToken
	if refreshToken == "" {
		refreshToken = p.RefreshToken
	}
	return &TokenResult{
		AccessToken:  payload.Data.AccessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    payload.Data.ExpiresIn,
	}, nil
}
