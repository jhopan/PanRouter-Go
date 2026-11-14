package oauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	json "encoding/json/v2"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"panrouter/proxy/internal/handlerutil"
)

var (
	zedWebBase      = "https://zed.dev"
	zedCloudBase    = "https://cloud.zed.dev"
	zedSignInPath   = "/native_app_signin"
	zedMePath       = "/client/users/me"
	zedSystemHeader = "x-zed-system-id"
	zedDefaultPort  = 58443
	zedKeyPrefix    = "zed-rsa-pkcs1:"
)

// HandleZedAuthorize generates an RSA-2048 keypair and returns the native_app_signin URL.
// GET /api/oauth/zed/authorize?port=... — mirrors upstream zedAuth.js (no local listener;
// the user pastes the redirect URL back, matching the dashboard manual-callback UX).
func (h *OAuthHandler) HandleZedAuthorize(w http.ResponseWriter, r *http.Request) {
	port := zedDefaultPort
	if p := strings.TrimSpace(r.URL.Query().Get("port")); p != "" {
		var v int
		if _, err := fmt.Sscanf(p, "%d", &v); err == nil && v > 0 && v < 65536 {
			port = v
		}
	}
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "generate RSA keypair failed")
		return
	}
	derPub := x509.MarshalPKCS1PublicKey(&priv.PublicKey)
	pubB64 := base64.URLEncoding.EncodeToString(derPub)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	verifier := zedKeyPrefix + base64.RawURLEncoding.EncodeToString(privPEM)
	systemID := randomUUID()

	u, _ := url.Parse(zedWebBase + zedSignInPath)
	q := u.Query()
	q.Set("native_app_port", fmt.Sprint(port))
	q.Set("native_app_public_key", pubB64)
	q.Set("system_id", systemID)
	u.RawQuery = q.Encode()

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"url": u.String(), "authUrl": u.String(),
		"codeVerifier": verifier, "systemId": systemID,
		"nativeAppPort": port, "flowType": "rsa_key_exchange", "provider": "zed",
	})
}

// decodeZedBase64 decodes base64url that may or may not carry padding.
//
// Zed's native_app_signin redirect hands back the ciphertext as padded
// base64url ("...==" at the end for a 2048-bit RSA block), while the
// verifier we mint is unpadded. base64.RawURLEncoding rejects the "="
// padding outright ("illegal base64 data at input byte N"), so decoding
// only the raw form made every real browser callback fail. Try the padded
// form first and fall back to the raw one.
func decodeZedBase64(s string) ([]byte, error) {
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

func decodeZedVerifier(verifier string) ([]byte, error) {
	if !strings.HasPrefix(verifier, zedKeyPrefix) {
		return nil, fmt.Errorf("missing Zed private key verifier; restart the login flow")
	}
	return decodeZedBase64(strings.TrimPrefix(verifier, zedKeyPrefix))
}

func parseZedCallback(raw string) (userID, encrypted string, err error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", "", fmt.Errorf("missing Zed callback URL")
	}
	data := map[string]string{}
	if strings.HasPrefix(text, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(text), &obj); err != nil {
			return "", "", fmt.Errorf("invalid Zed callback JSON")
		}
		for k, v := range obj {
			if s, ok := v.(string); ok {
				data[k] = s
			}
		}
	} else {
		q := text
		if i := strings.Index(text, "?"); i >= 0 {
			q = text[i+1:]
		} else {
			q = strings.TrimPrefix(text, "?")
		}
		vals, _ := url.ParseQuery(q)
		for k := range vals {
			data[k] = vals.Get(k)
		}
	}
	userID = firstNonEmpty(data["user_id"], data["userId"])
	encrypted = firstNonEmpty(data["access_token"], data["accessToken"], data["token"])
	if userID == "" || encrypted == "" {
		return "", "", fmt.Errorf("Zed callback must include user_id and access_token")
	}
	return userID, encrypted, nil
}

func decryptZedToken(encrypted, verifier string) (string, error) {
	pemBytes, err := decodeZedVerifier(verifier)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return "", fmt.Errorf("invalid Zed private key")
	}
	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse Zed private key failed: %v", err)
	}
	cipher, err := decodeZedBase64(encrypted)
	if err != nil {
		return "", fmt.Errorf("decode Zed token failed: %v", err)
	}
	oaepErr := error(nil)
	if plain, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, priv, cipher, nil); err == nil {
		return string(plain), nil
	} else {
		oaepErr = err
	}
	if plain, err := rsa.DecryptPKCS1v15(rand.Reader, priv, cipher); err == nil {
		// PKCS#1 v1.5 unpadding is not integrity-checked: replacement chars
		// prove garbage output — fail loudly instead of storing it.
		if strings.ContainsRune(string(plain), '�') {
			return "", fmt.Errorf("failed to decrypt Zed access token: %v", oaepErr)
		}
		return string(plain), nil
	}
	return "", fmt.Errorf("failed to decrypt Zed access token: %v", oaepErr)
}

// HandleZedExchange decrypts the callback token and stores the connection.
// POST /api/oauth/zed/exchange
func (h *OAuthHandler) HandleZedExchange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"codeVerifier"`
		SystemID     string `json:"systemId"`
		Name         string `json:"name"`
	}
	if raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	userID, encrypted, err := parseZedCallback(body.Code)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	accessToken, err := decryptZedToken(encrypted, body.CodeVerifier)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Best-effort userinfo + organization: a callback that decrypts is a signed-in
	// session even when the profile lookup fails, so the ephemeral failure of the
	// lookup never refuses the login.
	systemID := body.SystemID
	email, name, orgID := "", "", ""
	if user, uerr := h.fetchZedUser(r.Context(), userID, accessToken, systemID); uerr == nil {
		email = user.Email
		name = firstNonEmpty(user.Name, user.DisplayName)
		orgID = user.organizationID()
	}

	connName := connectionDisplayName("zed", body.Name, email, name)
	if connName == name && name == "" {
		connName = "Zed"
	}
	connID := "zed-" + shortHash(accessToken)
	psd := map[string]any{"authMethod": "oauth", "userId": userID}
	if systemID != "" {
		psd["systemId"] = systemID
	}
	if orgID != "" {
		psd["organizationId"] = orgID
	}
	dataMap := map[string]any{"apiKey": accessToken, "accessToken": accessToken}
	if email != "" {
		dataMap["email"] = email
	}
	dataMap["providerSpecificData"] = psd
	if err := h.upsertOAuthConnection(connID, "zed", connName, dataMap); err != nil {
		if errors.Is(err, errConnectionDataMarshal) {
			handlerutil.WriteJSONError(w, http.StatusInternalServerError, "failed to marshal connection data")
			return
		}
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save connection: %v", err))
		return
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"status": "authorized", "id": connID, "connectionId": connID,
		"provider": "zed", "name": connName, "email": email,
		"userId": userID, "organizationId": orgID,
	})
}
