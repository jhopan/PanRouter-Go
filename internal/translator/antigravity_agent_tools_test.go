package translator_test

import (
	"encoding/json"
	"testing"

	"panrouter/proxy/internal/translator"
)

// TestWrapForAntigravityAgentToolsKeepTheCallerContract pins the live Hub wire
// captured 2026-11-18: an agent request carries its actual tools (Hub: 16, CLI:
// 12), not a padded fake IDE inventory. Adding 21 decoys or rewriting every name
// to `_ide` made a 13-tool agent request look unlike either official client and
// correlated with detail-free 429 RESOURCE_EXHAUSTED on the long tool path.
func TestWrapForAntigravityAgentToolsKeepTheCallerContract(t *testing.T) {
	body := []byte(`{
		"system_instruction":{"parts":[{"text":"You are a helpful assistant."}]},
		"contents":[
			{"role":"model","parts":[{"functionCall":{"name":"execute_code","args":{"code":"ls"}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"execute_code","response":{"result":"ok"}}}]}
		],
		"tools":[{"functionDeclarations":[
			{"name":"execute_code","description":"Run code","parameters":{"type":"object"}},
			{"name":"read_file","description":"Read file","parameters":{"type":"object"}}
		]}]
	}`)

	wrapped, err := translator.WrapForAntigravity(body, "proj-1", "claude-sonnet-4-6", "tools-session")
	if err != nil {
		t.Fatalf("WrapForAntigravity: %v", err)
	}
	var envelope struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(wrapped, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var request struct {
		Contents []struct {
			Parts []struct {
				FunctionCall *struct {
					Name string `json:"name"`
				} `json:"functionCall"`
				FunctionResponse *struct {
					Name string `json:"name"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
		Tools []struct {
			FunctionDeclarations []struct {
				Name string `json:"name"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
		ToolConfig map[string]any `json:"toolConfig"`
	}
	if err := json.Unmarshal(envelope.Request, &request); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if len(request.Tools) != 1 || len(request.Tools[0].FunctionDeclarations) != 2 {
		t.Fatalf("wire tool declarations = %#v, want exactly the 2 caller tools", request.Tools)
	}
	if got := request.Tools[0].FunctionDeclarations[0].Name; got != "execute_code" {
		t.Errorf("first wire declaration = %q, want caller name execute_code", got)
	}
	var callName, responseName string
	for _, content := range request.Contents {
		for _, part := range content.Parts {
			if part.FunctionCall != nil {
				callName = part.FunctionCall.Name
			}
			if part.FunctionResponse != nil {
				responseName = part.FunctionResponse.Name
			}
		}
	}
	if callName != "execute_code" {
		t.Errorf("wire functionCall = %q, want caller name execute_code", callName)
	}
	if responseName != "execute_code" {
		t.Errorf("wire functionResponse = %q, want caller name execute_code", responseName)
	}
	if request.ToolConfig == nil {
		t.Fatal("toolConfig missing")
	}
	fc, _ := request.ToolConfig["functionCallingConfig"].(map[string]any)
	if got, _ := fc["mode"].(string); got != "VALIDATED" {
		t.Errorf("toolConfig mode = %q, want VALIDATED", got)
	}
}
