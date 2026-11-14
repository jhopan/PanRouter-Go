package dashboard

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// The current-password rule lives on the settings password change, not on the
// reset route (which exists for an operator who no longer has the password).
// Upstream's PATCH /api/settings answers "Invalid current password" when the
// supplied value does not verify against the stored hash.
func TestChangeDashboardPasswordRequiresTheCurrentPassword(t *testing.T) {
	repo, cleanup := setupSettingsTestDB(t)
	defer cleanup()

	h := NewDashboardHandler(repo, nil)

	hash, err := bcrypt.GenerateFromPassword([]byte("current-pass"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateSettingsRaw(map[string]any{"password": string(hash)}); err != nil {
		t.Fatalf("seed password: %v", err)
	}

	t.Run("mismatch is refused with upstream's message", func(t *testing.T) {
		err := h.changeDashboardPassword("wrong-pass", "new-pass")
		if err == nil {
			t.Fatal("a wrong current password was accepted")
		}
		if got := err.Error(); got != "Invalid current password" {
			t.Errorf("error = %q, want upstream's %q", got, "Invalid current password")
		}
		// The stored verifier is untouched by a refused change.
		raw, err := repo.GetSettingsRaw()
		if err != nil {
			t.Fatalf("read settings: %v", err)
		}
		if got, _ := raw["password"].(string); got != string(hash) {
			t.Error("a refused change rewrote the stored verifier")
		}
	})

	t.Run("correct current password stores a new verifier", func(t *testing.T) {
		if err := h.changeDashboardPassword("current-pass", "new-pass"); err != nil {
			t.Fatalf("changeDashboardPassword: %v", err)
		}
		raw, err := repo.GetSettingsRaw()
		if err != nil {
			t.Fatalf("read settings: %v", err)
		}
		stored, _ := raw["password"].(string)
		if stored == "" {
			t.Fatal("no verifier was stored")
		}
		if stored == string(hash) {
			t.Error("the verifier was not replaced")
		}
		if err := bcrypt.CompareHashAndPassword([]byte(stored), []byte("new-pass")); err != nil {
			t.Errorf("the stored verifier does not match the new password: %v", err)
		}
		if h.verifyDashboardPassword("current-pass") {
			t.Error("the old password still verifies after the change")
		}
	})
}

// With no stored hash, upstream accepts an empty current password or the
// well-known "123456" default for the first-time set, and refuses anything
// else with the same message.
func TestChangeDashboardPasswordFirstTimeSet(t *testing.T) {
	repo, cleanup := setupSettingsTestDB(t)
	defer cleanup()

	h := NewDashboardHandler(repo, nil)

	for _, tc := range []struct {
		name    string
		current string
		wantErr string
	}{
		{name: "empty current", current: ""},
		{name: "default current", current: defaultInitialPassword},
		{name: "arbitrary current", current: "not-it", wantErr: "Invalid current password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := repo.UpdateSettingsRaw(map[string]any{"password": ""}); err != nil {
				t.Fatalf("clear password: %v", err)
			}
			err := h.changeDashboardPassword(tc.current, "first-pass")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("changeDashboardPassword: %v", err)
				}
				if !h.verifyDashboardPassword("first-pass") {
					t.Error("the new password does not verify")
				}
				return
			}
			if err == nil {
				t.Fatal("an invalid first-time current password was accepted")
			}
			if got := err.Error(); got != tc.wantErr {
				t.Errorf("error = %q, want %q", got, tc.wantErr)
			}
		})
	}
}
