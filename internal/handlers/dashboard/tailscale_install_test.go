package dashboard

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"panrouter/proxy/internal/tunnel"
)

type sseEvent struct {
	Event string
	Data  map[string]any
}

func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	events := []sseEvent{}
	for _, frame := range strings.Split(strings.TrimSpace(body), "\n\n") {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		var event sseEvent
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				payload := strings.TrimPrefix(line, "data: ")
				if err := json.Unmarshal([]byte(payload), &event.Data); err != nil {
					t.Fatalf("unmarshal %s payload: %v", event.Event, err)
				}
			}
		}
		events = append(events, event)
	}
	return events
}

// Port-of-upstream test for src/app/api/tunnel/tailscale-install/route.js.
func TestTailscaleInstall(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())

	repo, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)

	origInstall := tailscaleInstallFn
	origPlatform := tailscaleInstallPlatform
	origBrew := tailscaleInstallHasBrew
	t.Cleanup(func() {
		tailscaleInstallFn = origInstall
		tailscaleInstallPlatform = origPlatform
		tailscaleInstallHasBrew = origBrew
	})

	tests := []struct {
		name        string
		platform    string
		brew        bool
		body        string
		installAuth string
		installErr  error
		wantStatus  int
		wantJSON    string
		wantEvents  []string
		wantAuthURL string
		wantAuthSet bool
		wantError   string
	}{
		{
			name:       "linux without a sudo password is refused before streaming",
			platform:   "linux",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			wantJSON:   "Sudo password is required",
		},
		{
			name:        "linux with a password streams progress then the login URL",
			platform:    "linux",
			body:        `{"sudoPassword":"hunter2"}`,
			installAuth: "https://login.tailscale.com/a/abc123",
			wantStatus:  http.StatusOK,
			wantEvents:  []string{"progress", "done"},
			wantAuthURL: "https://login.tailscale.com/a/abc123",
			wantAuthSet: true,
		},
		{
			name:       "a rejected sudo password is named as such",
			platform:   "linux",
			body:       `{"sudoPassword":"wrong"}`,
			installErr: errors.New("sudo: incorrect password, try again"),
			wantStatus: http.StatusOK,
			wantEvents: []string{"progress", "error"},
			wantError:  "Wrong sudo password",
		},
		{
			name:        "windows needs no password and reports no login URL",
			platform:    "windows",
			body:        `{}`,
			installAuth: "",
			wantStatus:  http.StatusOK,
			wantEvents:  []string{"progress", "done"},
			wantAuthURL: "",
			wantAuthSet: true,
		},
		{
			name:        "homebrew macOS needs no password",
			platform:    "darwin",
			brew:        true,
			body:        `{}`,
			installAuth: "https://login.tailscale.com/a/brew1",
			wantStatus:  http.StatusOK,
			wantEvents:  []string{"progress", "done"},
			wantAuthURL: "https://login.tailscale.com/a/brew1",
			wantAuthSet: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tailscaleInstallPlatform = func() string { return tt.platform }
			tailscaleInstallHasBrew = func() bool { return tt.brew }
			called := 0
			tailscaleInstallFn = func(ctx context.Context, opts tunnel.TailscaleInstallOptions) (string, error) {
				called++
				if opts.Progress != nil {
					opts.Progress("installing...")
				}
				if opts.Hostname == "" {
					t.Error("hostname is empty, want the tunnel shortId")
				}
				return tt.installAuth, tt.installErr
			}

			h := NewDashboardHandler(repo)
			router := setupTestRouterWith(repo, h)
			req := httptest.NewRequest(http.MethodPost, "/api/tunnel/tailscale-install", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantJSON != "" {
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if body["error"] != tt.wantJSON {
					t.Errorf("error = %v, want %q", body["error"], tt.wantJSON)
				}
				if called != 0 {
					t.Errorf("installer ran %d times on a refused request, want 0", called)
				}
				return
			}

			if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
				t.Errorf("Content-Type = %q, want text/event-stream", ct)
			}
			events := parseSSE(t, rec.Body.String())
			var names []string
			for _, event := range events {
				names = append(names, event.Event)
			}
			if strings.Join(names, ",") != strings.Join(tt.wantEvents, ",") {
				t.Fatalf("events = %v, want %v (body %q)", names, tt.wantEvents, rec.Body.String())
			}
			if tt.wantAuthSet {
				done := events[len(events)-1]
				if done.Data["success"] != true {
					t.Errorf("done.success = %v, want true", done.Data["success"])
				}
				if tt.wantAuthURL == "" {
					if value, present := done.Data["authUrl"]; !present || value != nil {
						t.Errorf("done.authUrl = %v (present %v), want null", value, present)
					}
				} else if done.Data["authUrl"] != tt.wantAuthURL {
					t.Errorf("done.authUrl = %v, want %v", done.Data["authUrl"], tt.wantAuthURL)
				}
			}
			if tt.wantError != "" {
				if got := events[len(events)-1].Data["error"]; got != tt.wantError {
					t.Errorf("error event = %v, want %q", got, tt.wantError)
				}
			}
			if called != 1 {
				t.Errorf("installer ran %d times, want 1", called)
			}
		})
	}
}
