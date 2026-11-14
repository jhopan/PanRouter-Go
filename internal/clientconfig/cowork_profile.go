package clientconfig

// The cowork profile document and the MCP servers it lists. Split from
// cowork.go because the adapter decides *when* to write and this file decides
// *what*: the SECURITY_RELAX block, the default remote plugins, and Claude
// Desktop's own config edits.
//
// Mirrors src/shared/constants/coworkPlugins.js and the constants at the top of
// src/app/api/cli-tools/cowork-settings/route.js.

// coworkPlugin is one default remote MCP server (DEFAULT_PLUGINS in
// src/shared/constants/coworkPlugins.js).
type coworkPlugin struct {
	name      string
	url       string
	transport string
	oauth     bool
	toolNames []string
}

// coworkDefaultPlugins mirrors DEFAULT_PLUGINS in the upstream constants file.
var coworkDefaultPlugins = []coworkPlugin{
	{
		name:      "exa",
		url:       "https://mcp.exa.ai/mcp",
		transport: "http",
		toolNames: []string{"web_search_exa", "web_fetch_exa"},
	},
	{
		name:      "tavily",
		url:       "https://mcp.tavily.com/mcp",
		transport: "http",
		oauth:     true,
		toolNames: []string{"tavily_search", "tavily_extract", "tavily_crawl", "tavily_map"},
	},
}

// coworkLocalPluginNames are LOCAL_STDIO_PLUGINS' names; an earlier version
// wrote them into Claude's own config and the route now removes them.
var coworkLocalPluginNames = []string{"browsermcp"}

// coworkConfig is one profile document, in the field order the route builds it:
// SECURITY_RELAX first, then the inference fields, then the servers.
type coworkConfig struct {
	CoworkEgressAllowedHosts            []string          `json:"coworkEgressAllowedHosts"`
	DisabledBuiltinTools                []string          `json:"disabledBuiltinTools"`
	IsLocalDevMcpEnabled                bool              `json:"isLocalDevMcpEnabled"`
	IsDesktopExtensionEnabled           bool              `json:"isDesktopExtensionEnabled"`
	IsDesktopExtensionDirectoryEnabled  bool              `json:"isDesktopExtensionDirectoryEnabled"`
	IsDesktopExtensionSignatureRequired bool              `json:"isDesktopExtensionSignatureRequired"`
	IsClaudeCodeForDesktopEnabled       bool              `json:"isClaudeCodeForDesktopEnabled"`
	DisableEssentialTelemetry           bool              `json:"disableEssentialTelemetry"`
	DisableNonessentialTelemetry        bool              `json:"disableNonessentialTelemetry"`
	DisableNonessentialServices         bool              `json:"disableNonessentialServices"`
	InferenceProvider                   string            `json:"inferenceProvider"`
	InferenceGatewayBaseURL             string            `json:"inferenceGatewayBaseUrl"`
	InferenceGatewayAPIKey              string            `json:"inferenceGatewayApiKey"`
	InferenceModels                     []coworkModel     `json:"inferenceModels"`
	ManagedMcpServers                   []coworkMCPServer `json:"managedMcpServers,omitempty"`
}

type coworkModel struct {
	Name string `json:"name"`
}

// coworkMCPServer is one managedMcpServers entry.
type coworkMCPServer struct {
	Name       string            `json:"name"`
	URL        string            `json:"url"`
	Transport  string            `json:"transport"`
	OAuth      bool              `json:"oauth,omitempty"`
	ToolPolicy map[string]string `json:"toolPolicy,omitempty"`
}

// coworkConfigFor builds the profile document SECURITY_RELAX and the request
// describe.
func coworkConfigFor(opts Options, models []string) coworkConfig {
	inferenceModels := make([]coworkModel, 0, len(models))
	for _, m := range models {
		inferenceModels = append(inferenceModels, coworkModel{Name: m})
	}
	return coworkConfig{
		CoworkEgressAllowedHosts:            []string{"*"},
		DisabledBuiltinTools:                []string{},
		IsLocalDevMcpEnabled:                true,
		IsDesktopExtensionEnabled:           true,
		IsDesktopExtensionDirectoryEnabled:  true,
		IsDesktopExtensionSignatureRequired: false,
		IsClaudeCodeForDesktopEnabled:       true,
		DisableEssentialTelemetry:           true,
		DisableNonessentialTelemetry:        true,
		DisableNonessentialServices:         true,
		InferenceProvider:                   coworkProvider,
		InferenceGatewayBaseURL:             opts.BaseURL,
		InferenceGatewayAPIKey:              cliAPIKey(opts.APIKey),
		InferenceModels:                     inferenceModels,
		ManagedMcpServers:                   coworkManagedServers(),
	}
}

// coworkManagedServers renders DEFAULT_PLUGINS as managedMcpServers entries,
// with the toolPolicy map buildManagedMcpServers builds: bare and
// "<name>-"-prefixed variants, both allowed.
func coworkManagedServers() []coworkMCPServer {
	out := make([]coworkMCPServer, 0, len(coworkDefaultPlugins))
	for _, p := range coworkDefaultPlugins {
		entry := coworkMCPServer{Name: p.name, URL: p.url, Transport: p.transport, OAuth: p.oauth}
		if len(p.toolNames) > 0 {
			policy := make(map[string]string, len(p.toolNames)*2)
			prefix := p.name + "-"
			for _, tool := range p.toolNames {
				policy[tool] = "allow"
				policy[prefix+tool] = "allow"
			}
			entry.ToolPolicy = policy
		}
		out = append(out, entry)
	}
	return out
}

func coworkSkipApprovals() map[string]bool {
	out := make(map[string]bool, len(coworkDefaultPlugins))
	for _, p := range coworkDefaultPlugins {
		out[p.name] = true
	}
	return out
}

// coworkDesktopPlan switches Claude Desktop into 3p mode and removes the stdio
// entries an earlier version wrote into its own config.
func coworkDesktopPlan(file TargetFile) FilePlan {
	edits := []Edit{{KeyPath: []string{"deploymentMode"}, Value: "3p", CreateIfMissing: true}}
	for _, name := range coworkLocalPluginNames {
		edits = append(edits, Edit{KeyPath: []string{"mcpServers", name}, Delete: true})
	}
	return FilePlan{File: file, Edits: edits}
}

// coworkLegacyDesktopPlan is the reset-side cleanup: it only exists when the
// desktop config is already there and still holds a managed stdio entry.
func coworkLegacyDesktopPlan(file TargetFile) *FilePlan {
	b, err := readClientFile(file)
	if err != nil || len(b) == 0 {
		return nil
	}
	var cfg map[string]any
	if parseClientJSON(b, &cfg) != nil {
		return nil
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		return nil
	}
	var edits []Edit
	for _, name := range coworkLocalPluginNames {
		if _, present := servers[name]; present {
			edits = append(edits, Edit{KeyPath: []string{"mcpServers", name}, Delete: true})
		}
	}
	if len(edits) == 0 {
		return nil
	}
	return &FilePlan{File: file, Edits: edits}
}
