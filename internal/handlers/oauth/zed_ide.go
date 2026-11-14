package oauth

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"panrouter/proxy/internal/browsercookies"
)

const (
	// zedDefaultCredentialsURL is the credential URL Zed uses when its settings
	// name none.
	zedDefaultCredentialsURL = "https://zed.dev"
	// zedKeyringLabel is the label Zed files its keyring item under. The label
	// is a preference, not a requirement: an item under another label is still
	// Zed's when its attributes and account are.
	zedKeyringLabel = "zed-github-account"
	// zedSystemIDKey is the kv_store key holding the id the IDE sends to
	// cloud.zed.dev.
	zedSystemIDKey = "system_id"
)

// ZedCredentialReader reads the signed-in Zed IDE session off this machine.
//
// It is an interface so the auto-import lane can be exercised without a real
// keyring, a real Zed install, or a real database — and so a machine that
// cannot answer says so through the reader instead of through the handler.
type ZedCredentialReader interface {
	ReadZedCredentials(ctx context.Context) (ZedIdeCredentials, error)
}

// ZedIdeCredentials is what a read of the machine found.
//
// Found is the whole answer: when it is false, Error carries the reason the
// lane reports. An error return is reserved for a reader that failed to run at
// all, which the route answers with 500 — the same split upstream makes
// between a not-found result and a thrown exception.
type ZedIdeCredentials struct {
	Found          bool
	UserID         string
	AccessToken    string
	SystemID       string
	CredentialsURL string
	Error          string
}

// platformZedCredentials reads Zed's session the way the IDE itself stores it:
// the credential in the OS secret store, the system id in Zed's local SQLite.
type platformZedCredentials struct{}

// ReadZedCredentials reads the credential URL Zed is configured for, then the
// session filed under it.
func (platformZedCredentials) ReadZedCredentials(ctx context.Context) (ZedIdeCredentials, error) {
	credentialsURL := zedCredentialsURL()
	userID, accessToken, err := readZedKeyringSecret(ctx, credentialsURL)
	if err != nil {
		return ZedIdeCredentials{Error: err.Error(), CredentialsURL: credentialsURL}, nil
	}
	if userID == "" || accessToken == "" {
		return ZedIdeCredentials{
			Error:          "Zed IDE session not found in the system keyring. Sign in to Zed, then retry.",
			CredentialsURL: credentialsURL,
		}, nil
	}
	systemID := readZedSystemID()
	if systemID == "" {
		systemID = randomUUID()
	}
	return ZedIdeCredentials{
		Found:          true,
		UserID:         userID,
		AccessToken:    accessToken,
		SystemID:       systemID,
		CredentialsURL: credentialsURL,
	}, nil
}

// readZedKeyringSecret reads the account and token out of the platform's
// secret store.
//
// The credentials URL is what Zed keys its item by, and the macOS Keychain is
// asked under the bare host as well because older installs filed it that way.
func readZedKeyringSecret(ctx context.Context, credentialsURL string) (userID, accessToken string, err error) {
	for _, query := range zedSecretQueries(credentialsURL) {
		secrets, err := browsercookies.ReadSecrets(ctx, query)
		if err != nil {
			return "", "", err
		}
		if picked, ok := pickZedSecret(secrets); ok {
			return picked.Username, picked.Value, nil
		}
	}
	return "", "", nil
}

// zedSecretQueries names the same item the way each platform's store is
// addressed: a credential target on Windows, an internet-password server on
// macOS, secret-service attributes elsewhere.
func zedSecretQueries(credentialsURL string) []browsercookies.SecretQuery {
	switch runtime.GOOS {
	case "windows":
		return []browsercookies.SecretQuery{{Target: "zed:url=" + credentialsURL}}
	case "darwin":
		servers := []string{credentialsURL, stripURLScheme(credentialsURL), "zed.dev"}
		queries := make([]browsercookies.SecretQuery, 0, len(servers))
		seen := make(map[string]bool, len(servers))
		for _, server := range servers {
			if server == "" || seen[server] {
				continue
			}
			seen[server] = true
			queries = append(queries, browsercookies.SecretQuery{Server: server})
		}
		return queries
	default:
		return []browsercookies.SecretQuery{{
			Attributes: map[string]string{"url": credentialsURL},
		}}
	}
}

// pickZedSecret prefers the item Zed labels as its own, then any item that
// carries both an account and a secret.
func pickZedSecret(secrets []browsercookies.Secret) (browsercookies.Secret, bool) {
	for _, secret := range secrets {
		if secret.Label == zedKeyringLabel && secret.Username != "" && secret.Value != "" {
			return secret, true
		}
	}
	for _, secret := range secrets {
		if secret.Username != "" && secret.Value != "" {
			return secret, true
		}
	}
	return browsercookies.Secret{}, false
}

// zedCredentialsURL resolves the credential URL Zed keys its session by.
//
// A non-default server is stored in Zed's settings, and the session is filed
// under whatever that server is, so the settings are read first and the
// default host is only the fallback.
func zedCredentialsURL() string {
	for _, path := range zedSettingsPaths() {
		raw, ok := readSmallFile(path)
		if !ok {
			continue
		}
		var settings struct {
			CredentialsURL string `json:"credentials_url"`
			ServerURL      string `json:"server_url"`
		}
		if err := json.Unmarshal(raw, &settings); err != nil {
			continue
		}
		if url := firstNonEmpty(settings.CredentialsURL, settings.ServerURL); url != "" {
			return strings.TrimRight(url, "/")
		}
	}
	return zedDefaultCredentialsURL
}

// readSmallFile reads a settings file, refusing anything that is not a settings
// file's size instead of pulling it into memory whole.
func readSmallFile(path string) ([]byte, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		return nil, false
	}
	return raw, true
}

// zedSettingsPaths are the places Zed keeps settings.json on this platform.
func zedSettingsPaths() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return []string{filepath.Join(home, "Library", "Application Support", "Zed", "settings.json")}
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			local = filepath.Join(home, "AppData", "Local")
		}
		return []string{filepath.Join(local, "Zed", "settings.json")}
	default:
		config := os.Getenv("XDG_CONFIG_HOME")
		if config == "" {
			config = filepath.Join(home, ".config")
		}
		return []string{filepath.Join(config, "zed", "settings.json")}
	}
}

// zedGlobalDBPaths are the candidate Zed databases that hold the global
// kv_store, one per release channel.
func zedGlobalDBPaths() []string {
	home, _ := os.UserHomeDir()
	channels := []string{"0-global", "0-stable", "0-preview"}
	var base string
	switch runtime.GOOS {
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support", "Zed", "db")
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			local = filepath.Join(home, "AppData", "Local")
		}
		base = filepath.Join(local, "Zed", "db")
	default:
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" {
			data = filepath.Join(home, ".local", "share")
		}
		base = filepath.Join(data, "zed", "db")
	}
	paths := make([]string, 0, len(channels))
	for _, channel := range channels {
		paths = append(paths, filepath.Join(base, channel, "db.sqlite"))
	}
	return paths
}

// readZedSystemID reads the system id Zed's IDE sends to cloud.zed.dev.
//
// It is a preference, not a requirement: a machine that has never stored one
// still signs in, with an id minted for this import.
func readZedSystemID() string {
	for _, path := range zedGlobalDBPaths() {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if value := queryZedKvStore(path, zedSystemIDKey); value != "" {
			return value
		}
	}
	return ""
}

// queryZedKvStore reads one key out of Zed's kv_store.
//
// The database is opened read-only and immutable, the same way the Cursor
// import reads its state database: the IDE may be running and writing, and the
// reader must not take a write lock on the file it is only looking at.
func queryZedKvStore(path, key string) string {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		return ""
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var value string
	if err := db.QueryRowContext(ctx, "SELECT value FROM kv_store WHERE key = ? LIMIT 1", key).Scan(&value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// stripURLScheme drops the scheme and trailing slashes from a URL, which is how
// older Zed installs filed the Keychain item.
func stripURLScheme(raw string) string {
	stripped := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	return strings.TrimRight(stripped, "/")
}

// zedUserIDString renders the body's userId as upstream's String(body.userId)
// would. Zed sends the id as a number in some payloads and as a string in
// others, and both name the same account.
func zedUserIDString(raw jsontext.Value) string {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return ""
	}
	if strings.HasPrefix(text, `"`) {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return ""
		}
		return strings.TrimSpace(id)
	}
	return strings.Trim(text, `"`)
}
