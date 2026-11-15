package executor

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"panrouter/proxy/internal/log"
)

// devinMCPToolPrefix is the prefix the CLI requires on a discovered MCP tool
// name (devin-cli.js:120). A call comes back with it, and the client must see
// its own tool name, so the prefix is added on the way out and stripped on the
// way back.
const devinMCPToolPrefix = "mcp_"

// devinCLIPermissionModeBypass is the default DEVIN_PERMISSION_MODE.
//
// The default agent blocks on a permission request for the first shell or file
// tool call, and this transport never answers one unless it is asked to — the
// stream would simply stop there. Upstream makes the same choice and warns that
// it lets the agent run unattended (devin-cli.js:452-455).
const devinCLIPermissionModeBypass = "bypass"

// devinCLIToMCPName adds the discovery prefix the CLI requires, and is
// idempotent so a tool the caller already named with it is not doubled.
func devinCLIToMCPName(name string) string {
	if strings.HasPrefix(name, devinMCPToolPrefix) {
		return name
	}
	return devinMCPToolPrefix + name
}

// devinCLIFromMCPName strips the discovery prefix.
func devinCLIFromMCPName(name string) string {
	return strings.TrimPrefix(name, devinMCPToolPrefix)
}

// devinCLIToolResults maps every tool result the client already produced to the
// MCP tool name the bridge will seed it under.
//
// The bridge answers a re-call with the recorded result instead of the
// "(awaiting client tool_result)" placeholder, which is what lets a client that
// round-trips a tool call continue the same task: the next request replays the
// whole history, the MCP server is seeded with what those calls returned, and
// the agent's second attempt gets real data. OpenAI (`tool_calls` + role=tool)
// and Claude (tool_use/tool_result blocks) spellings are both read, because the
// gateway serves both and a client is casual about which one it sends.
func devinCLIToolResults(messages []devinCLIMessage) map[string]string {
	idToName := map[string]string{}
	results := map[string]string{}
	collectNames := func(calls []devinCLIToolCall) {
		for _, call := range calls {
			if call.ID != "" && call.Name != "" {
				idToName[call.ID] = devinCLIToMCPName(call.Name)
			}
		}
	}
	for _, message := range messages {
		if message.Role == "assistant" {
			collectNames(devinCLIOpenAIToolCalls(message.ToolCalls))
			for _, block := range devinCLIContentBlocks(message.Content) {
				if block.Type == "tool_use" && block.ID != "" && block.Name != "" {
					idToName[block.ID] = devinCLIToMCPName(block.Name)
				}
			}
		}
		if message.Role == "tool" && message.ToolCallID != "" {
			if name := idToName[message.ToolCallID]; name != "" {
				results[name] = devinCLIRawText(message.Content)
			}
		}
		if message.Role != "user" {
			continue
		}
		for _, block := range devinCLIContentBlocks(message.Content) {
			if block.Type != "tool_result" || block.ToolUseID == "" {
				continue
			}
			if name := idToName[block.ToolUseID]; name != "" {
				results[name] = devinCLIRawText(block.Content)
			}
		}
	}
	return results
}

// devinCLIClientToolsMCP builds the `clientTools` MCP server declaration that
// exposes the caller's OpenAI tools to the CLI, or nil when the request
// declares none.
func devinCLIClientToolsMCP(tools []deepSeekToolSpec, results map[string]string) map[string]any {
	declarations := make([]devinMCPToolDeclaration, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" {
			continue
		}
		schema := tool.Function.Parameters
		if len(schema) == 0 {
			schema = jsontext.Value(`{"type":"object","properties":{}}`)
		}
		declarations = append(declarations, devinMCPToolDeclaration{
			Name:        devinCLIToMCPName(name),
			Description: tool.Function.Description,
			InputSchema: schema,
		})
	}
	if len(declarations) == 0 {
		return nil
	}
	env := map[string]string{"DEVIN_MCP_TOOLS": devinCLIMarshalText(declarations)}
	if len(results) > 0 {
		env["DEVIN_MCP_RESULTS"] = devinCLIMarshalText(results)
	}
	// Upstream spawns a node script via process.execPath; this port has no node
	// to assume, so the same server is a mode of this binary. The environment
	// contract (DEVIN_MCP_TOOLS / DEVIN_MCP_RESULTS) is identical.
	return map[string]any{
		"command": devinCLISelfExecutable(),
		"args":    []string{DevinClientToolsMCPArg},
		"env":     env,
	}
}

// devinCLIMCPServers collects the MCP servers the CLI should connect: the ones
// the operator lists in DEVIN_MCP_SERVERS, plus the client-tools bridge.
func devinCLIMCPServers(tools []deepSeekToolSpec, results map[string]string) map[string]any {
	servers := map[string]any{}
	if raw := strings.TrimSpace(os.Getenv("DEVIN_MCP_SERVERS")); raw != "" {
		configured := map[string]any{}
		if err := json.Unmarshal([]byte(raw), &configured); err != nil {
			log.Warn("devin-cli", "DEVIN_MCP_SERVERS is not a JSON object; ignoring it", "error", err)
		} else {
			for name, server := range configured {
				servers[name] = server
			}
		}
	}
	if bridge := devinCLIClientToolsMCP(tools, results); bridge != nil {
		servers[DevinClientToolsMCPName] = bridge
	}
	return servers
}

// devinCLIMCPConfigDir writes a throwaway devin config that auto-connects the
// servers, and returns the directory to hand the child as XDG_CONFIG_HOME.
//
// session/new's mcpServers alone does not make devin spawn them (ACP mcp/connect
// is still unstable), so the config file is what actually attaches them —
// exactly as upstream found. It replaces the user's own devin MCP config for the
// life of the child, which is why it is a throwaway directory rather than an
// edit of ~/.config.
func devinCLIMCPConfigDir(servers map[string]any) (string, error) {
	dir, err := os.MkdirTemp("", "devin-mcp-")
	if err != nil {
		return "", fmt.Errorf("devin-cli: create MCP config dir: %w", err)
	}
	devDir := filepath.Join(dir, "devin")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("devin-cli: create MCP config dir: %w", err)
	}
	payload, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("devin-cli: marshal MCP config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "config.json"), payload, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("devin-cli: write MCP config: %w", err)
	}
	return dir, nil
}

// devinCLIMarshalText serialises a value for an MCP environment variable.
func devinCLIMarshalText(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// devinCLISelfExecutable is the binary the client-tools MCP helper is spawned
// from.
func devinCLISelfExecutable() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "panrouter-go"
}
