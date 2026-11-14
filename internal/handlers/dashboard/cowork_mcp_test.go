package dashboard

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	json "encoding/json/v2"
)

// coworkFetchCall is one recorded outbound request.
type coworkFetchCall struct {
	method  string
	url     string
	headers map[string]string
	body    []byte
}

// stubCoworkFetch replaces the package fetch seam and records every call.
func stubCoworkFetch(t *testing.T, respond func(call int, c coworkFetchCall) (coworkHTTPResponse, error)) *[]coworkFetchCall {
	t.Helper()
	orig := coworkMCPDo
	calls := []coworkFetchCall{}
	coworkMCPDo = func(_ context.Context, method, rawURL string, headers map[string]string, body []byte) (coworkHTTPResponse, error) {
		c := coworkFetchCall{method: method, url: rawURL, headers: headers, body: append([]byte(nil), body...)}
		calls = append(calls, c)
		return respond(len(calls)-1, c)
	}
	t.Cleanup(func() { coworkMCPDo = orig })
	return &calls
}

// coworkJSONResponse builds a response with a JSON content type.
func coworkJSONResponse(status int, body string) coworkHTTPResponse {
	return coworkHTTPResponse{
		Status: status,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   []byte(body),
	}
}

// getCoworkRegistry issues the registry GET and decodes the envelope.
func getCoworkRegistry(t *testing.T, r http.Handler, target string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode registry response: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

// postCoworkTools issues a tools probe and decodes the result object.
func postCoworkTools(t *testing.T, r http.Handler, body, remoteAddr string) (int, coworkMCPProbeResult, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/cli-tools/cowork-mcp-tools", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var probe coworkMCPProbeResult
	var raw map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decode tools response: %v (%s)", err, rec.Body.String())
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &probe)
	}
	return rec.Code, probe, raw
}

func registryServerList(t *testing.T, out map[string]any) []any {
	t.Helper()
	servers, ok := out["servers"].([]any)
	if !ok {
		t.Fatalf("servers is not an array: %#v", out["servers"])
	}
	return servers
}

// TestCoworkMCPRegistry_UpstreamShapeAndFilters pins one page's mapping and the
// isDirectConnect rules: plaintext, claude.ai-mediated, tenant-scoped and
// templated URLs never reach the dashboard.
func TestCoworkMCPRegistry_UpstreamShapeAndFilters(t *testing.T) {
	calls := stubCoworkFetch(t, func(int, coworkFetchCall) (coworkHTTPResponse, error) {
		return coworkJSONResponse(http.StatusOK, `{
			"servers": [
				{
					"server": {"name": "alpha", "title": "Alpha", "description": "First",
						"remotes": [{"url": "https://alpha.example.com/mcp", "type": "sse"}]},
					"_meta": {"com.anthropic.api/mcp-registry": {
						"slug": "alpha-slug", "displayName": "Alpha Display", "oneLiner": "one liner",
						"isAuthless": true, "toolNames": ["t1", "t2"],
						"iconUrl": "https://cdn.example.com/alpha.png"}}
				},
				{
					"server": {"name": "defaults",
						"remotes": [{"url": "https://defaults.example.com/mcp"}]}
				},
				{
					"server": {"name": "plaintext",
						"remotes": [{"url": "http://plain.example.com/mcp"}]}
				},
				{
					"server": {"name": "mediated",
						"remotes": [{"url": "https://mcp.claude.com/sse"}]}
				},
				{
					"server": {"name": "tenant",
						"remotes": [{"url": "https://tenant.example.com/mcp"}]},
					"_meta": {"com.anthropic.api/mcp-registry": {"requiredFields": ["token"]}}
				},
				{
					"server": {"name": "templated",
						"remotes": [{"url": "https://x.example.com/{tenant}/mcp"}]}
				}
			],
			"metadata": {}
		}`), nil
	})

	repo, cleanup := setupTestDB(t)
	defer cleanup()
	h := NewDashboardHandler(repo)
	r := setupTestRouterWith(repo, h)

	code, out := getCoworkRegistry(t, r, "/api/cli-tools/cowork-mcp-registry")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %#v", code, out)
	}
	if out["cached"] != false {
		t.Errorf("first fetch must not report cached, got %#v", out["cached"])
	}
	if got := out["total"]; got != float64(2) {
		t.Errorf("total = %#v, want 2", got)
	}
	servers := registryServerList(t, out)
	if len(servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(servers))
	}

	alpha, _ := servers[0].(map[string]any)
	for key, want := range map[string]any{
		"name":        "alpha",
		"slug":        "alpha-slug",
		"title":       "Alpha",
		"description": "First",
		"url":         "https://alpha.example.com/mcp",
		"transport":   "sse",
		"oauth":       false,
		"toolCount":   float64(2),
		"iconUrl":     "https://cdn.example.com/alpha.png",
	} {
		if got := alpha[key]; got != want {
			t.Errorf("alpha.%s = %#v, want %#v", key, got, want)
		}
	}
	if names, _ := alpha["toolNames"].([]any); len(names) != 2 {
		t.Errorf("alpha.toolNames = %#v, want two entries", alpha["toolNames"])
	}

	fallback, _ := servers[1].(map[string]any)
	for key, want := range map[string]any{
		"name":        "defaults",
		"slug":        "defaults",
		"title":       "defaults",
		"description": "",
		"transport":   "http",
		"oauth":       true,
		"toolCount":   float64(0),
	} {
		if got := fallback[key]; got != want {
			t.Errorf("defaults.%s = %#v, want %#v", key, got, want)
		}
	}
	if fallback["iconUrl"] != nil {
		t.Errorf("defaults.iconUrl = %#v, want null", fallback["iconUrl"])
	}
	if names, _ := fallback["toolNames"].([]any); len(names) != 0 {
		t.Errorf("defaults.toolNames = %#v, want []", fallback["toolNames"])
	}

	if len(*calls) != 1 {
		t.Fatalf("expected one registry page, got %d calls", len(*calls))
	}
	if !strings.Contains((*calls)[0].url, "limit=500") ||
		!strings.Contains((*calls)[0].url, "visibility=commercial,gsuite,gsuite-google") {
		t.Errorf("registry URL lost upstream's paging parameters: %s", (*calls)[0].url)
	}
}

// TestCoworkMCPRegistry_CacheTTLAndRefresh covers the acceptance contract: a
// second call inside the TTL does not fetch, a stale entry refetches exactly
// once, and refresh=1 bypasses a fresh entry.
func TestCoworkMCPRegistry_CacheTTLAndRefresh(t *testing.T) {
	calls := stubCoworkFetch(t, func(int, coworkFetchCall) (coworkHTTPResponse, error) {
		return coworkJSONResponse(http.StatusOK, `{"servers":[{"server":{"name":"alpha","remotes":[{"url":"https://alpha.example.com/mcp"}]}}],"metadata":{}}`), nil
	})

	repo, cleanup := setupTestDB(t)
	defer cleanup()
	h := NewDashboardHandler(repo)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	h.coworkMCPNow = func() time.Time { return now }
	r := setupTestRouterWith(repo, h)

	if code, out := getCoworkRegistry(t, r, "/api/cli-tools/cowork-mcp-registry"); code != http.StatusOK || out["cached"] != false {
		t.Fatalf("first call = %d %#v, want 200 cached=false", code, out["cached"])
	}
	if len(*calls) != 1 {
		t.Fatalf("first call fetched %d times, want 1", len(*calls))
	}

	if _, out := getCoworkRegistry(t, r, "/api/cli-tools/cowork-mcp-registry"); out["cached"] != true {
		t.Fatalf("second call cached = %#v, want true", out["cached"])
	}
	if len(*calls) != 1 {
		t.Fatalf("second call inside TTL fetched again: %d calls", len(*calls))
	}

	now = now.Add(59 * time.Minute)
	if _, out := getCoworkRegistry(t, r, "/api/cli-tools/cowork-mcp-registry"); out["cached"] != true {
		t.Fatalf("call at TTL-1m cached = %#v, want true", out["cached"])
	}
	if len(*calls) != 1 {
		t.Fatalf("call at TTL-1m refetched: %d calls", len(*calls))
	}

	now = now.Add(2 * time.Minute) // 61 minutes after the cached entry
	if _, out := getCoworkRegistry(t, r, "/api/cli-tools/cowork-mcp-registry"); out["cached"] != false {
		t.Fatalf("stale call cached = %#v, want false", out["cached"])
	}
	if len(*calls) != 2 {
		t.Fatalf("stale entry should refetch once, got %d calls", len(*calls))
	}
	if _, out := getCoworkRegistry(t, r, "/api/cli-tools/cowork-mcp-registry"); out["cached"] != true {
		t.Fatalf("post-refetch call cached = %#v, want true", out["cached"])
	}
	if len(*calls) != 2 {
		t.Fatalf("post-refetch call fetched again: %d calls", len(*calls))
	}

	if _, out := getCoworkRegistry(t, r, "/api/cli-tools/cowork-mcp-registry?refresh=1"); out["cached"] != false {
		t.Fatalf("refresh=1 cached = %#v, want false", out["cached"])
	}
	if len(*calls) != 3 {
		t.Fatalf("refresh=1 should fetch, got %d calls", len(*calls))
	}
}

// TestCoworkMCPRegistry_PagesAndDedupes pins cursor handling and first-wins
// dedupe by url.
func TestCoworkMCPRegistry_PagesAndDedupes(t *testing.T) {
	calls := stubCoworkFetch(t, func(call int, c coworkFetchCall) (coworkHTTPResponse, error) {
		if call == 0 {
			return coworkJSONResponse(http.StatusOK, `{
				"servers": [{"server":{"name":"one","remotes":[{"url":"https://one.example.com/mcp"}]}}],
				"metadata": {"nextCursor": "cur/1&2"}
			}`), nil
		}
		if !strings.Contains(c.url, "cursor=cur%2F1%262") {
			t.Errorf("second page cursor not escaped: %s", c.url)
		}
		return coworkJSONResponse(http.StatusOK, `{
			"servers": [
				{"server":{"name":"one-again","remotes":[{"url":"https://one.example.com/mcp"}]}},
				{"server":{"name":"two","remotes":[{"url":"https://two.example.com/mcp"}]}}
			],
			"metadata": {}
		}`), nil
	})

	repo, cleanup := setupTestDB(t)
	defer cleanup()
	r := setupTestRouterWith(repo, NewDashboardHandler(repo))

	_, out := getCoworkRegistry(t, r, "/api/cli-tools/cowork-mcp-registry")
	servers := registryServerList(t, out)
	if len(servers) != 2 {
		t.Fatalf("servers = %d, want 2 after dedupe", len(servers))
	}
	first, _ := servers[0].(map[string]any)
	if first["name"] != "one" {
		t.Errorf("dedupe should keep the first entry, got %#v", first["name"])
	}
	if len(*calls) != 2 {
		t.Errorf("expected two pages, got %d calls", len(*calls))
	}
}

// TestCoworkMCPRegistry_CapsServerCount pins the bound on one fetch: the cache
// can hold at most coworkRegistryMaxServers entries, and paging stops once the
// cap is reached instead of buffering an unbounded catalogue.
func TestCoworkMCPRegistry_CapsServerCount(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"servers":[`)
	for i := range coworkRegistryMaxServers + 1 {
		if i > 0 {
			body.WriteString(",")
		}
		body.WriteString(`{"server":{"name":"s`)
		body.WriteString(strconv.Itoa(i))
		body.WriteString(`","remotes":[{"url":"https://s`)
		body.WriteString(strconv.Itoa(i))
		body.WriteString(`.example.com/mcp"}]}}`)
	}
	body.WriteString(`],"metadata":{"nextCursor":"more"}}`)

	calls := stubCoworkFetch(t, func(int, coworkFetchCall) (coworkHTTPResponse, error) {
		return coworkJSONResponse(http.StatusOK, body.String()), nil
	})

	repo, cleanup := setupTestDB(t)
	defer cleanup()
	r := setupTestRouterWith(repo, NewDashboardHandler(repo))

	_, out := getCoworkRegistry(t, r, "/api/cli-tools/cowork-mcp-registry")
	if got := out["total"]; got != float64(coworkRegistryMaxServers) {
		t.Errorf("total = %#v, want %d", got, coworkRegistryMaxServers)
	}
	if len(registryServerList(t, out)) != coworkRegistryMaxServers {
		t.Errorf("servers = %d, want %d", len(registryServerList(t, out)), coworkRegistryMaxServers)
	}
	if len(*calls) != 1 {
		t.Errorf("paging should stop at the cap, got %d calls", len(*calls))
	}
}

// TestCoworkMCPRegistry_FetchErrorIsNotCached pins upstream's 500 body and the
// fact that a failed fetch leaves nothing behind to serve.
func TestCoworkMCPRegistry_FetchErrorIsNotCached(t *testing.T) {
	calls := stubCoworkFetch(t, func(int, coworkFetchCall) (coworkHTTPResponse, error) {
		return coworkHTTPResponse{}, errors.New("registry offline")
	})

	repo, cleanup := setupTestDB(t)
	defer cleanup()
	h := NewDashboardHandler(repo)
	r := setupTestRouterWith(repo, h)

	code, out := getCoworkRegistry(t, r, "/api/cli-tools/cowork-mcp-registry")
	if code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", code)
	}
	if out["error"] != "registry offline" {
		t.Errorf("error = %#v, want registry offline", out["error"])
	}
	if got := out["total"]; got != float64(0) {
		t.Errorf("total = %#v, want 0", got)
	}
	if len(registryServerList(t, out)) != 0 {
		t.Errorf("servers must be empty on error")
	}

	if code, _ := getCoworkRegistry(t, r, "/api/cli-tools/cowork-mcp-registry"); code != http.StatusInternalServerError {
		t.Fatalf("second call = %d, want another 500", code)
	}
	if len(*calls) != 2 {
		t.Errorf("a failed fetch must not be cached, got %d calls", len(*calls))
	}
}

// successfulProbeAnswers is the fake MCP server: initialize, initialized
// notification, tools/list. It records both HTTP and protocol details.
func successfulProbeAnswers(t *testing.T) func(int, coworkFetchCall) (coworkHTTPResponse, error) {
	t.Helper()
	return func(call int, c coworkFetchCall) (coworkHTTPResponse, error) {
		if c.method != http.MethodPost {
			t.Errorf("call %d method = %s, want POST", call, c.method)
		}
		for _, header := range []string{"Content-Type", "Accept", "MCP-Protocol-Version"} {
			if c.headers[header] == "" {
				t.Errorf("call %d missing %s header", call, header)
			}
		}
		switch {
		case bytes.Contains(c.body, []byte(`"initialize"`)):
			return coworkHTTPResponse{
				Status: http.StatusOK,
				Header: http.Header{
					"Content-Type":   []string{"application/json"},
					"Mcp-Session-Id": []string{"sess-1"},
				},
				Body: []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`),
			}, nil
		case bytes.Contains(c.body, []byte(`notifications/initialized`)):
			if !strings.Contains(string(c.body), `"params":{}`) {
				t.Errorf("notification body = %s", c.body)
			}
			return coworkHTTPResponse{Status: http.StatusAccepted, Header: http.Header{}}, nil
		case bytes.Contains(c.body, []byte(`"tools/list"`)):
			return coworkJSONResponse(http.StatusOK, `{"jsonrpc":"2.0","id":2,"result":{"tools":[
				{"name":"search","description":"Search the web"},
				{"name":"noop"}
			]}}`), nil
		}
		t.Errorf("unexpected probe body: %s", c.body)
		return coworkHTTPResponse{Status: http.StatusBadRequest, Header: http.Header{}}, nil
	}
}

// TestCoworkMCPTools_ProbeMapsFakeMCPResponse pins the three-step handshake, the
// session-id propagation and the tool mapping.
func TestCoworkMCPTools_ProbeMapsFakeMCPResponse(t *testing.T) {
	calls := stubCoworkFetch(t, successfulProbeAnswers(t))

	repo, cleanup := setupTestDB(t)
	defer cleanup()
	r := setupTestRouterWith(repo, NewDashboardHandler(repo))

	code, probe, raw := postCoworkTools(t, r, `{"url":"https://mcp.example.com/rpc"}`, "")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %#v", code, raw)
	}
	if len(probe.Tools) != 2 {
		t.Fatalf("tools = %d, want 2: %#v", len(probe.Tools), raw)
	}
	if probe.Tools[0].Name != "search" || probe.Tools[0].Description != "Search the web" {
		t.Errorf("first tool = %#v", probe.Tools[0])
	}
	if probe.Tools[1].Name != "noop" || probe.Tools[1].Description != "" {
		t.Errorf("second tool = %#v (description must default to empty)", probe.Tools[1])
	}
	if probe.RequiresAuth || probe.Error != "" {
		t.Errorf("unexpected error/auth fields: %#v", raw)
	}
	if _, ok := raw["requiresAuth"]; ok {
		t.Errorf("requiresAuth must be omitted when false: %#v", raw)
	}

	if len(*calls) != 3 {
		t.Fatalf("expected initialize+notification+tools/list, got %d calls", len(*calls))
	}
	if !strings.Contains((*calls)[0].url, "https://mcp.example.com/rpc") {
		t.Errorf("probe url = %s", (*calls)[0].url)
	}
	for _, idx := range []int{1, 2} {
		if got := (*calls)[idx].headers["mcp-session-id"]; got != "sess-1" {
			t.Errorf("call %d mcp-session-id = %q, want sess-1", idx, got)
		}
	}
}

// TestCoworkMCPTools_ParsesSSE pins the event-stream branch: only the frame
// with id 2 is the tool list.
func TestCoworkMCPTools_ParsesSSE(t *testing.T) {
	stubCoworkFetch(t, func(call int, c coworkFetchCall) (coworkHTTPResponse, error) {
		switch call {
		case 0:
			return coworkHTTPResponse{Status: http.StatusOK, Header: http.Header{}, Body: []byte(`{}`)}, nil
		case 1:
			return coworkHTTPResponse{Status: http.StatusAccepted, Header: http.Header{}}, nil
		default:
			return coworkHTTPResponse{
				Status: http.StatusOK,
				Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: []byte("event: message\n" +
					"data: not-json\n\n" +
					`data: {"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"wrong"}]}}` + "\n\n" +
					`data: {"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"sse-tool","description":"From SSE"}]}}` + "\n\n"),
			}, nil
		}
	})

	repo, cleanup := setupTestDB(t)
	defer cleanup()
	r := setupTestRouterWith(repo, NewDashboardHandler(repo))

	code, probe, raw := postCoworkTools(t, r, `{"url":"https://mcp.example.com/sse"}`, "")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %#v", code, raw)
	}
	if len(probe.Tools) != 1 || probe.Tools[0].Name != "sse-tool" || probe.Tools[0].Description != "From SSE" {
		t.Errorf("SSE tools = %#v", probe.Tools)
	}
}

// TestCoworkMCPTools_RequiresAuthOnInit pins 401/403 as requiresAuth with no
// further calls, matching upstream's OAuth signal.
func TestCoworkMCPTools_RequiresAuthOnInit(t *testing.T) {
	calls := stubCoworkFetch(t, func(int, coworkFetchCall) (coworkHTTPResponse, error) {
		return coworkHTTPResponse{Status: http.StatusUnauthorized, Header: http.Header{}}, nil
	})

	repo, cleanup := setupTestDB(t)
	defer cleanup()
	r := setupTestRouterWith(repo, NewDashboardHandler(repo))

	code, probe, raw := postCoworkTools(t, r, `{"url":"https://oauth.example.com/mcp"}`, "")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %#v", code, raw)
	}
	if !probe.RequiresAuth {
		t.Errorf("requiresAuth = false, want true: %#v", raw)
	}
	if len(probe.Tools) != 0 {
		t.Errorf("tools = %#v, want []", probe.Tools)
	}
	if len(*calls) != 1 {
		t.Errorf("auth signal should stop after initialize, got %d calls", len(*calls))
	}
}

// TestCoworkMCPTools_RefusesUnreachableServer pins upstream's 200-with-error
// answer for a transport failure.
func TestCoworkMCPTools_RefusesUnreachableServer(t *testing.T) {
	stubCoworkFetch(t, func(int, coworkFetchCall) (coworkHTTPResponse, error) {
		return coworkHTTPResponse{}, errors.New("dial tcp 203.0.113.7:443: connect: connection refused")
	})

	repo, cleanup := setupTestDB(t)
	defer cleanup()
	r := setupTestRouterWith(repo, NewDashboardHandler(repo))

	code, probe, raw := postCoworkTools(t, r, `{"url":"https://unreachable.example.com/mcp"}`, "")
	if code != http.StatusOK {
		t.Fatalf("upstream reports transport failures as 200, got %d: %#v", code, raw)
	}
	if !strings.Contains(probe.Error, "connection refused") {
		t.Errorf("error = %q, want the transport message", probe.Error)
	}
	if len(probe.Tools) != 0 {
		t.Errorf("tools = %#v, want []", probe.Tools)
	}
}

// TestCoworkMCPTools_InitFailure pins upstream's `init <status>` error text.
func TestCoworkMCPTools_InitFailure(t *testing.T) {
	stubCoworkFetch(t, func(int, coworkFetchCall) (coworkHTTPResponse, error) {
		return coworkHTTPResponse{Status: http.StatusBadGateway, Header: http.Header{}}, nil
	})

	repo, cleanup := setupTestDB(t)
	defer cleanup()
	r := setupTestRouterWith(repo, NewDashboardHandler(repo))

	code, probe, raw := postCoworkTools(t, r, `{"url":"https://broken.example.com/mcp"}`, "")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %#v", code, raw)
	}
	if probe.Error != "init 502" {
		t.Errorf("error = %q, want init 502", probe.Error)
	}
	if len(probe.Tools) != 0 {
		t.Errorf("tools = %#v, want []", probe.Tools)
	}
}

// TestCoworkMCPTools_RefusesPrivateAddress is the SSRF gate: a remote caller
// naming a loopback target is refused with upstream's status and message, and
// nothing is dialed. A local caller keeps the self-hosted server exemption, but
// still cannot reach the cloud metadata service.
func TestCoworkMCPTools_RefusesPrivateAddress(t *testing.T) {
	const loopbackMCP = `{"url":"http://127.0.0.1:11434/mcp"}`

	t.Run("remote caller is refused", func(t *testing.T) {
		calls := stubCoworkFetch(t, successfulProbeAnswers(t))

		repo, cleanup := setupTestDB(t)
		defer cleanup()
		r := setupTestRouterWith(repo, NewDashboardHandler(repo))

		code, _, raw := postCoworkTools(t, r, loopbackMCP, "203.0.113.9:4321")
		if code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %#v", code, raw)
		}
		if raw["error"] != "URL not allowed" {
			t.Errorf("error = %#v, want URL not allowed", raw["error"])
		}
		if _, ok := raw["tools"]; ok {
			t.Errorf("upstream's refusal body has no tools key: %#v", raw)
		}
		if len(*calls) != 0 {
			t.Errorf("blocked URL was dialed %d times", len(*calls))
		}
	})

	t.Run("local caller keeps self-hosted server", func(t *testing.T) {
		stubCoworkFetch(t, successfulProbeAnswers(t))

		repo, cleanup := setupTestDB(t)
		defer cleanup()
		r := setupTestRouterWith(repo, NewDashboardHandler(repo))

		code, probe, raw := postCoworkTools(t, r, loopbackMCP, "127.0.0.1:5555")
		if code != http.StatusOK {
			t.Fatalf("local caller should probe, got %d: %#v", code, raw)
		}
		if len(probe.Tools) != 2 {
			t.Errorf("tools = %#v, want the local server's two tools", probe.Tools)
		}
	})

	t.Run("local caller still cannot reach cloud metadata", func(t *testing.T) {
		calls := stubCoworkFetch(t, successfulProbeAnswers(t))

		repo, cleanup := setupTestDB(t)
		defer cleanup()
		r := setupTestRouterWith(repo, NewDashboardHandler(repo))

		code, _, raw := postCoworkTools(t, r,
			`{"url":"http://169.254.169.254/latest/meta-data/"}`, "127.0.0.1:5555")
		if code != http.StatusBadRequest {
			t.Fatalf("metadata target should be refused, got %d: %#v", code, raw)
		}
		if raw["error"] != "URL not allowed" {
			t.Errorf("error = %#v, want URL not allowed", raw["error"])
		}
		if len(*calls) != 0 {
			t.Errorf("metadata target was dialed %d times", len(*calls))
		}
	})
}

// TestCoworkMCPTools_RequiresURL pins the 400 for a missing url.
func TestCoworkMCPTools_RequiresURL(t *testing.T) {
	calls := stubCoworkFetch(t, successfulProbeAnswers(t))

	repo, cleanup := setupTestDB(t)
	defer cleanup()
	r := setupTestRouterWith(repo, NewDashboardHandler(repo))

	code, _, raw := postCoworkTools(t, r, `{}`, "")
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %#v", code, raw)
	}
	if raw["error"] != "url required" {
		t.Errorf("error = %#v, want url required", raw["error"])
	}
	if len(*calls) != 0 {
		t.Errorf("missing url was dialed %d times", len(*calls))
	}
}
