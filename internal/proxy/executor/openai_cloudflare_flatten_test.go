package executor

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"panrouter/proxy/internal/providers"
)

// The Cloudflare Workers AI rule from upstream paramSupport STRIP_RULES
// (#1926): Workers AI validates `messages[].content` against a oneOf root schema
// that accepts a plain string and rejects the OpenAI content-part array, so a
// client that always sends parts is answered with a 400 that names nothing.

func TestFlattenCloudflareContent(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		changed bool
	}{
		{
			name:    "text blocks are joined in order",
			body:    `{"messages":[{"role":"user","content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}]}`,
			want:    "hello world",
			changed: true,
		},
		{
			name:    "a non-text block contributes nothing",
			body:    `{"messages":[{"role":"user","content":[{"type":"text","text":"look: "},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}},{"type":"text","text":"done"}]}]}`,
			want:    "look: done",
			changed: true,
		},
		{
			name:    "an array with no text blocks becomes an empty string",
			body:    `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`,
			want:    "",
			changed: true,
		},
		{
			name:    "every role is flattened, not only the user turn",
			body:    `{"messages":[{"role":"system","content":[{"type":"text","text":"sys"}]},{"role":"assistant","content":[{"type":"text","text":"asst"}]}]}`,
			want:    "sys|asst",
			changed: true,
		},
		{
			name:    "string content is left alone",
			body:    `{"messages":[{"role":"user","content":"already a string"}]}`,
			changed: false,
		},
		{
			name:    "a body with no messages is left alone",
			body:    `{"model":"@cf/meta/llama-3.1-8b-instruct-fp8-fast"}`,
			changed: false,
		},
		{
			name:    "unparsable JSON is returned untouched",
			body:    `not json`,
			changed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := flattenCloudflareContent([]byte(tt.body))
			if !tt.changed {
				if string(out) != tt.body {
					t.Fatalf("body changed when it must not: %s", out)
				}
				return
			}
			if string(out) == tt.body {
				t.Fatal("body was not flattened")
			}
			var decoded struct {
				Messages []struct {
					Content jsontext.Value `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatalf("flattened body does not parse: %v", err)
			}
			if len(decoded.Messages) == 0 {
				t.Fatal("flattening dropped the messages array")
			}
			joined := ""
			for i, msg := range decoded.Messages {
				var s string
				if err := json.Unmarshal(msg.Content, &s); err != nil {
					t.Fatalf("message %d content is not a string: %s", i, msg.Content)
				}
				if i > 0 {
					joined += "|"
				}
				joined += s
			}
			if joined != tt.want {
				t.Errorf("content = %q, want %q", joined, tt.want)
			}
		})
	}
}

// The rest of the body must survive the rewrite: Cloudflare is picky about the
// content shape, not about the request.
func TestFlattenCloudflareContentKeepsOtherFields(t *testing.T) {
	body := []byte(`{"model":"@cf/meta/llama-3.1-8b-instruct-fp8-fast","max_tokens":64,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	out := flattenCloudflareContent(body)
	var m map[string]jsontext.Value
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("flattened body does not parse: %v", err)
	}
	for key, want := range map[string]string{
		"model":      `"@cf/meta/llama-3.1-8b-instruct-fp8-fast"`,
		"max_tokens": "64",
		"stream":     "true",
	} {
		if string(m[key]) != want {
			t.Errorf("%s = %s, want %s", key, m[key], want)
		}
	}
}

// The rule has to be wired for the provider, not merely available: a body sent
// through ForwardOpenAI must reach the upstream with a string, and a provider
// without the rule must keep its parts.
func TestForwardOpenAI_CloudflareFlattensContentParts(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		received, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	partsBody := []byte(`{"model":"@cf/meta/llama-3.1-8b-instruct-fp8-fast","messages":[{"role":"user","content":[{"type":"text","text":"say "},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}},{"type":"text","text":"hi"}]}]}`)

	for _, provider := range []string{"cloudflare-ai", "cf"} {
		t.Run(provider, func(t *testing.T) {
			received = nil
			rec := httptest.NewRecorder()
			if err := ForwardOpenAI(rec, &Request{
				Ctx:      t.Context(),
				Client:   srv.Client(),
				Config:   &providers.ProviderConfig{BaseURL: srv.URL + "/chat/completions"},
				APIKey:   "k",
				Provider: provider,
				Body:     partsBody,
			}); err != nil {
				t.Fatalf("ForwardOpenAI failed: %v", err)
			}
			var sent struct {
				Messages []struct {
					Content jsontext.Value `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(received, &sent); err != nil {
				t.Fatalf("upstream body does not parse: %v", err)
			}
			var content string
			if err := json.Unmarshal(sent.Messages[0].Content, &content); err != nil {
				t.Fatalf("upstream received array content, want a string: %s", sent.Messages[0].Content)
			}
			if content != "say hi" {
				t.Errorf("upstream content = %q, want %q", content, "say hi")
			}
		})
	}

	// A provider without the rule still receives the parts: the flattening must
	// not have widened into every OpenAI-compatible lane.
	received = nil
	rec := httptest.NewRecorder()
	if err := ForwardOpenAI(rec, &Request{
		Ctx:      t.Context(),
		Client:   srv.Client(),
		Config:   &providers.ProviderConfig{BaseURL: srv.URL + "/chat/completions"},
		APIKey:   "k",
		Provider: "openai",
		Body:     partsBody,
	}); err != nil {
		t.Fatalf("ForwardOpenAI openai failed: %v", err)
	}
	var plain struct {
		Messages []struct {
			Content jsontext.Value `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(received, &plain); err != nil {
		t.Fatalf("upstream body does not parse: %v", err)
	}
	var parts []any
	if err := json.Unmarshal(plain.Messages[0].Content, &parts); err != nil {
		t.Fatalf("openai content = %s, want the content-part array untouched", plain.Messages[0].Content)
	}
	if len(parts) != 3 {
		t.Errorf("openai received %d parts, want 3", len(parts))
	}
}
