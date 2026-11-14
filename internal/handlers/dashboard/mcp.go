package dashboard

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"io"
	"net/http"

	"panrouter/proxy/internal/auth"
	"panrouter/proxy/internal/constants"
	"panrouter/proxy/internal/mcpbridge"
	"panrouter/proxy/internal/shutdown"
)

// MCP stdio bridge over HTTP: GET /api/mcp/{plugin}/sse and
// POST /api/mcp/{plugin}/message.
//
// Upstream (src/app/api/mcp/[plugin]/{sse,message}/route.js) is the SSE
// transport of the MCP spec: the SSE stream hands the client an endpoint to
// POST JSON-RPC messages to, and every line the child writes to stdout comes
// back as an `event: message` frame. internal/mcpbridge owns the children; this
// file owns the HTTP contract.

// maxMCPMessageBytes caps one POSTed JSON-RPC message. A tool call with an
// inline image is the largest thing a client sends, and the child parses the
// line as one JSON value; a bigger body is a client bug or an attempt to make
// the gateway buffer without bound.
const maxMCPMessageBytes = 8 << 20

// mcpLocalOnlyMessage is upstream's LOCAL_ONLY_PATHS refusal body
// (src/dashboardGuard.js: canAccessLocalOnlyRoute).
const mcpLocalOnlyMessage = "Local only: CLI token required"

// mcpBridge returns the bridge this handler registered, or the process-wide one.
func (h *DashboardHandler) mcpBridge() *mcpbridge.Bridge {
	if h.MCPBridge != nil {
		return h.MCPBridge
	}
	return mcpbridge.Default()
}

// mcpAccessAllowed applies upstream's local-only rule for /api/mcp/: a valid
// local CLI token, or a peer on this machine. The dashboard auth gate has
// already run (these routes sit in its group), so this is the second rule —
// the one that keeps a tunnelled or LAN request from spawning a child process
// on the operator's desktop.
func mcpAccessAllowed(r *http.Request) bool {
	if auth.ValidCLIToken(r.Header.Get(auth.CLITokenHeader)) {
		return true
	}
	return nodeRequestIsLocal(r)
}

// HandleMCPSSE serves GET /api/mcp/{plugin}/sse.
//
// An unknown plugin is upstream's plain-text 404, not JSON: the SSE route
// answers `Unknown plugin: <name>` with a text body.
func (h *DashboardHandler) HandleMCPSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeDashboardError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	if !mcpAccessAllowed(r) {
		writeDashboardError(w, http.StatusForbidden, mcpLocalOnlyMessage)
		return
	}

	plugin := getURLParam(r, "plugin")
	bridge := h.mcpBridge()
	session, err := bridge.RegisterSession(plugin)
	if err != nil {
		if errors.Is(err, mcpbridge.ErrUnknownPlugin) {
			w.Header().Set(constants.HeaderContentType, "text/plain;charset=UTF-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("Unknown plugin: " + plugin))
			return
		}
		writeDashboardError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The child is killed once its last subscriber leaves, so a closed tab must
	// always reach this.
	defer bridge.UnregisterSession(plugin, session.ID())

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// MCP SSE handshake: tell the client where to POST messages.
	if _, err := w.Write([]byte("event: endpoint\ndata: /api/mcp/" + plugin +
		"/message?sessionId=" + session.ID() + "\n\n")); err != nil {
		return
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-shutdown.Done():
			return
		case frame := <-session.Frames():
			if _, err := w.Write([]byte(frame)); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// HandleMCPMessage serves POST /api/mcp/{plugin}/message.
//
// Contract, in upstream's order: the local-only gate (middleware, so it runs
// before the route), the unknown-plugin 404, JSON parsing (an unparseable body
// is 500, since upstream's request.json() throws), the stdin write, then 202
// with an empty body. Delivery is fire-and-forget: the child's answer arrives
// on the SSE stream, which is why the POST cannot report it.
func (h *DashboardHandler) HandleMCPMessage(w http.ResponseWriter, r *http.Request) {
	if !mcpAccessAllowed(r) {
		writeDashboardError(w, http.StatusForbidden, mcpLocalOnlyMessage)
		return
	}

	plugin := getURLParam(r, "plugin")
	if _, ok := mcpbridge.FindPlugin(plugin); !ok {
		writeDashboardError(w, http.StatusNotFound, "Unknown plugin: "+plugin)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxMCPMessageBytes+1))
	if err != nil {
		writeDashboardError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(body) > maxMCPMessageBytes {
		writeDashboardError(w, http.StatusInternalServerError, "message too large")
		return
	}
	// Upstream parses the body with request.json() and reports the parse error
	// as a 500. Go's parser rejects the same bodies, so its reason is reported
	// rather than a generic string.
	var raw jsontext.Value
	if err := json.Unmarshal(body, &raw); err != nil {
		writeDashboardError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := h.mcpBridge().Send(plugin, body); err != nil {
		if errors.Is(err, mcpbridge.ErrNotRunning) {
			writeDashboardError(w, http.StatusInternalServerError, "Bridge not running: "+plugin)
			return
		}
		writeDashboardError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
