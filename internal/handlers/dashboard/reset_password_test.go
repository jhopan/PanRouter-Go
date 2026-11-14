package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"panrouter/proxy/internal/auth"
)

// The recovery path for a locked-out operator: clear the stored hash so the
// gateway falls back to the configured initial password. Without it there is no
// recovery short of deleting the database, because `password` is a protected
// setting and a PUT through /api/settings is refused.
//
// Upstream's route is a bare POST() that reads no body, so this drives an empty
// request and asserts the stored *state*, never the old bytes: the verifier is
// gone, the old password no longer logs in, and the fallback is back.
func TestHandleResetPasswordClearsTheStoredHash(t *testing.T) {
	ResetResetPasswordLimiter()
	auth.ResetLoginLimiter()
	repo, cleanup := setupSettingsTestDB(t)
	defer cleanup()

	hash, err := bcrypt.GenerateFromPassword([]byte("forgotten"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateSettingsRaw(map[string]any{"password": string(hash)}); err != nil {
		t.Fatalf("seed password: %v", err)
	}
	if h, _ := repo.GetSettingsRaw(); h["password"] == "" {
		t.Fatal("fixture did not store a hash")
	}

	h := NewDashboardHandler(repo, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", nil)
	req.RemoteAddr = "127.0.0.1:41234"
	req.Host = "localhost:20128"
	rec := httptest.NewRecorder()

	h.HandleResetPassword(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, `"success":true`) {
		t.Errorf("body = %s, want upstream's {success:true}", got)
	}
	raw, err := repo.GetSettingsRaw()
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if got, ok := raw["password"].(string); ok && got != "" {
		t.Error("stored password verifier survived the reset")
	}
	// The initial password works again, which is what "reset" has to mean.
	if !h.verifyDashboardPassword(defaultInitialPassword) {
		t.Error("the default password should verify after a reset")
	}
	// And the forgotten one does not.
	if h.verifyDashboardPassword("forgotten") {
		t.Error("the old password still verifies after a reset")
	}

	// A dashboard login with the old password is refused, which is what the
	// operator sees: the hash really is gone, not just shadowed.
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"password":"forgotten"}`))
	loginReq.RemoteAddr = "127.0.0.1:41234"
	loginReq.Host = "localhost:20128"
	loginRec := httptest.NewRecorder()
	h.HandleAuthLogin(loginRec, loginReq)
	if loginRec.Code != http.StatusUnauthorized {
		t.Fatalf("login with the old password status = %d, want 401", loginRec.Code)
	}
	if len(loginRec.Result().Cookies()) != 0 {
		t.Error("a refused login still set a session cookie")
	}
}

// Upstream's guard puts reset-password in LOCAL_ONLY_PATHS, so the network
// origin is the only thing between an exposed tunnel and a password reset.
func TestHandleResetPasswordRefusesNonLocalCallers(t *testing.T) {
	ResetResetPasswordLimiter()
	repo, cleanup := setupSettingsTestDB(t)
	defer cleanup()

	h := NewDashboardHandler(repo, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", nil)
	req.RemoteAddr = "203.0.113.7:5555"
	req.Host = "gateway.example.com"
	rec := httptest.NewRecorder()

	h.HandleResetPassword(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "localhost") {
		t.Errorf("error does not say why: %s", rec.Body.String())
	}

	// Security regression: spoofing Host: localhost must NOT bypass the RemoteAddr gate.
	reqSpoofed := httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", nil)
	reqSpoofed.RemoteAddr = "203.0.113.7:5555"
	reqSpoofed.Host = "localhost"
	recSpoofed := httptest.NewRecorder()
	h.HandleResetPassword(recSpoofed, reqSpoofed)
	if recSpoofed.Code != http.StatusForbidden {
		t.Fatalf("spoofed Host: localhost status = %d, want 403 Forbidden", recSpoofed.Code)
	}
}

// CSRF / Cross-site form POST attacks must be rejected even from loopback.
func TestHandleResetPasswordRefusesCrossSiteCallers(t *testing.T) {
	ResetResetPasswordLimiter()
	repo, cleanup := setupSettingsTestDB(t)
	defer cleanup()

	h := NewDashboardHandler(repo, nil)

	// Subtests share one peer address, so the bucket is cleared per case.
	t.Run("rejects cross-site Sec-Fetch-Site", func(t *testing.T) {
		ResetResetPasswordLimiter()
		req := httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", nil)
		req.RemoteAddr = "127.0.0.1:41234"
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		h.HandleResetPassword(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("cross-site Sec-Fetch-Site status = %d, want 403", rec.Code)
		}
	})

	t.Run("rejects foreign Origin", func(t *testing.T) {
		ResetResetPasswordLimiter()
		req := httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", nil)
		req.RemoteAddr = "127.0.0.1:41234"
		req.Header.Set("Origin", "https://evil.com")
		rec := httptest.NewRecorder()
		h.HandleResetPassword(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("foreign Origin status = %d, want 403", rec.Code)
		}
	})

	t.Run("rejects foreign Referer", func(t *testing.T) {
		ResetResetPasswordLimiter()
		req := httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", nil)
		req.RemoteAddr = "127.0.0.1:41234"
		req.Header.Set("Referer", "https://evil.com/attack.html")
		rec := httptest.NewRecorder()
		h.HandleResetPassword(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("foreign Referer status = %d, want 403", rec.Code)
		}
	})

	t.Run("accepts same-origin localhost Origin", func(t *testing.T) {
		ResetResetPasswordLimiter()
		req := httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", nil)
		req.RemoteAddr = "127.0.0.1:41234"
		req.Header.Set("Origin", "http://localhost:20130")
		rec := httptest.NewRecorder()
		h.HandleResetPassword(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("same-origin localhost Origin status = %d, want 200", rec.Code)
		}
	})
}

// The response must never carry the password literal — a reset that printed it
// would hand the credential to anyone who can reach the endpoint.
func TestHandleResetPasswordDoesNotEchoTheDefault(t *testing.T) {
	ResetResetPasswordLimiter()
	repo, cleanup := setupSettingsTestDB(t)
	defer cleanup()

	h := NewDashboardHandler(repo, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", nil)
	req.RemoteAddr = "127.0.0.1:41234"
	req.Host = "127.0.0.1:20128"
	rec := httptest.NewRecorder()

	h.HandleResetPassword(rec, req)

	if strings.Contains(rec.Body.String(), defaultInitialPassword) {
		t.Errorf("response leaked the default password: %s", rec.Body.String())
	}
}

// Upstream's route reads no body at all: the POST itself is the whole request.
// A client that sends one anyway must not be able to change the outcome, and a
// client that sends nothing must not be refused.
func TestHandleResetPasswordIgnoresTheBody(t *testing.T) {
	repo, cleanup := setupSettingsTestDB(t)
	defer cleanup()

	h := NewDashboardHandler(repo, nil)
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "no body", body: ""},
		{name: "empty object", body: `{}`},
		{name: "unrelated body", body: `{"confirm":false,"password":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ResetResetPasswordLimiter()
			hash, err := bcrypt.GenerateFromPassword([]byte("forgotten"), bcrypt.DefaultCost)
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.UpdateSettingsRaw(map[string]any{"password": string(hash)}); err != nil {
				t.Fatalf("seed password: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", strings.NewReader(tc.body))
			req.RemoteAddr = "127.0.0.1:41234"
			rec := httptest.NewRecorder()
			h.HandleResetPassword(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}

			raw, err := repo.GetSettingsRaw()
			if err != nil {
				t.Fatalf("read settings: %v", err)
			}
			if got, ok := raw["password"].(string); ok && got != "" {
				t.Error("the stored password verifier survived the reset")
			}
		})
	}
}

// "Loopback" is not "trusted": a same-host tunnel forwards strangers from
// 127.0.0.1. The rate limit is what stops one caller from clearing the hash in a
// loop, so it has to bite before the reset runs.
func TestHandleResetPasswordRateLimits(t *testing.T) {
	repo, cleanup := setupSettingsTestDB(t)
	defer cleanup()
	ResetResetPasswordLimiter()

	h := NewDashboardHandler(repo, nil)

	codes := make([]int, 0, resetPasswordMaxPerWindow+1)
	for i := 0; i < resetPasswordMaxPerWindow+1; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", nil)
		req.RemoteAddr = "127.0.0.1:41234"
		rec := httptest.NewRecorder()
		h.HandleResetPassword(rec, req)
		codes = append(codes, rec.Code)
	}

	for i, code := range codes[:resetPasswordMaxPerWindow] {
		if code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, want 200", i, code)
		}
	}
	if last := codes[resetPasswordMaxPerWindow]; last != http.StatusTooManyRequests {
		t.Fatalf("attempt past the budget status = %d, want 429", last)
	}

	// A different peer has its own budget.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/reset-password", nil)
	req.RemoteAddr = "127.0.0.2:41234"
	rec := httptest.NewRecorder()
	h.HandleResetPassword(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("a second peer status = %d, want 200 (budgets are per address)", rec.Code)
	}
}
