package dashboard

import (
	"bufio"
	"context"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"panrouter/proxy/internal/auth"
	"panrouter/proxy/internal/mcpbridge"
)

// stubEnv marks the re-executed test binary as the stub MCP child.
const stubEnv = "PANROUTER_MCP_STUB"

// TestMCPStubProcess is not a test: it is the child the bridge spawns in these
// tests. Every stdin line is echoed back as a JSON-RPC result, so a test can
// observe a message reaching the child as well as a frame coming back.
func TestMCPStubProcess(t *testing.T) {
	if os.Getenv(stubEnv) != "1" {
		t.Skip("stub child for the MCP route tests")
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		echo, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "echo: " + scanner.Text()}},
			},
		})
		if err != nil {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "%s\n", echo)
	}
	os.Exit(0)
}

// stubBridge spawns the stub above instead of npx.
func stubBridge(t *testing.T) *mcpbridge.Bridge {
	t.Helper()
	b := mcpbridge.NewWithSpawn(func(mcpbridge.Plugin) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=TestMCPStubProcess")
		cmd.Env = append(os.Environ(), stubEnv+"=1")
		return cmd, nil
	})
	t.Cleanup(b.KillAll)
	return b
}

// routeRequest builds what a chi router hands the handler: the {plugin} path
// value, and by default a peer on this machine (upstream's local-only rule).
func routeRequest(method, plugin, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.SetPathValue("plugin", plugin)
	req.RemoteAddr = "127.0.0.1:51234"
	return req
}

func errorFromBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, rec.Body.String())
	}
	return errorMessage(t, out)
}

// upstream: src/app/api/mcp/[plugin]/message/route.js — findPlugin comes before
// anything else, and an unknown name answers {"error":"Unknown plugin: …"} 404.
func TestHandleMCPMessage_UnknownPluginIs404(t *testing.T) {
	h := &DashboardHandler{MCPBridge: stubBridge(t)}
	req := routeRequest(http.MethodPost, "not-a-plugin", "/api/mcp/not-a-plugin/message",
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`)

	rec := httptest.NewRecorder()
	h.HandleMCPMessage(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := errorFromBody(t, rec); got != "Unknown plugin: not-a-plugin" {
		t.Errorf("error = %q, want upstream's %q", got, "Unknown plugin: not-a-plugin")
	}
}

// Upstream ignores sessionId entirely: a POST is accepted as long as the
// plugin's child is running, and refused with "Bridge not running: <name>"
// when it is not. An unknown session is therefore a bridge-state question, not
// a session lookup — this pins the shape of that refusal.
func TestHandleMCPMessage_UnknownSessionIsBridgeNotRunning(t *testing.T) {
	h := &DashboardHandler{MCPBridge: stubBridge(t)}
	req := routeRequest(http.MethodPost, "browsermcp",
		"/api/mcp/browsermcp/message?sessionId=not-a-session",
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`)

	rec := httptest.NewRecorder()
	h.HandleMCPMessage(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := errorFromBody(t, rec); got != "Bridge not running: browsermcp" {
		t.Errorf("error = %q, want upstream's %q", got, "Bridge not running: browsermcp")
	}
}

func TestHandleMCPMessage_DeliversMessageToChild(t *testing.T) {
	bridge := stubBridge(t)
	session, err := bridge.RegisterSession("browsermcp")
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	defer bridge.UnregisterSession("browsermcp", session.ID())

	h := &DashboardHandler{MCPBridge: bridge}
	body := `{"jsonrpc":"2.0","id":42,"method":"tools/list"}`
	req := routeRequest(http.MethodPost, "browsermcp",
		"/api/mcp/browsermcp/message?sessionId="+session.ID(), body)

	rec := httptest.NewRecorder()
	h.HandleMCPMessage(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (upstream answers an empty 202)", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("202 carries a body %q; upstream sends none", rec.Body.String())
	}

	select {
	case frame := <-session.Frames():
		if !strings.Contains(frame, "event: message") || !strings.Contains(frame, "tools/list") {
			t.Errorf("the child did not answer the posted message: %q", frame)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no frame came back from the child")
	}
}

func TestHandleMCPMessage_InvalidJSONIs500(t *testing.T) {
	h := &DashboardHandler{MCPBridge: stubBridge(t)}
	req := routeRequest(http.MethodPost, "browsermcp", "/api/mcp/browsermcp/message", `{"jsonrpc":`)

	rec := httptest.NewRecorder()
	h.HandleMCPMessage(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := errorFromBody(t, rec); got == "" {
		t.Error("500 carries no error message")
	}
}

// Upstream LOCAL_ONLY_PATHS ("/api/mcp/"): a caller that is not on this machine
// and holds no CLI token is refused before any child is spawned.
func TestHandleMCPRoutes_RemoteCallerIsForbidden(t *testing.T) {
	bridge := stubBridge(t)
	h := &DashboardHandler{MCPBridge: bridge}

	tests := []struct {
		name   string
		method string
		plugin string
		target string
		body   string
	}{
		{name: "message", method: http.MethodPost, plugin: "browsermcp", target: "/api/mcp/browsermcp/message", body: `{"jsonrpc":"2.0","id":1}`},
		{name: "sse", method: http.MethodGet, plugin: "browsermcp", target: "/api/mcp/browsermcp/sse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := routeRequest(tt.method, tt.plugin, tt.target, tt.body)
			req.RemoteAddr = "203.0.113.9:40000"
			rec := httptest.NewRecorder()

			if tt.method == http.MethodGet {
				h.HandleMCPSSE(rec, req)
			} else {
				h.HandleMCPMessage(rec, req)
			}

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if got := errorFromBody(t, rec); got != mcpLocalOnlyMessage {
				t.Errorf("error = %q, want %q", got, mcpLocalOnlyMessage)
			}
		})
	}
}

func TestHandleMCPMessage_CLITokenAdmitsRemoteCaller(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())

	h := &DashboardHandler{MCPBridge: stubBridge(t)}
	// An unknown plugin keeps the request off the spawn path; reaching the 404
	// proves the local-only gate let it through.
	req := routeRequest(http.MethodPost, "not-a-plugin", "/api/mcp/not-a-plugin/message",
		`{"jsonrpc":"2.0","id":1}`)
	req.RemoteAddr = "203.0.113.9:40000"
	req.Header.Set(auth.CLITokenHeader, auth.CLIToken())

	rec := httptest.NewRecorder()
	h.HandleMCPMessage(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 from the unknown-plugin check", rec.Code)
	}
}

var mcpSessionIDPattern = regexp.MustCompile(
	`^event: endpoint\ndata: /api/mcp/browsermcp/message\?sessionId=[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\n\n$`)

// flushCancelRecorder ends the request as soon as the handler flushes, so a
// stream that blocks after its handshake returns without any sleep.
type flushCancelRecorder struct {
	*httptest.ResponseRecorder
	cancel  context.CancelFunc
	flushed bool
}

func (w *flushCancelRecorder) Flush() {
	if !w.flushed {
		w.flushed = true
		w.cancel()
	}
}

func TestHandleMCPSSE_WritesHandshakeBeforeBlocking(t *testing.T) {
	bridge := stubBridge(t)
	h := &DashboardHandler{MCPBridge: bridge}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := routeRequest(http.MethodGet, "browsermcp", "/api/mcp/browsermcp/sse", "").WithContext(ctx)

	rec := &flushCancelRecorder{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	h.HandleMCPSSE(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	headers := map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache, no-transform",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	}
	for header, want := range headers {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if !rec.flushed {
		t.Fatal("the handshake was written but never flushed; the client would wait for it")
	}
	got := rec.Body.String()
	if !mcpSessionIDPattern.MatchString(got) {
		t.Fatalf("handshake = %q, want an endpoint event with a session id", got)
	}
}

func TestHandleMCPSSE_UnknownPluginIsPlainText404(t *testing.T) {
	h := &DashboardHandler{MCPBridge: stubBridge(t)}
	req := routeRequest(http.MethodGet, "not-a-plugin", "/api/mcp/not-a-plugin/sse", "")

	rec := httptest.NewRecorder()
	h.HandleMCPSSE(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Body.String(); got != "Unknown plugin: not-a-plugin" {
		t.Errorf("body = %q, want upstream's plain-text message", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
}
