// Package mcpbridge runs the preset stdio MCP servers and relays their
// newline-delimited JSON-RPC to HTTP SSE clients.
//
// It is the Go port of upstream's inline bridge (src/lib/mcp/stdioSseBridge.js
// plus src/app/api/mcp/[plugin]/{sse,message}/route.js): one child process per
// plugin, spawned on the first SSE subscriber, frames broadcast to every
// subscriber of that plugin, client messages written back to the child's stdin,
// and the child killed once its last subscriber disconnects.
//
// Only the presets below may spawn. Upstream is explicit that this is the RCE
// boundary — findPlugin() matches the hardcoded LOCAL_STDIO_PLUGINS table and a
// caller-supplied command is never executed — so the same rule holds here: the
// plugin id in the URL selects from this table and nothing else.
package mcpbridge

// Plugin is one preset local stdio MCP server.
type Plugin struct {
	// Name is the id that appears in /api/mcp/{plugin}/… and the key this
	// package stores the child under.
	Name string
	// Command and Args are the spawn line, fixed by the preset.
	Command string
	Args    []string
}

// localPlugins mirrors LOCAL_STDIO_PLUGINS in
// src/shared/constants/coworkPlugins.js. There is exactly one preset: Browser
// MCP drives a running Chrome through its extension.
var localPlugins = []Plugin{
	{
		Name:    "browsermcp",
		Command: "npx",
		Args:    []string{"-y", "@browsermcp/mcp@latest"},
	},
}

// FindPlugin returns the preset plugin with that name. The second result is
// false for anything else, and callers must answer upstream's "Unknown plugin"
// rather than spawning: a name that is not in the table is not a command.
func FindPlugin(name string) (Plugin, bool) {
	for _, p := range localPlugins {
		if p.Name == name {
			return p, true
		}
	}
	return Plugin{}, false
}
