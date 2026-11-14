// Production wiring for the CodeBuddy Intl daily auto-ping: the store reads and
// writes the connection rows, and the sender is one ping through the ordinary
// chat lane.
//
// This is the Go side of upstream's
// `getProviderConnections` / `updateProviderConnection` /
// `refreshAndUpdateCredentials` / `executor.execute` chain.
package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/handlers/chat"
	"panrouter/proxy/internal/models"
	"panrouter/proxy/internal/proxy"
)

// CodeBuddySettingsFromRepo reads the settings row and returns the
// `codebuddyIntlAutoPing` configuration.
//
// It is a function this package re-reads on every tick, so a model override
// saved in the dashboard applies without a restart. A settings read that fails
// yields the disabled zero value: an unreadable row must not make the scheduler
// ping accounts whose switch the operator may have turned off.
func CodeBuddySettingsFromRepo(repo *db.Repo) func() CodeBuddySettings {
	return func() CodeBuddySettings {
		if repo == nil {
			return CodeBuddySettings{Connections: map[string]bool{}}
		}
		raw, err := repo.GetSettingsRaw()
		if err != nil {
			return CodeBuddySettings{Connections: map[string]bool{}}
		}
		return CodeBuddySettingsFromBlob(raw)
	}
}

// CodeBuddyStoreFromRepo reads and writes the connection rows the daily cycle
// runs on.
func CodeBuddyStoreFromRepo(repo *db.Repo) CodeBuddyStore {
	return &codeBuddyRepoStore{repo: repo}
}

type codeBuddyRepoStore struct {
	repo *db.Repo
}

// Connections lists the active CodeBuddy Intl connections. The rows carry the
// last-attempt record, which is what makes "already pinged this cycle" survive
// a restart.
func (s *codeBuddyRepoStore) Connections() ([]CodeBuddyConnection, error) {
	if s == nil || s.repo == nil {
		return nil, nil
	}
	rows, err := s.repo.GetProviderConnections(CodeBuddyProvider, true)
	if err != nil {
		return nil, fmt.Errorf("list %s connections: %w", CodeBuddyProvider, err)
	}
	out := make([]CodeBuddyConnection, 0, len(rows))
	for _, row := range rows {
		out = append(out, codeBuddyConnectionFromRow(row))
	}
	return out, nil
}

// RecordAttempt writes the ping onto the connection row.
//
// The row is read first and the three values are merged into its data blob, as
// upstream merges them into the row object: a partial payload must not drop the
// credentials, the proxy pool or anything else the connection carries. Only the
// data column is written (UpdateConnectionData), so a reorder or a switch the
// operator made in the dashboard is not rolled back by a daily ping.
func (s *codeBuddyRepoStore) RecordAttempt(connectionID, dayKey string, at time.Time, model string) error {
	if s == nil || s.repo == nil {
		return nil
	}
	conn, err := s.repo.GetProviderConnectionByID(connectionID)
	if err != nil {
		return fmt.Errorf("read %s for the auto-ping record: %w", connectionID, err)
	}
	if conn == nil {
		return nil
	}
	raw := map[string]any{}
	if strings.TrimSpace(conn.Data) != "" {
		if err := json.Unmarshal([]byte(conn.Data), &raw); err != nil {
			// Refused rather than overwritten: writing a fresh object here
			// would erase the credential of a row this could not read.
			return fmt.Errorf("parse %s data for the auto-ping record: %w", connectionID, err)
		}
	}
	raw["lastAutoPingDayKey"] = dayKey
	raw["lastAutoPingAt"] = at.UTC().Format(time.RFC3339)
	raw["lastAutoPingModel"] = model
	encoded, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("encode %s auto-ping record: %w", connectionID, err)
	}
	return s.repo.UpdateConnectionData(connectionID, string(encoded))
}

// codeBuddyConnectionFromRow reads the fields the cycle decides on out of a row
// and its data blob.
func codeBuddyConnectionFromRow(row *models.ProviderConnection) CodeBuddyConnection {
	conn := CodeBuddyConnection{ID: row.ID}
	switch {
	case row.Name != nil && strings.TrimSpace(*row.Name) != "":
		conn.DisplayName = *row.Name
	case row.Email != nil && strings.TrimSpace(*row.Email) != "":
		conn.DisplayName = *row.Email
	}
	conn.Data = row.Data

	raw := map[string]any{}
	if strings.TrimSpace(row.Data) != "" {
		if err := json.Unmarshal([]byte(row.Data), &raw); err != nil {
			// A blob this cannot parse carries no attempt record; the row is
			// still pingable, and the write path refuses to clobber it.
			return conn
		}
	}
	if dayKey, ok := raw["lastAutoPingDayKey"].(string); ok {
		conn.LastAutoPingDayKey = dayKey
	}
	if at, ok := raw["lastAutoPingAt"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339, at); err == nil {
			conn.LastAutoPingAt = parsed
		}
	}
	return conn
}

// CodeBuddySendFromChat sends one ping to one connection on one model.
//
// The request rides the ordinary chat lane with the connection pinned, so it
// uses the same credential resolution, OAuth refresh, proxy-pool binding and
// executor as real traffic; it is marked as a probe by the lane, which keeps it
// out of the usage ledger (upstream makes the same distinction).
//
// Before the request the credential is refreshed, which is upstream's
// explicit `refreshAndUpdateCredentials` call. The refresh is non-fatal
// (upstream swallows the same error and pings with the token it has): the lane
// re-reads the credential from the row, so a refresh that lands mid-flight is
// still the one used.
func CodeBuddySendFromChat(chatH *chat.ChatHandler) CodeBuddySender {
	return func(ctx context.Context, conn CodeBuddyConnection, model string) error {
		if chatH == nil {
			return fmt.Errorf("codebuddy auto-ping: no chat handler")
		}
		refreshCodeBuddyCredential(chatH, conn)
		body, err := CodeBuddyPingBody(model)
		if err != nil {
			return err
		}
		if err := chatH.PingConnection(ctx, CodeBuddyProvider, model, conn.ID, body); err != nil {
			return codeBuddyPingFailure(err)
		}
		return nil
	}
}

// refreshCodeBuddyCredential refreshes an expired OAuth token before the ping.
// It is best-effort by design: a refresh that fails leaves the connection
// usable with the token it already has, and the ping reports the real answer.
func refreshCodeBuddyCredential(chatH *chat.ChatHandler, conn CodeBuddyConnection) {
	// The refreshed token is persisted on the row, which is where the lane
	// reads the credential from, so the return value is not needed here.
	_, _, _ = chatH.RefreshOAuthTokenIfExpired(conn.ID, codeBuddyAccessToken(conn.Data))
}

// codeBuddyAccessToken reads the credential a CodeBuddy Intl request carries.
// The provider authenticates with the OAuth access token; `apiKey` is the
// mirrored copy the CLI login writes, so it is the fallback.
func codeBuddyAccessToken(data string) string {
	if strings.TrimSpace(data) == "" {
		return ""
	}
	raw := map[string]any{}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return ""
	}
	if token, ok := raw["accessToken"].(string); ok && token != "" {
		return token
	}
	token, _ := raw["apiKey"].(string)
	return token
}

// codeBuddyPingFailure narrows a lane error to the facts the model walk
// classifies. The lane reports an upstream refusal as *proxy.UpstreamError,
// which carries both the status and the body; anything else keeps the lane's own
// error, so an unclassified failure ends the walk rather than trying another
// model on a hope.
func codeBuddyPingFailure(err error) error {
	var upstreamErr *proxy.UpstreamError
	if errors.As(err, &upstreamErr) {
		return &CodeBuddyPingError{Status: upstreamErr.StatusCode, Text: string(upstreamErr.Body)}
	}
	return err
}

// CodeBuddyPingBody builds the body a CodeBuddy ping sends.
//
// max_tokens is upstream's 16 (the CodeBuddy GPT-family endpoints reject a
// smaller one), the prompt the shared one-word ping text, and `stream: false`
// is safe: the CodeBuddy executor forces stream=true upstream, because the
// provider rejects a non-stream request with 11101, and re-aggregates the SSE
// for the client.
func CodeBuddyPingBody(model string) ([]byte, error) {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": CodeBuddyPingMaxTokens,
		"stream":     false,
		"messages": []map[string]any{
			{"role": "user", "content": PingText},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build codebuddy ping body: %w", err)
	}
	return body, nil
}
