package dashboard

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"io"
	"net/http"

	"panrouter/proxy/internal/constants"
	"panrouter/proxy/internal/handlerutil"
)

// Dashboard boot surface: GET /api/init and GET|POST /api/locale.
//
// Both are unauthenticated upstream (src/dashboardGuard.js PUBLIC_API_PATHS:
// "/api/init", "/api/locale"), so they are mounted outside the dashboard auth
// group.

// initBody is the whole of upstream's GET /api/init response
// (src/app/api/init/route.js: `new Response("Initialized", { status: 200 })`).
// Next's string-body Response sets `text/plain;charset=UTF-8` and no caching
// header of its own, and the body carries no state, no id and no credential —
// it exists only so a boot sequence has something to ping.
const initBody = "Initialized"

// initContentType is the media type Next assigns a string Response body.
const initContentType = "text/plain;charset=UTF-8"

// HandleGetInit serves GET /api/init.
//
// The response is a constant: same status, same 11 bytes, no Set-Cookie, no
// cache directive. Upstream sets none either, which is why a client is free to
// cache it; nothing in the body can go stale.
func (h *DashboardHandler) HandleGetInit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(constants.HeaderContentType, initContentType)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(initBody))
}

// localeCookieName and localeCookieMaxAge mirror upstream's LOCALE_COOKIE and
// the 1-year max age the POST sets (src/i18n/config.js, src/app/api/locale/route.js).
const (
	localeCookieName   = "locale"
	localeCookieMaxAge = 60 * 60 * 24 * 365
)

// supportedLocales mirrors LOCALES in src/i18n/config.js. Upstream's list is
// the complete set of translation files under public/i18n/literals, so a locale
// missing here is one the dashboard has no strings for.
var supportedLocales = map[string]bool{
	"en": true, "vi": true, "zh-CN": true, "zh-TW": true, "ja": true,
	"pt-BR": true, "pt-PT": true, "ko": true, "es": true, "de": true,
	"fr": true, "he": true, "ar": true, "ru": true, "pl": true,
	"cs": true, "nl": true, "tr": true, "uk": true, "tl": true,
	"id": true, "km": true, "th": true, "hi": true, "bn": true,
	"ur": true, "ro": true, "sv": true, "it": true, "el": true,
	"hu": true, "fi": true, "da": true, "no": true, "fa": true,
}

// normalizeLocale applies upstream's isSupportedLocale + normalizeLocale to
// the POST body: the tag must be one of LOCALES, and a supported tag maps to
// itself. Upstream's normalizeLocale also aliases a bare "zh" to "zh-CN", but
// the POST validates before it normalizes, so "zh" answers 400 there — the same
// answer this function produces by refusing it.
func normalizeLocale(locale string) (string, bool) {
	if !supportedLocales[locale] {
		return "", false
	}
	return locale, true
}

// localePayload is the POST body upstream reads ({ locale }). The value is
// kept raw so a non-string locale reaches the same 400 an unsupported string
// does — upstream passes whatever the body held to isSupportedLocale, which
// answers false for a number, an object or null.
type localePayload struct {
	Locale jsontext.Value `json:"locale"`
}

// HandleSetLocale serves POST /api/locale.
//
// Upstream writes the normalized locale into the `locale` cookie (path "/",
// one year) and answers {"success":true,"locale":...}; a missing or unsupported
// locale is 400 {"error":"Invalid locale"} and an unparseable body is
// 500 {"error":"Failed to set locale"}.
//
// The body is read through a cap because a locale name is a few bytes: without
// one, an unauthenticated caller could hand this route an unbounded body.
func (h *DashboardHandler) HandleSetLocale(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLocaleBodyBytes))
	if err != nil {
		writeDashboardError(w, http.StatusInternalServerError, "Failed to set locale")
		return
	}
	var payload localePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeDashboardError(w, http.StatusInternalServerError, "Failed to set locale")
		return
	}
	var locale string
	if len(payload.Locale) > 0 {
		// A non-string locale is left empty, which fails the support check below
		// exactly like an unknown tag does.
		if err := json.Unmarshal(payload.Locale, &locale); err != nil {
			locale = ""
		}
	}
	normalized, ok := normalizeLocale(locale)
	if !ok {
		writeDashboardError(w, http.StatusBadRequest, "Invalid locale")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:   localeCookieName,
		Value:  normalized,
		Path:   "/",
		MaxAge: localeCookieMaxAge,
		// Next's cookies().set defaults to SameSite=Lax when the caller names
		// only path and maxAge, so the wire cookie matches upstream's.
		SameSite: http.SameSiteLaxMode,
	})
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"locale":  normalized,
	})
}

// maxLocaleBodyBytes caps the POST body. A locale tag is under 16 bytes; the
// JSON envelope around it adds no more than the key name.
const maxLocaleBodyBytes = 4 << 10

// HandleGetLocale answers GET /api/locale.
//
// Upstream defines no GET for this path (src/app/api/locale/route.js exports
// POST only) and the dashboard's translations are static files under
// /i18n/literals/<locale>.json, never an API response. Next answers the
// unhandled method with 405 and `Allow: POST`; this route answers the same, so
// a client that probes it learns the same thing it learns upstream instead of
// receiving an invented locale catalogue.
func (h *DashboardHandler) HandleGetLocale(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", http.MethodPost)
	w.WriteHeader(http.StatusMethodNotAllowed)
}

// writeDashboardError writes upstream's flat dashboard error body,
// {"error": "<message>"}.
//
// handlerutil.WriteJSONError emits the engine's OpenAI envelope
// ({"error":{"message","type","code"}}). That is the right shape on /v1, where
// clients parse an OpenAI error object, but the dashboard routes it was ported
// from answer a bare string and a client branching on `typeof error === "string"`
// would see an object instead. The MCP bridge below and the locale POST keep
// upstream's shape for that reason.
func writeDashboardError(w http.ResponseWriter, status int, message string) {
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, map[string]any{"error": message})
}
