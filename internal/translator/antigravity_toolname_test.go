package translator_test

import (
	"regexp"
	"strings"
	"testing"

	json "encoding/json/v2"
	"panrouter/proxy/internal/translator"
)

// geminiFunctionNameRe is the shape Google documents for a function
// declaration: [a-zA-Z_][a-zA-Z0-9_.:\-]{0,63}.
var geminiFunctionNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.:\-]{0,63}$`)

// TestCloakAntigravityRequest_SanitizesFunctionNames ports
// open-sse/executors/antigravity.js sanitizeFunctionName (:12-19): generateContent
// rejects the whole request when one declaration is not a valid function name,
// so the name is repaired before it reaches the wire, and the decloak map is
// keyed on the REPAIRED name because that is what the backend echoes back on a
// functionCall.
//
// Fails before the change: names went out verbatim ("my tool" plus the cloak
// suffix) and the map key carried the unsanitised spelling, which no response
// could ever match.
func TestCloakAntigravityRequest_SanitizesFunctionNames(t *testing.T) {
	longName := strings.Repeat("a", 70)
	tests := []struct {
		name     string
		toolName string
		wantWire string
	}{
		{name: "space is replaced", toolName: "my tool", wantWire: "my_tool_ide"},
		{name: "leading digit is prefixed", toolName: "9lives", wantWire: "_9lives_ide"},
		{name: "slash is replaced", toolName: "fs/read", wantWire: "fs_read_ide"},
		{name: "dots and colons survive", toolName: "mcp.server:run", wantWire: "mcp.server:run_ide"},
		{name: "already valid keeps the suffix", toolName: "get_weather", wantWire: "get_weather_ide"},
		{name: "overlong name is truncated to 64 bytes", toolName: longName, wantWire: strings.Repeat("a", 64)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &translator.GeminiRequest{
				Tools: []translator.GeminiTool{{FunctionDeclarations: []translator.GeminiFunctionDecl{
					{Name: tt.toolName, Parameters: map[string]any{"type": "object"}},
				}}},
			}
			cloaked, toolMap := translator.CloakAntigravityRequest(req, "")
			if cloaked == nil {
				t.Fatal("expected a cloaked request")
			}
			// The client declaration comes first; the decoys follow it.
			decl := cloaked.Tools[0].FunctionDeclarations[0]
			if decl.Name != tt.wantWire {
				t.Fatalf("wire name = %q, want %q", decl.Name, tt.wantWire)
			}
			if !geminiFunctionNameRe.MatchString(decl.Name) {
				t.Errorf("wire name %q is not a valid Gemini function name", decl.Name)
			}
			// (b) the response maps the echoed name back to the caller's own
			// spelling, including the characters sanitisation removed.
			if got := translator.UncloakToolName(decl.Name, toolMap); got != tt.toolName {
				t.Errorf("decloaked %q = %q, want the caller's %q", decl.Name, got, tt.toolName)
			}
		})
	}

	// Every injected declaration must be valid too: the decoys and the native
	// names are what pad the list, and one bad name rejects the whole request.
	req := &translator.GeminiRequest{
		Tools: []translator.GeminiTool{{FunctionDeclarations: []translator.GeminiFunctionDecl{
			{Name: "my tool", Parameters: map[string]any{"type": "object"}},
			{Name: "view_file"},
		}}},
	}
	cloaked, _ := translator.CloakAntigravityRequest(req, "")
	for _, decl := range cloaked.Tools[0].FunctionDeclarations {
		if !geminiFunctionNameRe.MatchString(decl.Name) {
			t.Errorf("declaration %q is not a valid Gemini function name", decl.Name)
		}
	}
}

// TestCloakAntigravityRequest_SanitizesHistoryToolCalls pins the same repair on
// the conversation history: a functionCall/functionResponse Google sent back
// under the cloaked name must match the declaration the tools[] list carries,
// or the model sees a call to a tool it was never offered.
func TestCloakAntigravityRequest_SanitizesHistoryToolCalls(t *testing.T) {
	req := &translator.GeminiRequest{
		Tools: []translator.GeminiTool{{FunctionDeclarations: []translator.GeminiFunctionDecl{
			{Name: "my tool", Parameters: map[string]any{"type": "object"}},
		}}},
		Contents: []translator.GeminiContent{{
			Role: "model",
			Parts: []translator.GeminiPart{{
				FunctionCall: &translator.GeminiFunctionCall{Name: "my tool"},
			}},
		}, {
			Role: "user",
			Parts: []translator.GeminiPart{{
				FunctionResponse: &translator.GeminiFunctionResp{Name: "my tool"},
			}},
		}},
	}
	cloaked, _ := translator.CloakAntigravityRequest(req, "")
	if got := cloaked.Contents[0].Parts[0].FunctionCall.Name; got != "my_tool_ide" {
		t.Errorf("history functionCall = %q, want the sanitised wire name", got)
	}
	if got := cloaked.Contents[1].Parts[0].FunctionResponse.Name; got != "my_tool_ide" {
		t.Errorf("history functionResponse = %q, want the sanitised wire name", got)
	}
	if !geminiFunctionNameRe.MatchString(cloaked.Contents[0].Parts[0].FunctionCall.Name) {
		t.Error("history functionCall name is not a valid Gemini function name")
	}
}

// TestSanitizeFunctionName pins the ported rules themselves, including the
// empty-name placeholder upstream returns ("_unknown").
func TestSanitizeFunctionName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "_unknown"},
		{"_private", "_private"},
		{"a", "a"},
		{"has space", "has_space"},
		{"1st", "_1st"},
		{"a[b]", "a_b_"},
	}
	for _, tt := range tests {
		if got := translator.SanitizeFunctionName(tt.in); got != tt.want {
			t.Errorf("SanitizeFunctionName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestWrapForAntigravity_SessionIDShapes pins the sessionId acceptance set:
// upstream reads `body.request?.sessionId` whether it is a string or a number
// (open-sse/executors/antigravity.js:217-218, `toNumericSessionId(raw) || raw`),
// so a caller that sends a numeric id must still get a well-formed envelope.
//
// The wire field is a SIGNED NUMERIC id, always: a numeric client session
// passes through, and anything else is folded onto a stable negative number
// (`toNumericSessionId`: sha256 → high 8 bytes → `& 0x7fff…` → `-N`). The
// shipped client sends the same shape (`sessionId: -3750763034362895579` in
// the Hub 2.32.1 capture), and a literal session name on the wire is a
// fingerprint the backend does not recognise.
//
// The client's own body value is replaced rather than decoded into a typed
// field: the port derives the envelope session from the session it is handed (a
// header/connection id, always a Go string), so a numeric value in the body
// cannot break the marshal.
func TestWrapForAntigravity_SessionIDShapes(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "numeric session id", body: `{"sessionId":1794000000000,"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		{name: "string session id", body: `{"sessionId":"sess-1","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		{name: "null session id", body: `{"sessionId":null,"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		{name: "absent session id", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrapped, err := translator.WrapForAntigravity([]byte(tt.body), "proj-1", "gemini-3.8-flash-medium", "sess-derived")
			if err != nil {
				t.Fatalf("WrapForAntigravity: %v", err)
			}
			var envelope struct {
				Request map[string]any `json:"request"`
			}
			if err := json.Unmarshal(wrapped, &envelope); err != nil {
				t.Fatalf("the envelope is not decodable: %v", err)
			}
			sid, ok := envelope.Request["sessionId"].(string)
			if !ok || sid == "" {
				t.Fatalf("request.sessionId = %#v, want a non-empty string", envelope.Request["sessionId"])
			}
			if !antigravityNumericSessionIDRe.MatchString(sid) {
				t.Errorf("request.sessionId = %q, want a signed numeric id", sid)
			}
			// The same session keeps the same number, so a conversation stays one
			// conversation across turns.
			again, err := translator.WrapForAntigravity([]byte(tt.body), "proj-1", "gemini-3.8-flash-medium", "sess-derived")
			if err != nil {
				t.Fatalf("WrapForAntigravity: %v", err)
			}
			var second struct {
				Request map[string]any `json:"request"`
			}
			if err := json.Unmarshal(again, &second); err != nil {
				t.Fatalf("the envelope is not decodable: %v", err)
			}
			if got, _ := second.Request["sessionId"].(string); got != sid {
				t.Errorf("sessionId changed between turns: %q then %q", sid, got)
			}
		})
	}
}
