package dashboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	json "encoding/json/v2"

	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/proxy"
)

// Cowork's two network-backed CLI-tools routes.
//
// Mirrors:
//
//	src/app/api/cli-tools/cowork-mcp-registry/route.js  GET
//	src/app/api/cli-tools/cowork-mcp-tools/route.js     POST
//
// The registry is Anthropic's public MCP catalogue, filtered down to servers a
// third-party client can reach directly. The tools probe speaks just enough of
// the MCP streamable-HTTP transport (initialize, notifications/initialized,
// tools/list) to render a server's tool list on the cowork view.
//
// Both fetches leave the gateway, so both dial through proxy.NewSafeTransport —
// the socket-level guard the provider-node probes also rely on — and the tools
// route re-applies the request-level gate (handlerutil.AssertPublicURL) with
// upstream's loopback exemption for a self-hosted MCP server.

const (
	coworkRegistryURL        = "https://api.anthropic.com/mcp-registry/v0/servers"
	coworkRegistryVisibility = "commercial,gsuite,gsuite-google"
	// coworkRegistryTTL mirrors CACHE_TTL_MS in the upstream route.
	coworkRegistryTTL = time.Hour
	// coworkRegistryMaxPages mirrors upstream's loop (`i < 20`). Each page asks
	// for 500 servers, so the loop alone bounds a fetch at 10k entries; the
	// server cap below is the tighter bound.
	coworkRegistryMaxPages   = 20
	coworkRegistryPageLimit  = 500
	coworkRegistryMaxServers = 2000
	// coworkRegistryMaxCacheBytes is the size cap on the one cached entry. The
	// upstream cache is a single global slot, and this port keeps that shape:
	// one entry, dropped rather than cached when its serialized form is bigger
	// than this.
	coworkRegistryMaxCacheBytes = 8 << 20

	// coworkMCPTimeout is upstream's AbortSignal.timeout(8000), shared by all
	// three MCP requests.
	coworkMCPTimeout = 8 * time.Second
	// coworkMCPMaxBody bounds one response body. Upstream buffers without a
	// limit; a registry page or tool list this large is not a catalogue.
	coworkMCPMaxBody = 4 << 20
	// coworkMCPMaxRequest bounds the POST /api/cli-tools/cowork-mcp-tools body.
	coworkMCPMaxRequest = 1 << 20
)

// coworkHTTPResponse is the part of an HTTP response the two routes read.
// Package-level fetches return it so a test can inject a fake server without a
// listener — the same seam validateProbeDo uses.
type coworkHTTPResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// coworkMCPClient is the production dialer: the shared SSRF-safe transport, so
// a caller-supplied probe target (or a redirect it returns) cannot reach a
// private/reserved address even after the request-level gate has passed.
var coworkMCPClient = &http.Client{Transport: proxy.NewSafeTransport()}

// coworkMCPDo performs one outbound request. Production dials through the
// shared SSRF-safe transport; a test replaces it to count calls and answer with
// canned MCP/registry payloads.
var coworkMCPDo = func(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) (coworkHTTPResponse, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return coworkHTTPResponse{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := coworkMCPClient.Do(req)
	if err != nil {
		return coworkHTTPResponse{}, err
	}
	defer resp.Body.Close()

	// LimitReader+1 distinguishes "exactly at the cap" from "over it", so an
	// oversized page fails loudly instead of being cached truncated.
	data, err := io.ReadAll(io.LimitReader(resp.Body, coworkMCPMaxBody+1))
	if err != nil {
		return coworkHTTPResponse{}, err
	}
	if len(data) > coworkMCPMaxBody {
		return coworkHTTPResponse{Status: resp.StatusCode, Header: resp.Header}, fmt.Errorf("response body exceeds %d bytes", coworkMCPMaxBody)
	}
	return coworkHTTPResponse{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

// coworkRegistryCache is the single bounded slot upstream keeps on globalThis.
// The mutex also makes the fetch single-flight: a second caller inside the TTL
// waits for the first and then reads its entry instead of racing another fetch.
type coworkRegistryCache struct {
	mu   sync.Mutex
	ts   time.Time
	data *coworkRegistryData
}

// coworkRegistryData is the reusable half of the response.
type coworkRegistryData struct {
	Servers []coworkRegistryServer `json:"servers"`
	Total   int                    `json:"total"`
}

// coworkRegistryResponse is the wire shape: cached first, then the cached body.
type coworkRegistryResponse struct {
	Cached  bool                   `json:"cached"`
	Servers []coworkRegistryServer `json:"servers"`
	Total   int                    `json:"total"`
}

// coworkRegistryServer is one entry the cowork view can connect to.
type coworkRegistryServer struct {
	Name        string   `json:"name"`
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	URL         string   `json:"url"`
	Transport   string   `json:"transport"`
	OAuth       bool     `json:"oauth"`
	ToolNames   []string `json:"toolNames"`
	ToolCount   int      `json:"toolCount"`
	IconURL     *string  `json:"iconUrl"`
}

// coworkRegistryMeta is item._meta["com.anthropic.api/mcp-registry"].
type coworkRegistryMeta struct {
	Slug           string   `json:"slug"`
	DisplayName    string   `json:"displayName"`
	OneLiner       string   `json:"oneLiner"`
	RequiredFields []any    `json:"requiredFields"`
	IsAuthless     bool     `json:"isAuthless"`
	ToolNames      []string `json:"toolNames"`
	IconURL        string   `json:"iconUrl"`
}

type coworkRegistryItem struct {
	Server struct {
		Name        string `json:"name"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Remotes     []struct {
			URL  string `json:"url"`
			Type string `json:"type"`
		} `json:"remotes"`
	} `json:"server"`
	Meta map[string]coworkRegistryMeta `json:"_meta"`
}

type coworkRegistryPage struct {
	Servers  []coworkRegistryItem `json:"servers"`
	Metadata struct {
		NextCursor string `json:"nextCursor"`
	} `json:"metadata"`
}

// coworkMCPTool is one tools/list entry in the shape the dashboard renders.
type coworkMCPTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// coworkToolsListResult is the JSON-RPC result of tools/list.
type coworkToolsListResult struct {
	Tools []coworkMCPTool `json:"tools"`
}

// coworkJSONRPCResponse is the one JSON-RPC frame the probe reads.
type coworkJSONRPCResponse struct {
	ID     int                    `json:"id"`
	Result *coworkToolsListResult `json:"result"`
}

// coworkMCPProbeResult is upstream's result object. RequiresAuth and Error are
// omitted unless set, tools is always present (upstream returns [] on every
// path).
type coworkMCPProbeResult struct {
	RequiresAuth bool            `json:"requiresAuth,omitzero"`
	Error        string          `json:"error,omitzero"`
	Tools        []coworkMCPTool `json:"tools"`
}

// coworkClaudeMediatedRe and coworkAnthropicMCPRe mirror isDirectConnect's two
// rejections: claude.ai-mediated servers are broken in third-party mode, and
// api.anthropic.com/mcp entries are tenant-scoped.
var (
	coworkClaudeMediatedRe = regexp.MustCompile(`(?i)^https?://[^/]*\bmcp\.claude\.com\b`)
	coworkAnthropicMCPRe   = regexp.MustCompile(`(?i)^https?://api\.anthropic\.com/mcp`)
)

// coworkRegistryDirectConnect reports whether a remote URL is one this gateway
// can hand a desktop client, matching upstream's isDirectConnect.
func coworkRegistryDirectConnect(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, "<{") {
		return false
	}
	if coworkClaudeMediatedRe.MatchString(raw) || coworkAnthropicMCPRe.MatchString(raw) {
		return false
	}
	return strings.HasPrefix(strings.ToLower(raw), "https://")
}

// coworkNow is the TTL clock. A test freezes it so "inside the TTL" and
// "stale" are deterministic without sleeping.
func (h *DashboardHandler) coworkNow() time.Time {
	if h.coworkMCPNow != nil {
		return h.coworkMCPNow()
	}
	return time.Now()
}

// HandleCoworkMCPRegistry serves GET /api/cli-tools/cowork-mcp-registry.
//
// Contract from upstream: `?refresh=1` bypasses the cache; a fresh entry
// answers `{cached:true, servers, total}`; a fetched one answers
// `{cached:false, servers, total}`; a thrown fetch/parse answers 500 with
// `{error, servers:[], total:0}`. A non-2xx page is not an error — upstream
// stops paging and returns what it has, and so does this port.
func (h *DashboardHandler) HandleCoworkMCPRegistry(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("refresh") == "1"

	h.coworkRegistry.mu.Lock()
	defer h.coworkRegistry.mu.Unlock()

	if !force && h.coworkRegistry.data != nil {
		if age := h.coworkNow().Sub(h.coworkRegistry.ts); age < coworkRegistryTTL {
			handlerutil.WriteJSON(w, http.StatusOK, coworkRegistryResponse{
				Cached:  true,
				Servers: h.coworkRegistry.data.Servers,
				Total:   h.coworkRegistry.data.Total,
			})
			return
		}
	}

	servers, err := fetchCoworkRegistry(r.Context())
	if err != nil {
		handlerutil.WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   err.Error(),
			"servers": []coworkRegistryServer{},
			"total":   0,
		})
		return
	}
	data := &coworkRegistryData{Servers: servers, Total: len(servers)}

	// Size cap, not just a TTL: a catalogue that grew past the cap is answered
	// but not retained, so the one cache slot can never hold an unbounded body.
	if encoded, mErr := json.Marshal(data); mErr == nil && len(encoded) <= coworkRegistryMaxCacheBytes {
		h.coworkRegistry.data = data
		h.coworkRegistry.ts = h.coworkNow()
	}

	handlerutil.WriteJSON(w, http.StatusOK, coworkRegistryResponse{
		Cached:  false,
		Servers: data.Servers,
		Total:   data.Total,
	})
}

// fetchCoworkRegistry pages the public catalogue. The URL is a fixed public
// host, so the socket-level guard is the relevant one here; the tools route
// applies the request-level gate because its target is caller-supplied.
func fetchCoworkRegistry(ctx context.Context) ([]coworkRegistryServer, error) {
	out := make([]coworkRegistryServer, 0, coworkRegistryPageLimit)
	cursor := ""
	for page := 0; page < coworkRegistryMaxPages && len(out) < coworkRegistryMaxServers; page++ {
		pageURL := fmt.Sprintf("%s?limit=%d&visibility=%s", coworkRegistryURL, coworkRegistryPageLimit, coworkRegistryVisibility)
		if cursor != "" {
			pageURL += "&cursor=" + url.QueryEscape(cursor)
		}

		reqCtx, cancel := context.WithTimeout(ctx, coworkMCPTimeout)
		resp, err := coworkMCPDo(reqCtx, http.MethodGet, pageURL, map[string]string{
			"Accept": "application/json",
		}, nil)
		cancel()
		if err != nil {
			return nil, err
		}
		if resp.Status < 200 || resp.Status > 299 {
			// Upstream's `if (!r.ok) break;`: return the pages already read.
			break
		}

		var parsed coworkRegistryPage
		if err := json.Unmarshal(resp.Body, &parsed); err != nil {
			return nil, err
		}
		for _, item := range parsed.Servers {
			if len(out) >= coworkRegistryMaxServers {
				break
			}
			s := item.Server
			meta := item.Meta["com.anthropic.api/mcp-registry"]
			if len(meta.RequiredFields) > 0 {
				continue
			}
			remoteURL, remoteType := "", ""
			if len(s.Remotes) > 0 {
				remoteURL, remoteType = s.Remotes[0].URL, s.Remotes[0].Type
			}
			if !coworkRegistryDirectConnect(remoteURL) {
				continue
			}

			slug := meta.Slug
			if slug == "" {
				slug = s.Name
			}
			title := s.Title
			if title == "" {
				title = meta.DisplayName
			}
			if title == "" {
				title = s.Name
			}
			description := s.Description
			if description == "" {
				description = meta.OneLiner
			}
			transport := "http"
			if remoteType == "sse" {
				transport = "sse"
			}
			toolNames := meta.ToolNames
			if toolNames == nil {
				toolNames = []string{}
			}
			var icon *string
			if meta.IconURL != "" {
				iconValue := meta.IconURL
				icon = &iconValue
			}
			out = append(out, coworkRegistryServer{
				Name:        s.Name,
				Slug:        slug,
				Title:       title,
				Description: description,
				URL:         remoteURL,
				Transport:   transport,
				OAuth:       !meta.IsAuthless,
				ToolNames:   toolNames,
				ToolCount:   len(toolNames),
				IconURL:     icon,
			})
		}

		cursor = parsed.Metadata.NextCursor
		if cursor == "" {
			break
		}
	}

	// Dedupe by URL, keeping the first occurrence (upstream filters the same
	// way after paging).
	seen := make(map[string]struct{}, len(out))
	deduped := make([]coworkRegistryServer, 0, len(out))
	for _, s := range out {
		if _, ok := seen[s.URL]; ok {
			continue
		}
		seen[s.URL] = struct{}{}
		deduped = append(deduped, s)
	}
	return deduped, nil
}

// HandleCoworkMCPTools serves POST /api/cli-tools/cowork-mcp-tools.
//
// Contract from upstream: a missing/empty url is `400 {error:"url required"}`;
// a target the SSRF gate refuses is `400 {error:"URL not allowed"}`; every
// completed probe — including init failures and timeouts — is `200` with
// `{tools}` plus either `requiresAuth` or `error`.
func (h *DashboardHandler) HandleCoworkMCPTools(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, coworkMCPMaxRequest))
	if err != nil {
		handlerutil.WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
			"tools": []coworkMCPTool{},
		})
		return
	}
	defer r.Body.Close()

	var req struct {
		URL any `json:"url"`
	}
	// Upstream calls request.json(), which throws on an empty or malformed body
	// (caught as a 500), then only requires url to be a non-empty string.
	if err := json.Unmarshal(body, &req); err != nil {
		handlerutil.WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "invalid JSON body",
			"tools": []coworkMCPTool{},
		})
		return
	}
	target, _ := req.URL.(string)
	if target == "" {
		handlerutil.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "url required"})
		return
	}

	// SSRF gate. Cloud metadata is refused for every caller — the local-caller
	// exemption exists so a self-hosted MCP server on loopback stays reachable,
	// not so the dashboard can dial the instance credentials service. Remote
	// callers get the full public-URL check, which also resolves DNS.
	if handlerutil.IsCloudMetadataURL(r.Context(), target) {
		handlerutil.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "URL not allowed"})
		return
	}
	if !nodeRequestIsLocal(r) {
		if err := handlerutil.AssertPublicURL(target); err != nil {
			handlerutil.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "URL not allowed"})
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), coworkMCPTimeout)
	defer cancel()

	result := probeCoworkMCP(ctx, target)
	handlerutil.WriteJSON(w, http.StatusOK, result)
}

// probeCoworkMCP runs the three-step MCP handshake. No auth header is sent:
// authless servers answer, OAuth servers answer 401/403 which becomes
// requiresAuth so the dashboard can skip the tool list instead of showing an
// error.
func probeCoworkMCP(ctx context.Context, target string) coworkMCPProbeResult {
	baseHeaders := func() map[string]string {
		return map[string]string{
			"Content-Type":         "application/json",
			"Accept":               "application/json, text/event-stream",
			"MCP-Protocol-Version": "2025-06-18",
		}
	}
	emptyTools := func(errText string, requiresAuth bool) coworkMCPProbeResult {
		return coworkMCPProbeResult{
			RequiresAuth: requiresAuth,
			Error:        errText,
			Tools:        []coworkMCPTool{},
		}
	}

	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"9router","version":"1"}}}`)
	initRes, err := coworkMCPDo(ctx, http.MethodPost, target, baseHeaders(), initBody)
	if err != nil {
		return emptyTools(coworkMCPErrorText(err), false)
	}
	if initRes.Status == http.StatusUnauthorized || initRes.Status == http.StatusForbidden {
		return emptyTools("", true)
	}
	if initRes.Status < 200 || initRes.Status > 299 {
		return emptyTools(fmt.Sprintf("init %d", initRes.Status), false)
	}
	sessionID := ""
	if initRes.Header != nil {
		sessionID = initRes.Header.Get("mcp-session-id")
	}

	listHeaders := func() map[string]string {
		headers := baseHeaders()
		if sessionID != "" {
			headers["mcp-session-id"] = sessionID
		}
		return headers
	}

	// notifications/initialized is required by the spec before tools/list. Its
	// failure is not fatal — upstream ignores it too.
	_, _ = coworkMCPDo(ctx, http.MethodPost, target, listHeaders(),
		[]byte(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`))

	listRes, err := coworkMCPDo(ctx, http.MethodPost, target, listHeaders(),
		[]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if err != nil {
		return emptyTools(coworkMCPErrorText(err), false)
	}
	if listRes.Status == http.StatusUnauthorized || listRes.Status == http.StatusForbidden {
		return emptyTools("", true)
	}

	var parsed *coworkJSONRPCResponse
	contentType := ""
	if listRes.Header != nil {
		contentType = listRes.Header.Get("content-type")
	}
	if strings.Contains(contentType, "text/event-stream") {
		// Each `data:` line is one JSON-RPC message; the tools answer is the
		// frame with id 2. Malformed frames are skipped, as upstream does.
		for _, line := range strings.Split(string(listRes.Body), "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var msg coworkJSONRPCResponse
			if json.Unmarshal([]byte(payload), &msg) != nil {
				continue
			}
			if msg.ID == 2 && msg.Result != nil {
				parsed = &msg
				break
			}
		}
	} else if len(listRes.Body) > 0 {
		var msg coworkJSONRPCResponse
		if json.Unmarshal(listRes.Body, &msg) == nil {
			parsed = &msg
		}
	}
	if parsed == nil || parsed.Result == nil {
		return coworkMCPProbeResult{Tools: []coworkMCPTool{}}
	}
	tools := parsed.Result.Tools
	if tools == nil {
		tools = []coworkMCPTool{}
	}
	return coworkMCPProbeResult{Tools: tools}
}

// coworkMCPErrorText maps a transport failure to upstream's message: an aborted
// request reads "timeout", anything else reads as the error itself.
func coworkMCPErrorText(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	return err.Error()
}
