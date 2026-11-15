package chat

import (
	"context"
	"strings"
	"testing"

	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/providers"
	"panrouter/proxy/internal/thinking"
	"panrouter/proxy/internal/tokensaver"
	"panrouter/proxy/internal/translator"
)

// tokensaverStats builds the RTK accounting the saver pass reports.
func tokensaverStats(before, after, hits int, filters ...string) tokensaver.MessageCompressionStats {
	return tokensaver.MessageCompressionStats{BytesBefore: before, BytesAfter: after, Hits: hits, Filters: filters}
}

func TestBodyShapeCounts(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		msgs   int
		tools  int
		detail string
	}{
		{
			name: "chat completions",
			body: `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`,
			msgs: 2, tools: 1,
		},
		{
			name: "claude messages carry tools at the top level too",
			body: `{"model":"m","system":"s","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"a"},{"name":"b"},{"name":"c"}]}`,
			msgs: 1, tools: 3,
		},
		{
			name: "responses input",
			body: `{"model":"m","input":[{"role":"user","content":"hi"},{"role":"user","content":"again"}],"tools":[{"type":"function"}]}`,
			msgs: 2, tools: 1,
		},
		{
			name: "gemini contents",
			body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			msgs: 1, tools: 0,
		},
		{
			name: "no tools declared",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			msgs: 1, tools: 0,
		},
		{
			name: "empty messages fall through to input",
			body: `{"messages":[],"input":[{"role":"user","content":"hi"}]}`,
			msgs: 1, tools: 0,
			detail: "upstream takes the first non-empty of messages/input/contents",
		},
		{
			name: "nested values do not confuse the walk",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"{\"messages\":[1,2,3]}"}]}],"metadata":{"messages":[1,2]}}`,
			msgs: 1, tools: 0,
		},
		{
			name: "empty body",
			body: "",
			msgs: 0, tools: 0,
		},
		{
			name: "not an object",
			body: `[{"role":"user"}]`,
			msgs: 0, tools: 0,
		},
		{
			name: "truncated body reports what it counted",
			body: `{"messages":[{"role":"user"},{"role":"user"`,
			msgs: 1, tools: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMsgs, gotTools := bodyShapeCounts([]byte(tt.body))
			if gotMsgs != tt.msgs || gotTools != tt.tools {
				t.Errorf("bodyShapeCounts() = (%d, %d), want (%d, %d) %s",
					gotMsgs, gotTools, tt.msgs, tt.tools, tt.detail)
			}
		})
	}
}

func TestClientWireFormat(t *testing.T) {
	responsesCtx := translator.WithClientFormat(context.Background(), translator.ClientFormatResponses)
	tests := []struct {
		name     string
		ctx      context.Context
		endpoint string
		want     string
	}{
		{"messages endpoint is claude", context.Background(), "/v1/messages", providers.FormatClaude},
		{"converted messages endpoint is claude", context.Background(), "/v1/v1/messages", providers.FormatClaude},
		{"chat completions", context.Background(), "/v1/chat/completions", providers.FormatOpenAI},
		{"responses client", responsesCtx, "/v1/responses", providers.FormatOpenAIResponses},
		{"nil context", nil, "/v1/chat/completions", providers.FormatOpenAI},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientWireFormat(tt.ctx, tt.endpoint); got != tt.want {
				t.Errorf("clientWireFormat() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUpstreamWireFormat(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		model    string
		cfg      *providers.ProviderConfig
		want     string
	}{
		{
			name: "declared lane wins", provider: "opencode-zen", model: "muse-spark-1.3",
			cfg:  &providers.ProviderConfig{Format: providers.FormatOpenAI},
			want: providers.FormatOpenAIResponses,
		},
		{
			name: "registry wire when the model declares nothing", provider: "claude", model: "claude-sonnet-4-6",
			cfg:  &providers.ProviderConfig{Format: providers.FormatClaude},
			want: providers.FormatClaude,
		},
		{
			name: "chat completions by default", provider: "openai", model: "gpt-5.4",
			want: providers.FormatOpenAI,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := upstreamWireFormat(tt.provider, tt.model, tt.cfg); got != tt.want {
				t.Errorf("upstreamWireFormat(%q, %q) = %q, want %q", tt.provider, tt.model, got, tt.want)
			}
		})
	}
}

func TestThinkForLine(t *testing.T) {
	tests := []struct {
		name string
		in   *thinking.Intent
		want string
	}{
		{"no intent", nil, ""},
		{"disabled", &thinking.Intent{Mode: thinking.ModeNone}, "off"},
		{"auto", &thinking.Intent{Mode: thinking.ModeAuto}, "auto"},
		{"level", &thinking.Intent{Mode: thinking.ModeLevel, Level: "high"}, "high"},
		{"budget", &thinking.Intent{Mode: thinking.ModeBudget, Budget: 10000}, "10k"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := thinkForLine(tt.in); got != tt.want {
				t.Errorf("thinkForLine(%+v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestEmitAttemptLines_OrderAndShapes is the operator's view of one attempt: the
// request line, then the RTK savings line, then the flag line naming the
// caveman level that shaped the request.
func TestEmitAttemptLines_OrderAndShapes(t *testing.T) {
	log.ClearConsoleLogs()
	log.SetLevel(log.LevelInfo)
	defer log.SetLevel(log.LevelInfo)

	emitAttemptLines("🟡", log.RequestLine{
		ClientModel: "th/deepseek-v4.1-flash:free",
		Provider:    "tokenharbor",
		Model:       "deepseek-v4.1-flash:free",
		SourceFmt:   providers.FormatOpenAI,
		TargetFmt:   providers.FormatOpenAI,
		Stream:      true,
		Messages:    245,
		Tools:       13,
		AccName:     "Key 84",
		RequestID:   "9c6d61",
	}, tokenSaverResult{
		RTK: tokensaverStats(170413, 165817, 12, "dedup-log", "git-status", "build-output", "ls"),
		CavemanLevel: "full",
	})

	entries := log.ConsoleEntries()
	if len(entries) != 3 {
		t.Fatalf("expected 3 console lines, got %d: %+v", len(entries), entries)
	}
	wantContains := []string{
		"🟡 ▶ POST th/deepseek-v4.1-flash:free → tokenharbor/deepseek-v4.1-flash:free · FMT: openai→openai · STREAM · 245 MSG · 13 TOOL · ACC:Key 84 · REQ:9c6d61",
		"[RTK] saved 4596B / 170413B (2.7%) via [dedup-log,git-status,build-output,ls] hits=12",
		"🟡 ⚙ CAVEMAN:full",
	}
	for i, want := range wantContains {
		if !strings.Contains(entries[i].Line, want) {
			t.Errorf("line %d = %q, want it to contain %q", i, entries[i].Line, want)
		}
	}
}

// A saver pass that saved nothing and injected no persona must print no saver
// lines at all: a request line on its own is a valid, quiet request.
func TestEmitAttemptLines_NoSaverLinesWhenNothingHappened(t *testing.T) {
	log.ClearConsoleLogs()
	log.SetLevel(log.LevelInfo)
	defer log.SetLevel(log.LevelInfo)

	emitAttemptLines("🔵", log.RequestLine{
		ClientModel: "m", Provider: "p", Model: "m",
		SourceFmt: providers.FormatOpenAI, TargetFmt: providers.FormatOpenAI,
		Messages: 1, AccID: "0b1c2d3e-4f50", RequestID: "abcdef",
	}, tokenSaverResult{})

	entries := log.ConsoleEntries()
	if len(entries) != 1 {
		t.Fatalf("expected only the request line, got %d: %+v", len(entries), entries)
	}
	if !strings.Contains(entries[0].Line, "ACC:0b1c2d3e") {
		t.Errorf("line = %q, want the id prefix as the account", entries[0].Line)
	}
}