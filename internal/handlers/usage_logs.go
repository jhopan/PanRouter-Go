package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/log"
)

// usageLogLimit is how many request-log lines the view renders. Upstream's
// getRecentLogs is called with 200 by both aliases.
const usageLogLimit = 200

// logTimeLayout is the rendered timestamp: "02-01-2006 15:04:05", i.e.
// DD-MM-YYYY HH:mm:ss in the server's local zone, exactly as upstream's
// formatLogDate builds it from the Date object.
const logTimeLayout = "02-01-2006 15:04:05"

// HandleUsageLogs serves GET /api/usage/logs and GET /api/usage/request-logs.
//
// Both routes are the same feed: a JSON array of preformatted lines, newest
// first, one per usage row —
//
//	DD-MM-YYYY HH:mm:ss | <model> | <PROVIDER> | <account> | <sent> | <received> | <status>
//
// The dashboard prints them into a terminal pane without parsing, which is why
// this is text and not objects: upstream's getRecentLogs formats in the data
// layer and the route returns the array untouched.
//
// A read failure answers the empty array rather than a 500, matching upstream,
// whose getRecentLogs catches its own error and returns []. The view is a
// diagnostics feed; refusing it wholesale over one unreadable row is worse than
// showing nothing, and the failure is still logged server-side.
func HandleUsageLogs(repo *db.Repo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lines := make([]string, 0, usageLogLimit)
		rows, err := repo.GetRecentUsageLogs(usageLogLimit)
		if err != nil {
			log.Warn("usage", "recent request logs failed", "error", err)
			handlerutil.WriteJSON(w, http.StatusOK, lines)
			return
		}

		accounts := connectionDisplayNames(repo)
		for _, row := range rows {
			lines = append(lines, formatUsageLogLine(row, accounts))
		}
		handlerutil.WriteJSON(w, http.StatusOK, lines)
	}
}

// connectionDisplayNames maps a connection id to the name the log column shows.
// A lookup failure costs the friendly name, not the line: the id prefix is
// still enough to tell two accounts apart.
func connectionDisplayNames(repo *db.Repo) map[string]string {
	names := map[string]string{}
	conns, err := repo.GetProviderConnections("", false)
	if err != nil {
		return names
	}
	for _, c := range conns {
		name := ""
		if c.Name != nil {
			name = *c.Name
		}
		if name == "" && c.Email != nil {
			name = *c.Email
		}
		names[c.ID] = name
	}
	return names
}

// formatUsageLogLine renders one row in upstream's pipe-separated shape. Every
// field has a placeholder, so a column the row does not carry does not shift
// the rest of the line.
func formatUsageLogLine(row db.UsageHistoryRow, accounts map[string]string) string {
	account := accounts[row.ConnectionID]
	if account == "" {
		account = row.ConnectionID
		if len(account) > 8 {
			account = account[:8]
		}
	}
	if account == "" {
		account = "-"
	}

	model := row.Model
	if model == "" {
		model = "-"
	}
	provider := strings.ToUpper(row.Provider)
	if provider == "" {
		provider = "-"
	}
	status := row.Status
	if status == "" {
		status = "-"
	}

	parts := []string{
		formatLogTimestamp(row.Timestamp),
		model,
		provider,
		account,
		strconv.Itoa(row.PromptTokens),
		strconv.Itoa(row.CompletionTokens),
		status,
	}
	return strings.Join(parts, " | ")
}

// formatLogTimestamp renders a stored RFC3339 timestamp in local time. A value
// that does not parse is shown as stored: a malformed row is still worth a line.
func formatLogTimestamp(raw string) string {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	return t.Local().Format(logTimeLayout)
}
