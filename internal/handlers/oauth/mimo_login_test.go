package oauth

import (
	"context"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleMimoLoginStart_WalksTheLoginSurface pins the start contract: two
// upstream hops are walked server-side (me -> SSO -> login page), the browser
// gets the same-origin path of that page, and the session — including the
// cookies the hops seeded — rides out in the httpOnly session cookie.
func TestHandleMimoLoginStart_WalksTheLoginSurface(t *testing.T) {
	handler := NewOAuthHandler(nil)
	handler.mimoProxyProbe = func(context.Context) string { return "" }

	var hops []string
	handler.mimoHop = func(_ context.Context, sess *mimoLoginSession, target, ua string) (mimoHopResult, error) {
		hops = append(hops, target+"|"+ua)
		switch len(hops) {
		case 1:
			if want := "https://mimo-server-sgp.xiaomimimo.com/api/user/xiaomi/me"; target != want {
				t.Errorf("first hop = %q, want %q", target, want)
			}
			if ua != mimoAPIUA {
				t.Errorf("first hop UA = %q, want the desktop API UA", ua)
			}
			// The hop's Set-Cookie would be absorbed here; seeding the jar
			// directly is what the status test then observes inside the cookie.
			sess.Jar[mimoJarKey(mimoLoginCookie{Name: "deviceId", Domain: mimoAccountHost, Path: "/"})] = mimoLoginCookie{
				Name: "deviceId", Value: "dev-1", Domain: mimoAccountHost, Path: "/",
			}
			return mimoHopResult{Status: http.StatusFound, Location: "https://account.xiaomi.com/pass/serviceLogin?sid=mimosgp"}, nil
		default:
			if !strings.Contains(sess.cookieHeader(target), "deviceId=dev-1") {
				t.Errorf("second hop did not carry the seeded cookie: %q", sess.cookieHeader(target))
			}
			if ua != mimoSSOUA {
				t.Errorf("second hop UA = %q, want the SSO UA", ua)
			}
			return mimoHopResult{Status: http.StatusFound, Location: "https://account.xiaomi.com/fe/service/login?sid=mimosgp&v=1"}, nil
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/oauth/xiaomi-mimo/login/start", strings.NewReader(`{"region":"sgp"}`))
	req.Host = "gateway.local"
	rec := httptest.NewRecorder()
	handler.HandleMimoLoginStart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(hops) != 2 {
		t.Fatalf("walked %d hops, want 2: %v", len(hops), hops)
	}

	var resp struct {
		Success bool   `json:"success"`
		State   string `json:"state"`
		PageURL string `json:"pageUrl"`
		Region  string `json:"region"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success || resp.State == "" {
		t.Fatalf("unexpected response shape: %s", rec.Body.String())
	}
	if resp.Region != "sgp" {
		t.Errorf("region = %q, want sgp", resp.Region)
	}
	if resp.PageURL != "/fe/service/login?sid=mimosgp&v=1" {
		t.Errorf("pageUrl = %q, want the login page's same-origin path", resp.PageURL)
	}

	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == mimoSessionCookie {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("start did not set the login session cookie")
	}
	if !sessionCookie.HttpOnly {
		t.Error("session cookie must be httpOnly")
	}
	if sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", sessionCookie.SameSite)
	}
	decoded := decodeMimoSessionCookie(sessionCookie.Value)
	if decoded == nil {
		t.Fatal("session cookie does not decode")
	}
	if decoded.State != resp.State || decoded.Region != "sgp" {
		t.Errorf("decoded session = %+v, want state %q region sgp", decoded, resp.State)
	}
	if got := decoded.Jar[mimoJarKey(mimoLoginCookie{Name: "deviceId", Domain: mimoAccountHost, Path: "/"})].Value; got != "dev-1" {
		t.Errorf("decoded jar deviceId = %q, want dev-1", got)
	}
}

// TestHandleMimoLoginStart_RefusesUnexpectedHops pins the two upstream 502s:
// an me response that does not redirect to the account host, and a serviceLogin
// response that carries no page.
func TestHandleMimoLoginStart_RefusesUnexpectedHops(t *testing.T) {
	tests := []struct {
		name    string
		hops    []mimoHopResult
		wantMsg string
	}{
		{
			name:    "me answers without an account redirect",
			hops:    []mimoHopResult{{Status: http.StatusOK}},
			wantMsg: "Unexpected me response (200) — no account redirect",
		},
		{
			name: "serviceLogin answers without a location",
			hops: []mimoHopResult{
				{Status: http.StatusFound, Location: "https://account.xiaomi.com/pass/serviceLogin"},
				{Status: http.StatusOK},
			},
			wantMsg: "Unexpected serviceLogin response (200)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewOAuthHandler(nil)
			handler.mimoProxyProbe = func(context.Context) string { return "" }
			index := 0
			handler.mimoHop = func(context.Context, *mimoLoginSession, string, string) (mimoHopResult, error) {
				if index >= len(tt.hops) {
					t.Fatalf("unexpected extra hop")
				}
				res := tt.hops[index]
				index++
				return res, nil
			}

			req := httptest.NewRequest(http.MethodPost, "/api/oauth/xiaomi-mimo/login/start", strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			handler.HandleMimoLoginStart(rec, req)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantMsg) {
				t.Errorf("response %s is missing %q", rec.Body.String(), tt.wantMsg)
			}
		})
	}
}

// TestHandleMimoLoginStatus_ReportsPendingThenComplete pins the poll contract:
// a session with no identity yet answers pending (and re-arms the cookie), one
// whose poller produced an identity answers done with the one-shot clear.
func TestHandleMimoLoginStatus_ReportsPendingThenComplete(t *testing.T) {
	handler := NewOAuthHandler(nil)
	sess := newMimoLoginSession("cn")
	cookieValue := encodeMimoSessionCookie(sess)
	if cookieValue == "" {
		t.Fatal("could not encode the session cookie")
	}

	handler.mimoPoll = func(*mimoLoginSession) *mimoIdentity { return nil }
	req := httptest.NewRequest(http.MethodGet, "/api/oauth/xiaomi-mimo/login/status?state="+sess.State, nil)
	req.AddCookie(&http.Cookie{Name: mimoSessionCookie, Value: cookieValue})
	rec := httptest.NewRecorder()
	handler.HandleMimoLoginStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("pending status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var pending struct {
		Status string `json:"status"`
		Region string `json:"region"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
		t.Fatalf("decode pending body: %v", err)
	}
	if pending.Status != "pending" || pending.Region != "cn" {
		t.Errorf("pending body = %+v, want pending/cn", pending)
	}
	rearmed := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == mimoSessionCookie && c.Value != "" && c.MaxAge > 0 {
			rearmed = true
		}
	}
	if !rearmed {
		t.Error("a pending poll must re-arm the session cookie")
	}

	handler.mimoPoll = func(*mimoLoginSession) *mimoIdentity {
		return &mimoIdentity{PassToken: "pt-1", UserID: "u-1", CUserID: "c-1"}
	}
	req = httptest.NewRequest(http.MethodGet, "/api/oauth/xiaomi-mimo/login/status?state="+sess.State, nil)
	req.AddCookie(&http.Cookie{Name: mimoSessionCookie, Value: cookieValue})
	rec = httptest.NewRecorder()
	handler.HandleMimoLoginStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("done status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var done struct {
		Status    string  `json:"status"`
		Region    string  `json:"region"`
		PassToken string  `json:"passToken"`
		UserID    *string `json:"userId"`
		CUserID   *string `json:"cUserId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil {
		t.Fatalf("decode done body: %v", err)
	}
	if done.Status != "done" || done.PassToken != "pt-1" || done.Region != "cn" {
		t.Errorf("done body = %+v, want done with the captured passToken", done)
	}
	if done.UserID == nil || *done.UserID != "u-1" || done.CUserID == nil || *done.CUserID != "c-1" {
		t.Errorf("done identity = %+v, want u-1/c-1", done)
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == mimoSessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("a completed poll must clear the session cookie (one-shot identity)")
	}
}

// TestHandleMimoLoginStatus_ExpiredSession pins the 404 that sends the modal
// back to a fresh start: a missing cookie and a state mismatch both expire.
func TestHandleMimoLoginStatus_ExpiredSession(t *testing.T) {
	handler := NewOAuthHandler(nil)
	sess := newMimoLoginSession("cn")
	cookieValue := encodeMimoSessionCookie(sess)

	tests := []struct {
		name   string
		state  string
		cookie string
	}{
		{name: "no cookie", state: sess.State, cookie: ""},
		{name: "state mismatch", state: "someone-elses-state", cookie: cookieValue},
		{name: "garbage cookie", state: sess.State, cookie: "not-a-session"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/oauth/xiaomi-mimo/login/status?state="+tt.state, nil)
			if tt.cookie != "" {
				req.AddCookie(&http.Cookie{Name: mimoSessionCookie, Value: tt.cookie})
			}
			rec := httptest.NewRecorder()
			handler.HandleMimoLoginStatus(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body.Status != "expired" {
				t.Errorf("body status = %q, want expired", body.Status)
			}
		})
	}
}

// TestHandleMimoLoginStatus_DefaultPollerReadsTheJar pins the production poller:
// only account.xiaomi.com cookies count as identity, so a passToken the cluster
// host set is not a login.
func TestHandleMimoLoginStatus_DefaultPollerReadsTheJar(t *testing.T) {
	sess := newMimoLoginSession("cn")
	sess.Jar[mimoJarKey(mimoLoginCookie{Name: "passToken", Domain: "mimo-server-cn.xiaomimimo.com", Path: "/"})] = mimoLoginCookie{
		Name: "passToken", Value: "cluster-decoy", Domain: "mimo-server-cn.xiaomimimo.com", Path: "/",
	}
	handler := NewOAuthHandler(nil)

	req := httptest.NewRequest(http.MethodGet, "/api/oauth/xiaomi-mimo/login/status?state="+sess.State, nil)
	req.AddCookie(&http.Cookie{Name: mimoSessionCookie, Value: encodeMimoSessionCookie(sess)})
	rec := httptest.NewRecorder()
	handler.HandleMimoLoginStatus(rec, req)

	var body struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Status != "pending" {
		t.Fatalf("status = %q, want pending for a cluster-host-only cookie", body.Status)
	}

	sess.Jar[mimoJarKey(mimoLoginCookie{Name: "passToken", Domain: mimoAccountHost, Path: "/"})] = mimoLoginCookie{
		Name: "passToken", Value: "pt-account", Domain: mimoAccountHost, Path: "/",
	}
	req = httptest.NewRequest(http.MethodGet, "/api/oauth/xiaomi-mimo/login/status?state="+sess.State, nil)
	req.AddCookie(&http.Cookie{Name: mimoSessionCookie, Value: encodeMimoSessionCookie(sess)})
	rec = httptest.NewRecorder()
	handler.HandleMimoLoginStatus(rec, req)

	var done struct {
		Status    string `json:"status"`
		PassToken string `json:"passToken"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &done)
	if done.Status != "done" || done.PassToken != "pt-account" {
		t.Errorf("body = %+v, want done with the account-host passToken", done)
	}
}
