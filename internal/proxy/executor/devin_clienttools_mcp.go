package executor

import (
	"bufio"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"os"
	"strings"
)

// DevinClientToolsMCPArg selects the client-tools helper inside this binary.
//
// The lane exposes the caller's OpenAI tools to the CLI as a stdio MCP server.
// Upstream spawns a node script for that job (`process.execPath` + a written
// .mjs file); this port has no node runtime to assume, so the same server is a
// mode of this executable: same protocol, same environment contract
// (DEVIN_MCP_TOOLS / DEVIN_MCP_RESULTS), same answers.
const DevinClientToolsMCPArg = "devin-clienttools-mcp"

// DevinClientToolsMCPName is the MCP server name the CLI sees.
const DevinClientToolsMCPName = "clientTools"

// devinMCPProtocolVersion is the MCP protocol revision the helper announces,
// verbatim from upstream's bridge script.
const devinMCPProtocolVersion = "2024-11-05"

// devinMCPToolDeclaration is one tool as the CLI's MCP client discovers it:
// the OpenAI schema with the `mcp_` prefix, in MCP's own field spelling.
type devinMCPToolDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema jsontext.Value `json:"inputSchema"`
}

// ServeDevinClientToolsMCP runs the helper when argv selects it and reports
// whether it handled the process. Called first thing from main, before the CLI
// parser sees an argument it does not know.
func ServeDevinClientToolsMCP(args []string) bool {
	if len(args) == 0 || args[0] != DevinClientToolsMCPArg {
		return false
	}
	_ = serveDevinClientToolsMCP(os.Stdin, os.Stdout,
		os.Getenv("DEVIN_MCP_TOOLS"), os.Getenv("DEVIN_MCP_RESULTS"))
	return true
}

// serveDevinClientToolsMCP answers MCP requests on one line each until stdin
// ends. A line that is not JSON is ignored, exactly as upstream's bridge does:
// the CLI's client may write a banner or a partial write, and a bridge that
// died on one would take the agent's tool access down with it.
func serveDevinClientToolsMCP(in io.Reader, out io.Writer, toolsJSON, resultsJSON string) error {
	tools := []devinMCPToolDeclaration{}
	if strings.TrimSpace(toolsJSON) != "" {
		_ = json.Unmarshal([]byte(toolsJSON), &tools)
	}
	results := map[string]any{}
	if strings.TrimSpace(resultsJSON) != "" {
		_ = json.Unmarshal([]byte(resultsJSON), &results)
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 16<<10), devinCLIMaxFrameBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg devinCLIRPCMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if len(msg.ID) == 0 {
			continue // a notification needs no answer
		}
		switch msg.Method {
		case "initialize":
			if err := devinMCPWriteResponse(out, msg.ID, map[string]any{
				"protocolVersion": devinMCPProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": DevinClientToolsMCPName, "version": "1.0"},
			}); err != nil {
				return err
			}
		case "tools/list":
			if err := devinMCPWriteResponse(out, msg.ID, map[string]any{"tools": tools}); err != nil {
				return err
			}
		case "tools/call":
			var params struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			text := "(awaiting client tool_result)"
			if seeded, ok := results[params.Name]; ok {
				text = devinMCPText(seeded)
			}
			fmt.Fprintf(os.Stderr, "[client-tools] tool_call name=%s seeded=%v\n",
				params.Name, results[params.Name] != nil)
			if err := devinMCPWriteResponse(out, msg.ID, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}},
			}); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

// devinMCPWriteResponse writes one newline-terminated JSON-RPC result.
func devinMCPWriteResponse(out io.Writer, id jsontext.Value, result any) error {
	line, err := json.Marshal(devinCLIRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
	if err != nil {
		return fmt.Errorf("devin-cli client-tools: marshal response: %w", err)
	}
	if _, err := out.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("devin-cli client-tools: write response: %w", err)
	}
	return nil
}

// devinMCPText renders a seeded tool result the way upstream's bridge does:
// a string stays as it is, anything else is serialised as JSON.
func devinMCPText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}
