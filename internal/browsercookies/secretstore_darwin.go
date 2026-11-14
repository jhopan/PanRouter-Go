//go:build darwin

package browsercookies

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// platformSecrets reads an internet password out of the macOS Keychain.
//
// `security` prints the item's attributes on stdout and — because of -g — the
// password on stderr, so both streams are captured. The password is quoted and
// escaped, which is what the caller gets unescaped here.
func platformSecrets(ctx context.Context, query SecretQuery) ([]Secret, error) {
	if query.Server == "" {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "security", "find-internet-password", "-s", query.Server, "-g")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%w: %v", ErrNoSecretStore, err)
		}
		// The Keychain answered: the item is absent, or the operator declined
		// the access prompt. Both leave nothing to hand back.
		return nil, nil
	}

	password, ok := securityPassword(stderr.String())
	if !ok {
		return nil, nil
	}
	return []Secret{{
		Username:   securityAccount(stdout.String()),
		Value:      password,
		Attributes: map[string]string{"server": query.Server},
	}}, nil
}

var (
	securityPasswordLine  = regexp.MustCompile(`(?m)^password:\s*"(.*)"\s*$`)
	securityEmptyPassword = regexp.MustCompile(`(?m)^password:\s*$`)
	securityQuotedAccount = regexp.MustCompile(`"acct"<blob>="([^"]*)"`)
	securityHexAccount    = regexp.MustCompile(`"acct"<blob>=0x[0-9A-Fa-f]+\s+"([^"]*)"`)
)

// securityAccount reads the account the Keychain item is filed under.
func securityAccount(dump string) string {
	if m := securityQuotedAccount.FindStringSubmatch(dump); len(m) == 2 {
		return m[1]
	}
	if m := securityHexAccount.FindStringSubmatch(dump); len(m) == 2 {
		return m[1]
	}
	return ""
}

// securityPassword reads the password out of `security`'s password dump.
func securityPassword(dump string) (string, bool) {
	if m := securityPasswordLine.FindStringSubmatch(dump); len(m) == 2 {
		return unescapeSecurityPassword(m[1]), true
	}
	if securityEmptyPassword.MatchString(dump) {
		return "", true
	}
	return "", false
}

// unescapeSecurityPassword undoes the escaping `security` applies inside the
// quoted password dump, where \ and " are backslash-escaped.
func unescapeSecurityPassword(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	escaped := false
	for _, r := range value {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
