package dashboard

import (
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"

	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/tunnel"
)

// Port of upstream src/app/api/tunnel/tailscale-install/route.js.
//
// The install is slow (a package download, an elevation prompt, a login round
// trip), so the route answers an SSE stream: one `progress` event per line the
// installer prints, then exactly one terminal `done` or `error`. The dashboard
// renders those lines live instead of leaving the operator on a spinner with no
// idea whether anything is happening.
//
// Upstream's pre-flight is kept: on macOS without Homebrew and on Linux the
// install needs sudo, so a request that carries no password is refused before a
// stream is opened (and refused as plain JSON — an event stream that only ever
// carries an error is harder for the caller to branch on).

var (
	// tailscaleInstallFn is the installer seam. Tests replace it so no case
	// downloads a package or spawns sudo.
	tailscaleInstallFn = tunnel.InstallTailscale
	// tailscaleInstallPlatform and tailscaleInstallHasBrew decide whether a
	// sudo password is required. Pinned in tests so the assertion does not
	// depend on the machine running them.
	tailscaleInstallPlatform = func() string { return runtime.GOOS }
	tailscaleInstallHasBrew  = hasBrew
)

type tailscaleInstallRequest struct {
	SudoPassword string `json:"sudoPassword"`
}

// HandleTailscaleInstall handles POST /api/tunnel/tailscale-install.
func (h *DashboardHandler) HandleTailscaleInstall(w http.ResponseWriter, r *http.Request) {
	var req tailscaleInstallRequest
	if r.Body != nil {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<10))
		_ = json.Unmarshal(body, &req)
	}
	sudoPassword := strings.TrimSpace(req.SudoPassword)

	platform := tailscaleInstallPlatform()
	needsPassword := platform != "windows" && !(platform == "darwin" && tailscaleInstallHasBrew())
	if needsPassword && sudoPassword == "" {
		handlerutil.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error": "Sudo password is required",
		})
		return
	}

	shortID := ""
	if st := tunnel.LoadState(); st != nil {
		shortID = st.ShortID
	}
	if shortID == "" {
		shortID = tunnel.GenerateShortID()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	ctx := r.Context()
	send := func(event string, data any) bool {
		if ctx.Err() != nil {
			return false
		}
		payload, err := json.Marshal(data)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	authURL, err := tailscaleInstallFn(ctx, tunnel.TailscaleInstallOptions{
		SudoPassword: sudoPassword,
		Hostname:     shortID,
		Progress: func(message string) {
			send("progress", map[string]any{"message": message})
		},
	})
	if err != nil {
		message := err.Error()
		// sudo's own wording, reported the way the dashboard explains it.
		if strings.Contains(message, "incorrect password") || strings.Contains(message, "Sorry") {
			message = "Wrong sudo password"
		}
		send("error", map[string]any{"error": message})
		return
	}

	var authValue any
	if authURL != "" {
		authValue = authURL
	}
	send("done", map[string]any{
		"success": true,
		"authUrl": authValue,
	})
}
