package db

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Backup download tokens.
//
// A backup token lets an unattended machine (the Debian mirror) pull the
// database over HTTP. It is deliberately NOT an API key:
//
//   - different prefix, so the two are never confused in a log or a cron file
//   - scoped to exactly one route (GET /api/backup/download) — an API key
//     reaches the whole LLM surface, which is far too much for a mirror
//   - the machine id is embedded, so a leaked token says which machine it
//     belongs to
//   - the CRC makes the token self-validating: a typo is rejected without a
//     database lookup
//
// Format: bk-{machineId}-{keyId6}-{crc8}
//
// The full token is never stored: only its key id, the machine id and the CRC.
// The CRC is an HMAC over the machine id and the key id, keyed with a secret
// that never leaves the server, so the record can be checked without keeping
// the credential itself. Port of src/shared/utils/backupToken.js and
// src/lib/db/repos/backupTokensRepo.js.
const (
	backupTokenPrefix = "bk"
	backupKeyIDLen    = 6
	backupCRCLen      = 8

	// backupTokenSecretEnv lets an operator rotate the HMAC key. The default
	// matches upstream so tokens minted by either runtime verify on both.
	backupTokenSecretEnv     = "BACKUP_TOKEN_SECRET"
	backupTokenDefaultSecret = "panrouter-backup-token-secret"

	backupKeyIDAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// BackupToken is one stored mirror credential, in the shape the dashboard
// lists. The full token is absent by construction: it exists only in the
// response to the POST that created it.
type BackupToken struct {
	ID         string  `json:"id"`
	KeyID      string  `json:"keyId"`
	Name       string  `json:"name"`
	MachineID  string  `json:"machineId"`
	CRC        string  `json:"crc"`
	IsActive   bool    `json:"isActive"`
	LastUsedAt *string `json:"lastUsedAt"`
	CreatedAt  string  `json:"createdAt"`
}

func backupTokenSecret() string {
	if v := strings.TrimSpace(os.Getenv(backupTokenSecretEnv)); v != "" {
		return v
	}
	return backupTokenDefaultSecret
}

// backupTokenCRC is the self-validating tail: an HMAC over the machine id and
// key id, truncated to 8 hex characters. Truncation is safe here because the
// value only proves a typo was not made; the record lookup is what grants
// access.
func backupTokenCRC(machineID, keyID string) string {
	mac := hmac.New(sha256.New, []byte(backupTokenSecret()))
	mac.Write([]byte(machineID + keyID))
	return hex.EncodeToString(mac.Sum(nil))[:backupCRCLen]
}

// generateBackupKeyID mints the 6-character key id. crypto/rand.Int is used
// rather than a modulo over random bytes so every character keeps the same
// probability — a biased id space is a smaller id space.
func generateBackupKeyID() (string, error) {
	max := big.NewInt(int64(len(backupKeyIDAlphabet)))
	out := make([]byte, backupKeyIDLen)
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate backup key id: %w", err)
		}
		out[i] = backupKeyIDAlphabet[n.Int64()]
	}
	return string(out), nil
}

// GenerateBackupToken mints a token for a machine. The caller gets the full
// token exactly once; the stored record keeps only its parts.
func GenerateBackupToken(machineID string) (token, keyID, crc string, err error) {
	if strings.TrimSpace(machineID) == "" {
		return "", "", "", errors.New("machineId is required")
	}
	keyID, err = generateBackupKeyID()
	if err != nil {
		return "", "", "", err
	}
	crc = backupTokenCRC(machineID, keyID)
	return backupTokenPrefix + "-" + machineID + "-" + keyID + "-" + crc, keyID, crc, nil
}

// parseBackupToken splits a presented token, reporting whether its shape and
// CRC are right. ok=false means the value is not a backup token at all.
func parseBackupToken(token string) (machineID, keyID, crc string, valid, ok bool) {
	parts := strings.Split(token, "-")
	if len(parts) != 4 {
		return "", "", "", false, false
	}
	if parts[0] != backupTokenPrefix || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return "", "", "", false, false
	}
	expected := backupTokenCRC(parts[1], parts[2])
	valid = len(parts[3]) == len(expected) &&
		subtle.ConstantTimeCompare([]byte(expected), []byte(parts[3])) == 1
	return parts[1], parts[2], parts[3], valid, true
}

func (r *Repo) scanBackupToken(row interface{ Scan(...any) error }) (*BackupToken, error) {
	var (
		tok      BackupToken
		name     sql.NullString
		machine  sql.NullString
		lastUsed sql.NullString
		isActive sql.NullInt64
	)
	if err := row.Scan(&tok.ID, &tok.KeyID, &name, &machine, &tok.CRC, &isActive, &lastUsed, &tok.CreatedAt); err != nil {
		return nil, err
	}
	tok.Name = name.String
	tok.MachineID = machine.String
	tok.IsActive = isActive.Int64 == 1
	if lastUsed.Valid {
		v := lastUsed.String
		tok.LastUsedAt = &v
	}
	return &tok, nil
}

// GetBackupTokens lists every token, oldest first, the way the dashboard
// renders its table.
func (r *Repo) GetBackupTokens() ([]BackupToken, error) {
	rows, err := r.db.Query(
		`SELECT id, keyId, name, machineId, crc, isActive, lastUsedAt, createdAt
		   FROM backupTokens ORDER BY createdAt ASC`)
	if err != nil {
		return nil, fmt.Errorf("list backup tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	list := make([]BackupToken, 0, 4)
	for rows.Next() {
		tok, err := r.scanBackupToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scan backup token: %w", err)
		}
		list = append(list, *tok)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backup tokens: %w", err)
	}
	return list, nil
}

// GetBackupTokenByKeyID resolves a token record by its key id. A missing key
// id is nil, nil: the caller reports "unknown token", not a read failure.
func (r *Repo) GetBackupTokenByKeyID(keyID string) (*BackupToken, error) {
	if keyID == "" {
		return nil, nil
	}
	tok, err := r.scanBackupToken(r.db.QueryRow(
		`SELECT id, keyId, name, machineId, crc, isActive, lastUsedAt, createdAt
		   FROM backupTokens WHERE keyId = ? LIMIT 1`, keyID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get backup token %s: %w", keyID, err)
	}
	return tok, nil
}

// CreateBackupToken stores a new token for machineID and returns the record
// plus the full token, which is the only time it exists.
func (r *Repo) CreateBackupToken(name, machineID string) (*BackupToken, string, error) {
	token, keyID, crc, err := GenerateBackupToken(machineID)
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(name) == "" {
		name = "backup mirror"
	}
	tok := &BackupToken{
		ID:        uuid.NewString(),
		KeyID:     keyID,
		Name:      name,
		MachineID: machineID,
		CRC:       crc,
		IsActive:  true,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if _, err := r.db.Exec( //nolint:noctx
		`INSERT INTO backupTokens(id, keyId, name, machineId, crc, isActive, lastUsedAt, createdAt)
		 VALUES(?, ?, ?, ?, ?, 1, NULL, ?)`,
		tok.ID, tok.KeyID, tok.Name, tok.MachineID, tok.CRC, tok.CreatedAt,
	); err != nil {
		return nil, "", fmt.Errorf("create backup token: %w", err)
	}
	return tok, token, nil
}

// SetBackupTokenActive revokes (false) or re-enables (true) a token without
// deleting its history. It returns whether a row changed, which the handler
// turns into its 404.
func (r *Repo) SetBackupTokenActive(id string, active bool) (bool, error) {
	flag := 0
	if active {
		flag = 1
	}
	res, err := r.db.Exec( //nolint:noctx
		`UPDATE backupTokens SET isActive = ? WHERE id = ?`, flag, id)
	if err != nil {
		return false, fmt.Errorf("set backup token %s active=%v: %w", id, active, err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set backup token %s: %w", id, err)
	}
	return changed > 0, nil
}

// DeleteBackupToken removes a token permanently.
func (r *Repo) DeleteBackupToken(id string) (bool, error) {
	res, err := r.db.Exec( //nolint:noctx
		`DELETE FROM backupTokens WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete backup token %s: %w", id, err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete backup token %s: %w", id, err)
	}
	return changed > 0, nil
}

// CheckBackupToken verifies a presented token. The CRC is checked first — a
// typo or a mangled token is rejected without touching the database — then the
// key id is looked up and the record must exist and be active.
//
// The reason strings are the upstream ones verbatim: they are what the mirror's
// cron log shows when a download is refused, and they are short enough to be
// safe to reveal (none of them distinguishes a wrong key id from a wrong
// machine's token beyond what the caller already knows).
func (r *Repo) CheckBackupToken(token string) (ok bool, reason string, record *BackupToken) {
	_, keyID, crc, valid, shaped := parseBackupToken(token)
	if !shaped {
		return false, "Malformed token", nil
	}
	if !valid {
		return false, "Invalid token", nil
	}
	record, err := r.GetBackupTokenByKeyID(keyID)
	if err != nil {
		return false, "Invalid token", nil
	}
	if record == nil {
		return false, "Unknown token", nil
	}
	if !record.IsActive {
		return false, "Token has been revoked", nil
	}
	if record.CRC != crc {
		return false, "Invalid token", nil
	}
	return true, "", record
}

// TouchBackupToken records that a token was used, so the dashboard can show
// activity. Best effort: a failed touch must not break a download.
func (r *Repo) TouchBackupToken(keyID string) error {
	if keyID == "" {
		return nil
	}
	if _, err := r.db.Exec( //nolint:noctx
		`UPDATE backupTokens SET lastUsedAt = ? WHERE keyId = ?`,
		time.Now().UTC().Format(time.RFC3339), keyID); err != nil {
		return fmt.Errorf("touch backup token %s: %w", keyID, err)
	}
	return nil
}
