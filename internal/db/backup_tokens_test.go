package db

import (
	"encoding/json"
	"strings"
	"testing"
)

// The token store keeps only the parts needed to verify a token: the full value
// is returned once, by the create call, and is never readable back.
func TestBackupTokenStoreKeepsOnlyTheParts(t *testing.T) {
	repo := openEphemeralSchemaDB(t)

	record, token, err := repo.CreateBackupToken("debian-mirror", "any")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(token, "bk-any-") {
		t.Fatalf("token = %q, want the bk-<machine>-<keyId>-<crc> shape", token)
	}
	parts := strings.Split(token, "-")
	if len(parts) != 4 {
		t.Fatalf("token = %q, want four dash-separated parts", token)
	}
	if record.KeyID != parts[2] {
		t.Errorf("stored keyId = %q, want %q", record.KeyID, parts[2])
	}
	if record.MachineID != "any" || record.Name != "debian-mirror" || !record.IsActive {
		t.Errorf("record = %+v, want the created values", record)
	}

	var storedCRC string
	if err := repo.RawDB().QueryRow(`SELECT crc FROM backupTokens WHERE keyId = ?`, record.KeyID).Scan(&storedCRC); err != nil {
		t.Fatalf("read stored crc: %v", err)
	}
	if storedCRC != parts[3] {
		t.Errorf("stored crc = %q, want the token's tail %q", storedCRC, parts[3])
	}
	var columns int
	if err := repo.RawDB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('backupTokens') WHERE name = 'token'`).Scan(&columns); err != nil {
		t.Fatalf("inspect columns: %v", err)
	}
	if columns != 0 {
		t.Error("backupTokens carries a token column; the credential must not be stored")
	}
}

// Verification rejects a token it did not mint, and a revoked one, with the
// upstream reason strings the mirror's cron log shows.
func TestCheckBackupToken(t *testing.T) {
	repo := openEphemeralSchemaDB(t)

	_, token, err := repo.CreateBackupToken("mirror", "any")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if ok, reason, _ := repo.CheckBackupToken(token); !ok || reason != "" {
		t.Fatalf("fresh token: ok=%v reason=%q, want accepted", ok, reason)
	}

	tests := []struct {
		name       string
		token      string
		wantReason string
	}{
		{"empty", "", "Malformed token"},
		{"wrong prefix", "sk-machine-keyid-crc", "Malformed token"},
		{"three parts", "bk-machine-keyid", "Malformed token"},
		{"bad crc", "bk-any-abcdef-01234567", "Invalid token"},
		{"unknown key id", "bk-any-zzzzzz-01234567", "Invalid token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason, record := repo.CheckBackupToken(tt.token)
			if ok {
				t.Fatalf("CheckBackupToken(%q) accepted the token", tt.token)
			}
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
			if record != nil {
				t.Errorf("record = %+v, want nil on refusal", record)
			}
		})
	}

	// A well-shaped token whose key id is not stored is "unknown", not
	// "invalid": the CRC proves the shape, the lookup fails.
	stored, err := repo.GetBackupTokenByKeyID(strings.Split(token, "-")[2])
	if err != nil || stored == nil {
		t.Fatalf("reload token: %v (record %v)", err, stored)
	}

	// Revocation is reported distinctly, so an operator can tell "wrong token"
	// from "I turned that one off".
	if changed, err := repo.SetBackupTokenActive(stored.ID, false); err != nil || !changed {
		t.Fatalf("revoke: changed=%v err=%v", changed, err)
	}
	if ok, reason, _ := repo.CheckBackupToken(token); ok || reason != "Token has been revoked" {
		t.Fatalf("revoked token: ok=%v reason=%q", ok, reason)
	}
	if changed, err := repo.SetBackupTokenActive(stored.ID, true); err != nil || !changed {
		t.Fatalf("enable: changed=%v err=%v", changed, err)
	}
	if ok, _, _ := repo.CheckBackupToken(token); !ok {
		t.Error("re-enabled token was refused")
	}

	// Deleting removes it entirely.
	if changed, err := repo.DeleteBackupToken(stored.ID); err != nil || !changed {
		t.Fatalf("delete: changed=%v err=%v", changed, err)
	}
	if ok, reason, _ := repo.CheckBackupToken(token); ok || reason != "Unknown token" {
		t.Errorf("deleted token: ok=%v reason=%q", ok, reason)
	}
}

// The core schema creates the backupTokens table, so a fresh database can mint
// a token without a migration step.
func TestCoreSchemaIncludesBackupTokens(t *testing.T) {
	repo := openEphemeralSchemaDB(t)
	names := schemaTableNames(t, repo)
	if !names["backupTokens"] {
		t.Fatal("core schema does not create backupTokens")
	}
}

// Releasing a connection lifts the locks and ban fields a provider quota read
// has disproved, and leaves the rest of the connection alone.
func TestReleaseConnectionBlocks(t *testing.T) {
	repo := openEphemeralSchemaDB(t)
	data := `{"apiKey":"sk-live","baseUrl":"https://example.test","custom":"keep-me",` +
		`"modelLock_gpt-4o":"2099-01-01T00:00:00Z","modelLock_claude":"2099-01-01T00:00:00Z",` +
		`"bannedUntil":"2099-01-01T00:00:00Z","banReason":"403","rateLimitedUntil":"2099-01-01T00:00:00Z",` +
		`"quotaExhaustedUntil":"2099-01-01T00:00:00Z","errorCode":"rate_limit","lastError":"429",` +
		`"lastErrorAt":"2026-11-15T00:00:00Z","testStatus":"error","backoffLevel":4}`
	if _, err := repo.RawDB().Exec(
		`INSERT INTO providerConnections (id, provider, authType, name, isActive, data, createdAt, updatedAt)
		 VALUES ('conn-1', 'minimax-code', 'oauth', 'Primary', 1, ?, '2026-11-15T00:00:00Z', '2026-11-15T00:00:00Z')`,
		data,
	); err != nil {
		t.Fatalf("seed connection: %v", err)
	}

	if err := repo.ReleaseConnectionBlocks("conn-1"); err != nil {
		t.Fatalf("release: %v", err)
	}

	var raw string
	if err := repo.RawDB().QueryRow(`SELECT data FROM providerConnections WHERE id = 'conn-1'`).Scan(&raw); err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}

	for _, key := range []string{
		"modelLock_gpt-4o", "modelLock_claude", "bannedUntil", "banReason",
		"rateLimitedUntil", "quotaExhaustedUntil", "errorCode", "lastError", "lastErrorAt",
	} {
		if value, present := got[key]; !present || value != nil {
			t.Errorf("%s = %v (present=%v), want null", key, value, present)
		}
	}
	if got["testStatus"] != "active" {
		t.Errorf("testStatus = %v, want active", got["testStatus"])
	}
	if got["backoffLevel"] != float64(0) {
		t.Errorf("backoffLevel = %v, want 0", got["backoffLevel"])
	}
	// Nothing else on the connection is touched.
	if got["apiKey"] != "sk-live" || got["custom"] != "keep-me" || got["baseUrl"] != "https://example.test" {
		t.Errorf("release dropped unrelated fields: %v", got)
	}

	// An unknown connection is an error, not a silent success.
	if err := repo.ReleaseConnectionBlocks("absent"); err == nil {
		t.Error("releasing an unknown connection succeeded")
	}
}
