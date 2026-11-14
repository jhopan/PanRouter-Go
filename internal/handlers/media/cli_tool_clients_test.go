package media

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"panrouter/proxy/internal/clientconfig"
)

// The ten clients whose upstream settings routes the dashboard's CLI Tools view
// calls and which had no Go adapter: codewhale, copilot, cowork, deepseek-tui,
// devin, forge, grok-build, jcode, openclaw, smelt.
//
// Each route is the same three verbs over internal/clientconfig; these tests
// pin the three observable things per client:
//
//   - GET on a missing config answers 200 with the client's file list and an
//     empty plan (the shape upstream returns when its readFile throws),
//   - POST writes exactly the bytes upstream's route writes, and a second POST
//     changes nothing,
//   - an unreadable config is reported as an error rather than overwritten.

const (
	cliClientBaseURL = "http://localhost:20128"
	cliClientAPIKey  = "sk-test"
	cliClientModel   = "cc/claude-sonnet-5"
	cliClientModel2  = "glm/glm-5.3"
)

// cliClientBody is the request every case posts. The single-model clients read
// model; the array clients read models.
const cliClientBody = `{"baseUrl":"http://localhost:20128","apiKey":"sk-test","model":"cc/claude-sonnet-5","models":["cc/claude-sonnet-5","glm/glm-5.3"]}`

// cliClientCase is one client's expectations.
type cliClientCase struct {
	id string
	// want maps a home-relative path to the exact bytes the POST writes.
	want map[string]string
	// primary is the home-relative file made unreadable by the error test.
	primary string
}

// cliClientHome points every path resolver at a temp tree and empties PATH, so
// neither a developer's install nor their real config can leak into a test.
func cliClientHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE"} {
		t.Setenv(key, home)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", filepath.Join(home, "roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "local"))
	t.Setenv("PATH", filepath.Join(home, "empty-path"))
	return home
}

// cliClientWant is the table. Every byte below is what the upstream route
// writes for the request above; the tests assert equality, not substrings.
func cliClientWant() []cliClientCase {
	coworkProfileID := "00000000-0000-4000-8000-000000000001"
	coworkRoot, coworkFirstParty := coworkRootsForTest()
	return []cliClientCase{
		{
			id:      "codewhale",
			primary: filepath.Join(".codewhale", "config.toml"),
			want: map[string]string{
				filepath.Join(".codewhale", "config.toml"): `[openai]
base_url = "http://localhost:20128/v1"
api_key = "sk-test"
model = "cc/claude-sonnet-5"
`,
			},
		},
		{
			id:      "forge",
			primary: filepath.Join(".forge", "config.toml"),
			want: map[string]string{
				filepath.Join(".forge", "config.toml"): `[openai]
api_key = "sk-test"
base_url = "http://localhost:20128/v1"
model = "cc/claude-sonnet-5"
`,
			},
		},
		{
			id:      "deepseek-tui",
			primary: filepath.Join(".deepseek", "config.toml"),
			want: map[string]string{
				filepath.Join(".deepseek", "config.toml"): `provider = "openai"

[providers.openai]
base_url = "http://localhost:20128/v1"
api_key = "sk-test"
model = "cc/claude-sonnet-5"
`,
			},
		},
		{
			id:      "grok-build",
			primary: filepath.Join(".grok", "config.toml"),
			want: map[string]string{
				filepath.Join(".grok", "config.toml"): `[model.9router]
model = "cc/claude-sonnet-5"
base_url = "http://localhost:20128/v1"
name = "9Router"
description = "Routed via 9Router gateway"
api_backend = "chat_completions"
api_key = "sk-test"

[models]
default = "9router"
`,
			},
		},
		{
			id:      "jcode",
			primary: filepath.Join(".jcode", "config.toml"),
			want: map[string]string{
				filepath.Join(".jcode", "config.toml"): `[providers.9router]
type = "openai-compatible"
base_url = "http://localhost:20128/v1"
auth = "bearer"
api_key_env = "JCODE_9ROUTER_API_KEY"
env_file = "provider-9router.env"
default_model = "cc/claude-sonnet-5"
requires_api_key = true
`,
				filepath.Join(".config", "jcode", "provider-9router.env"): `JCODE_9ROUTER_API_KEY = "sk-test"
`,
			},
		},
		{
			id:      "smelt",
			primary: filepath.Join(".smelt", "config.json"),
			want: map[string]string{
				filepath.Join(".smelt", "config.json"): `{"baseUrl": "http://localhost:20128/v1", "apiKey": "sk-test", "model": "cc/claude-sonnet-5", "_managedBy": "PanRouter"}`,
			},
		},
		{
			id:      "openclaw",
			primary: filepath.Join(".openclaw", "openclaw.json"),
			want: map[string]string{
				filepath.Join(".openclaw", "openclaw.json"): `{"models": {"providers": {"9router": {"api":"openai-completions","apiKey":"sk-test","baseUrl":"http://localhost:20128/v1","models":[{"id":"cc/claude-sonnet-5","name":"claude-sonnet-5"},{"id":"glm/glm-5.3","name":"glm-5.3"}]}}}, "agents": {"defaults": {"model": {"primary": "9router/cc/claude-sonnet-5"}, "models": {"9router/cc/claude-sonnet-5":{},"9router/glm/glm-5.3":{}}}}}`,
			},
		},
		{
			id:      "copilot",
			primary: filepath.Join(copilotUserDirForTest(), "chatLanguageModels.json"),
			want: map[string]string{
				filepath.Join(copilotUserDirForTest(), "chatLanguageModels.json"): `[
  {
    "name": "9Router",
    "vendor": "azure",
    "apiKey": "sk-test",
    "models": [
      {
        "id": "cc/claude-sonnet-5",
        "name": "cc/claude-sonnet-5",
        "url": "http://localhost:20128/chat/completions#models.ai.azure.com",
        "toolCalling": true,
        "vision": false,
        "maxInputTokens": 128000,
        "maxOutputTokens": 16000
      },
      {
        "id": "glm/glm-5.3",
        "name": "glm/glm-5.3",
        "url": "http://localhost:20128/chat/completions#models.ai.azure.com",
        "toolCalling": true,
        "vision": false,
        "maxInputTokens": 128000,
        "maxOutputTokens": 16000
      }
    ]
  }
]`,
			},
		},
		{
			id:      "cowork",
			primary: filepath.Join(coworkRoot, "configLibrary", "_meta.json"),
			want: map[string]string{
				filepath.Join(coworkRoot, "config.json"): `{"operonSkipMcpApprovals": {"exa":true,"tavily":true}}`,
				filepath.Join(coworkRoot, "configLibrary", "_meta.json"): `{
  "appliedId": "` + coworkProfileID + `",
  "entries": [
    {
      "id": "` + coworkProfileID + `",
      "name": "Default"
    }
  ]
}`,
				filepath.Join(coworkRoot, "configLibrary", coworkProfileID+".json"): coworkProfileGolden(),
				filepath.Join(coworkFirstParty, "claude_desktop_config.json"):       `{"deploymentMode": "3p"}`,
			},
		},
		{
			id: "devin",
			// No config file to write: the binary owns its own auth.
			want: map[string]string{},
		},
	}
}

// coworkRootsForTest mirrors paths_clients.go's candidates for the current OS.
func coworkRootsForTest() (writeRoot, firstParty string) {
	switch runtime.GOOS {
	case "darwin":
		base := filepath.Join("Library", "Application Support")
		return filepath.Join(base, "Claude-3p"), filepath.Join(base, "Claude")
	case "windows":
		return filepath.Join("local", "Claude-3p"), filepath.Join("roaming", "Claude")
	}
	return filepath.Join(".config", "Claude-3p"), filepath.Join(".config", "Claude")
}

func copilotUserDirForTest() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join("Library", "Application Support", "Code", "User")
	case "windows":
		return filepath.Join("roaming", "Code", "User")
	}
	return filepath.Join(".config", "Code", "User")
}

// coworkProfileGolden is the 3p profile document: SECURITY_RELAX, the gateway,
// both requested models and the two default remote MCP servers.
func coworkProfileGolden() string {
	return `{
  "coworkEgressAllowedHosts": [
    "*"
  ],
  "disabledBuiltinTools": [],
  "isLocalDevMcpEnabled": true,
  "isDesktopExtensionEnabled": true,
  "isDesktopExtensionDirectoryEnabled": true,
  "isDesktopExtensionSignatureRequired": false,
  "isClaudeCodeForDesktopEnabled": true,
  "disableEssentialTelemetry": true,
  "disableNonessentialTelemetry": true,
  "disableNonessentialServices": true,
  "inferenceProvider": "gateway",
  "inferenceGatewayBaseUrl": "http://localhost:20128",
  "inferenceGatewayApiKey": "sk-test",
  "inferenceModels": [
    {
      "name": "cc/claude-sonnet-5"
    },
    {
      "name": "glm/glm-5.3"
    }
  ],
  "managedMcpServers": [
    {
      "name": "exa",
      "url": "https://mcp.exa.ai/mcp",
      "transport": "http",
      "toolPolicy": {
        "exa-web_fetch_exa": "allow",
        "exa-web_search_exa": "allow",
        "web_fetch_exa": "allow",
        "web_search_exa": "allow"
      }
    },
    {
      "name": "tavily",
      "url": "https://mcp.tavily.com/mcp",
      "transport": "http",
      "oauth": true,
      "toolPolicy": {
        "tavily-tavily_crawl": "allow",
        "tavily-tavily_extract": "allow",
        "tavily-tavily_map": "allow",
        "tavily-tavily_search": "allow",
        "tavily_crawl": "allow",
        "tavily_extract": "allow",
        "tavily_map": "allow",
        "tavily_search": "allow"
      }
    }
  ]
}`
}

// TestCLIToolClientsPostWritesWhatUpstreamWrites asserts the exact bytes each
// route writes, and that a second POST is a no-op.
func TestCLIToolClientsPostWritesWhatUpstreamWrites(t *testing.T) {
	for _, tc := range cliClientWant() {
		t.Run(tc.id, func(t *testing.T) {
			home := cliClientHome(t)
			h := NewCLIToolHandler(nil)

			rec := postCLITool(t, h, tc.id)
			if rec.Code != http.StatusOK {
				t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if resp["success"] != true || resp["tool"] != tc.id {
				t.Errorf("response = %v, want success for %s", resp, tc.id)
			}
			assertCLIFiles(t, home, tc.want)

			// A second apply must not rewrite a byte: the writer reports
			// changed=false and the files stay as the golden fixture wrote them.
			again := postCLITool(t, h, tc.id)
			if again.Code != http.StatusOK {
				t.Fatalf("second POST status = %d, body = %s", again.Code, again.Body.String())
			}
			var second map[string]any
			if err := json.Unmarshal(again.Body.Bytes(), &second); err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if second["changed"] != false {
				t.Errorf("second POST changed = %v, want false", second["changed"])
			}
			assertCLIFiles(t, home, tc.want)
		})
	}
}

// TestCLIToolClientsGetOnMissingConfig pins the read side: no config directory
// yet is a 200 with the client's paths and an empty plan, never a 500.
func TestCLIToolClientsGetOnMissingConfig(t *testing.T) {
	for _, tc := range cliClientWant() {
		t.Run(tc.id, func(t *testing.T) {
			home := cliClientHome(t)
			h := NewCLIToolHandler(nil)
			req := httptest.NewRequest(http.MethodGet, "/api/cli-tools/"+tc.id+"-settings?home="+home, nil)
			req.URL.Path = "/api/cli-tools/" + tc.id + "-settings"
			req.URL.RawQuery = "home=" + home
			rec := httptest.NewRecorder()
			h.HandleCLIToolGet(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if resp["tool"] != tc.id {
				t.Errorf("tool = %v, want %s", resp["tool"], tc.id)
			}
			if resp["installed"] != false {
				t.Errorf("installed = %v, want false on a missing config", resp["installed"])
			}
			files, _ := resp["files"].([]any)
			if want := resolvedFileCount(t, tc.id, home); len(files) != want {
				t.Errorf("files = %v, want %d entries", files, want)
			}
			if _, present := resp["plan"]; !present {
				t.Errorf("plan missing from the response: %v", resp)
			}
		})
	}
}

// TestCLIToolClientsUnreadableConfigIsReported makes the client's own config
// path unreadable (a directory where the file should be) and asserts the route
// reports it instead of overwriting it.
func TestCLIToolClientsUnreadableConfigIsReported(t *testing.T) {
	for _, tc := range cliClientWant() {
		if tc.primary == "" {
			continue // devin owns no file
		}
		t.Run(tc.id, func(t *testing.T) {
			home := cliClientHome(t)
			blocked := filepath.Join(home, tc.primary)
			if err := os.MkdirAll(blocked, 0o755); err != nil {
				t.Fatalf("create blocking directory: %v", err)
			}
			h := NewCLIToolHandler(nil)
			rec := postCLITool(t, h, tc.id)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("POST status = %d, want 500; body = %s", rec.Code, rec.Body.String())
			}
			var resp struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("parse error response: %v", err)
			}
			if strings.TrimSpace(resp.Error.Message) == "" {
				t.Errorf("error response carries no message: %s", rec.Body.String())
			}
			// The blocker is still a directory: nothing was written over it.
			info, err := os.Stat(blocked)
			if err != nil || !info.IsDir() {
				t.Errorf("the unreadable path was replaced (info=%v err=%v)", info, err)
			}
		})
	}
}

func postCLITool(t *testing.T, h *CLIToolHandler, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/cli-tools/"+id+"-settings", strings.NewReader(cliClientBody))
	req.URL.Path = "/api/cli-tools/" + id + "-settings"
	rec := httptest.NewRecorder()
	h.HandleCLIToolPost(rec, req)
	return rec
}

// resolvedFileCount is the number of files the adapter itself reports for a
// client, so the expectation follows paths_clients.go instead of restating it.
func resolvedFileCount(t *testing.T, id, home string) int {
	t.Helper()
	adapter, err := clientconfig.Resolve(id)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", id, err)
	}
	paths, err := adapter.Paths(clientconfig.Options{Home: home})
	if err != nil {
		t.Fatalf("Paths(%s): %v", id, err)
	}
	return len(paths.Files)
}

// assertCLIFiles writes the golden fixture into a temp directory, then compares
// every file the route wrote with the fixture's bytes and fails on anything
// extra. The comparison is byte-for-byte: a key order, an indent or a trailing
// newline that drifts shows up here.
func assertCLIFiles(t *testing.T, home string, want map[string]string) {
	t.Helper()
	golden := t.TempDir()
	for rel, content := range want {
		path := filepath.Join(golden, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("golden mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("golden write %s: %v", rel, err)
		}
	}

	seen := make(map[string]bool, len(want))
	err := filepath.Walk(home, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(home, path)
		if relErr != nil {
			return relErr
		}
		seen[rel] = true
		if _, ok := want[rel]; !ok {
			t.Errorf("unexpected file written: %s", rel)
			return nil
		}
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		expected, goldenErr := os.ReadFile(filepath.Join(golden, rel))
		if goldenErr != nil {
			return goldenErr
		}
		if string(got) != string(expected) {
			t.Errorf("%s bytes = %q, want golden %q", rel, got, expected)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", home, err)
	}
	for rel := range want {
		if !seen[rel] {
			t.Errorf("expected file %s was not written", rel)
		}
	}
}
