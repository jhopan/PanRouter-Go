package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// upstream: src/app/api/init/route.js — `new Response("Initialized", { status: 200 })`.
func TestHandleGetInit(t *testing.T) {
	h := &DashboardHandler{}

	first := httptest.NewRecorder()
	h.HandleGetInit(first, httptest.NewRequest(http.MethodGet, "/api/init", nil))

	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", first.Code)
	}
	if got := first.Body.String(); got != initBody {
		t.Errorf("body = %q, want %q", got, initBody)
	}
	if got := first.Header().Get("Content-Type"); got != initContentType {
		t.Errorf("Content-Type = %q, want %q", got, initContentType)
	}
	// The body is a constant with no state in it, so nothing here may require a
	// fresh response: no cookie, no validator that would force a revalidation,
	// and no cache-busting directive upstream does not send.
	for _, header := range []string{"Set-Cookie", "Cache-Control", "ETag", "Pragma"} {
		if got := first.Header().Get(header); got != "" {
			t.Errorf("%s = %q, want it unset (upstream sets none)", header, got)
		}
	}
	for _, secret := range []string{"sk-", "token", "key", "password", "secret"} {
		if strings.Contains(strings.ToLower(first.Body.String()), strings.ToLower(secret)) {
			t.Errorf("body carries %q; the boot ping must leak nothing", secret)
		}
	}

	second := httptest.NewRecorder()
	h.HandleGetInit(second, httptest.NewRequest(http.MethodGet, "/api/init", nil))
	if second.Body.String() != first.Body.String() {
		t.Errorf("two calls answered differently: %q vs %q", first.Body.String(), second.Body.String())
	}
}

// upstream: src/app/api/locale/route.js POST.
func TestHandleSetLocale(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantLocale string
		wantError  string
	}{
		{name: "a supported locale is stored", body: `{"locale":"vi"}`, wantStatus: http.StatusOK, wantLocale: "vi"},
		{name: "a regional locale is stored verbatim", body: `{"locale":"pt-BR"}`, wantStatus: http.StatusOK, wantLocale: "pt-BR"},
		{name: "unsupported locale is rejected", body: `{"locale":"xx"}`, wantStatus: http.StatusBadRequest, wantError: "Invalid locale"},
		{name: "a bare zh is not a supported tag", body: `{"locale":"zh"}`, wantStatus: http.StatusBadRequest, wantError: "Invalid locale"},
		{name: "missing locale is rejected", body: `{}`, wantStatus: http.StatusBadRequest, wantError: "Invalid locale"},
		{name: "empty locale is rejected", body: `{"locale":""}`, wantStatus: http.StatusBadRequest, wantError: "Invalid locale"},
		{name: "a number is not a locale", body: `{"locale":7}`, wantStatus: http.StatusBadRequest, wantError: "Invalid locale"},
		{name: "malformed JSON is a failure", body: `{"locale":`, wantStatus: http.StatusInternalServerError, wantError: "Failed to set locale"},
		{name: "an empty body is a failure", body: "", wantStatus: http.StatusInternalServerError, wantError: "Failed to set locale"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &DashboardHandler{}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/locale", strings.NewReader(tt.body))
			h.HandleSetLocale(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantError != "" {
				want := `{"error":"` + tt.wantError + `"}`
				if got := strings.TrimSpace(rec.Body.String()); got != want {
					t.Errorf("body = %s, want %s", got, want)
				}
				if cookies := rec.Result().Cookies(); len(cookies) != 0 {
					t.Errorf("a rejected request set cookies: %v", cookies)
				}
				return
			}

			want := `{"locale":"` + tt.wantLocale + `","success":true}`
			if got := strings.TrimSpace(rec.Body.String()); got != want {
				t.Errorf("body = %s, want %s", got, want)
			}
			cookie := findCookie(rec, localeCookieName)
			if cookie == nil {
				t.Fatal("no locale cookie was set")
			}
			if got := rec.Result().Cookies(); len(got) != 1 {
				t.Errorf("cookies = %v, want exactly the locale cookie", got)
			}
			// Upstream sets no cache directive on this response; only the
			// cookie carries the one-year lifetime.
			if got := rec.Header().Get("Cache-Control"); got != "" {
				t.Errorf("Cache-Control = %q, want it unset (upstream sets none)", got)
			}
			if cookie.Value != tt.wantLocale {
				t.Errorf("cookie value = %q, want %q", cookie.Value, tt.wantLocale)
			}
			if cookie.Path != "/" {
				t.Errorf("cookie path = %q, want /", cookie.Path)
			}
			if cookie.MaxAge != localeCookieMaxAge {
				t.Errorf("cookie Max-Age = %d, want %d", cookie.MaxAge, localeCookieMaxAge)
			}
			if cookie.SameSite != http.SameSiteLaxMode {
				t.Errorf("cookie SameSite = %v, want Lax (Next's default)", cookie.SameSite)
			}
		})
	}
}

// Upstream exports POST only, and the dashboard reads translations from
// /i18n/literals/<locale>.json rather than from this route.
func TestHandleGetLocaleAnswersMethodNotAllowed(t *testing.T) {
	h := &DashboardHandler{}
	rec := httptest.NewRecorder()
	h.HandleGetLocale(rec, httptest.NewRequest(http.MethodGet, "/api/locale", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (upstream defines no GET)", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("405 carries a body %q; upstream sends none", rec.Body.String())
	}
}

func findCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}
