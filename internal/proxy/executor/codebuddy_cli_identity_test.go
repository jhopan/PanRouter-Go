package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"panrouter/proxy/internal/providers"
)

// The CodeBuddy chat request must look like the official CLI 2.178.0 on the
// wire, on BOTH lanes.
//
// Source of truth: a live MITM capture of CodeBuddy Code CLI 2.178.0 on
// Windows/x64, ported from upstream `tests/unit/codebuddy-intl-cli-identity.test.js`.
// The gateway keys on this header set: a request missing it still passes
// channel verification (11128) but is classified as third-party automation and
// eventually answered with 11140 "account restricted due to unusual activity".
// Sending a header the official client never sends is the same tell as
// omitting one, which is why the removed headers are asserted absent rather
// than merely unused.

// codebuddyTestSub is the account id carried by the test bearer token.
const codebuddyTestSub = "85482e42-2454-468d-a2b0-f1f6311f0f81"

// codebuddyTestToken builds the JWT-shaped bearer the OAuth lane stores: the
// payload carries the account id in `sub`, the way the CLI's token does.
func codebuddyTestToken(sub string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + sub + `"}`))
	return "eyJhbGciOiJIUzI1NiJ9." + payload + ".sig"
}

// codebuddyCLIHeaderKeysCN is the complete header set the CN lane's request
// builder emits. Exact equality (not containment) keeps a stale header from
// creeping back in.
var codebuddyCLIHeaderKeysCN = []string{
	"Accept",
	"Content-Encoding",
	"Content-Type",
	"User-Agent",
	"X-Agent-Intent",
	"X-Agent-Purpose",
	"X-Agent-Runtime",
	"X-Agent-Type",
	"X-CodeBuddy-Request",
	"X-Conversation-ID",
	"X-Conversation-Message-ID",
	"X-Conversation-Request-ID",
	"X-Domain",
	"X-IDE-Name",
	"X-IDE-Type",
	"X-IDE-Version",
	"X-Private-Data",
	"X-Product",
	"X-Request-ID",
	"X-Requested-With",
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"X-User-Id",
}

// codebuddyCLIHeaderKeysIntl adds what the intl capture carries on top of the
// shared set: the root request id and the OpenTelemetry/B3 tracing family
// (open-sse/executors/codebuddy-intl.js buildHeaders). The CN executor sends no
// tracing — the CLI it mirrors does not — so this list is intl-only.
var codebuddyCLIHeaderKeysIntl = append(append([]string(nil), codebuddyCLIHeaderKeysCN...),
	"X-Root-Request-ID",
	"traceparent",
	"b3",
	"X-B3-TraceId",
	"X-B3-SpanId",
	"X-B3-ParentSpanId",
	"X-B3-Sampled",
	"X-Trace-ID",
	"baggage",
)

func TestCodeBuddyHeadersCLIIdentityPerLane(t *testing.T) {
	tests := []struct {
		name       string
		provider   string
		baseURL    string
		wantDomain string
	}{
		{
			name:       "intl lane",
			provider:   "codebuddy-intl",
			baseURL:    "https://www.codebuddy.ai/v2/chat/completions",
			wantDomain: "www.codebuddy.ai",
		},
		{
			name:       "cn lane",
			provider:   "codebuddy-cn",
			baseURL:    "https://copilot.tencent.com/v2/chat/completions",
			wantDomain: "copilot.tencent.com",
		},
	}
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &Request{
				Provider: tt.provider,
				APIKey:   codebuddyTestToken(codebuddyTestSub),
				Body:     body,
			}
			h := codebuddyHeaders(req, &providers.ProviderConfig{BaseURL: tt.baseURL}, body)

			keys := make([]string, 0, len(h))
			for k := range h {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			want := append([]string(nil), codebuddyCLIHeaderKeysCN...)
			if tt.provider == "codebuddy-intl" {
				want = append([]string(nil), codebuddyCLIHeaderKeysIntl...)
			}
			sort.Strings(want)
			if strings.Join(keys, ",") != strings.Join(want, ",") {
				t.Fatalf("header set mismatch\n got: %v\nwant: %v", keys, want)
			}

			stable := map[string]string{
				"User-Agent":                  "CLI/2.178.0 CodeBuddy/2.178.0",
				"X-IDE-Version":               "2.178.0",
				"Accept":                      "application/json",
				"Content-Encoding":            "gzip",
				"X-User-Id":                   codebuddyTestSub,
				"X-Product":                   "SaaS",
				"X-IDE-Type":                  "CLI",
				"X-IDE-Name":                  "CLI",
				"X-Private-Data":              "false",
				"X-Requested-With":            "XMLHttpRequest",
				"X-CodeBuddy-Request":         "1",
				"X-Domain":                    tt.wantDomain,
				"X-Agent-Intent":              "craft",
				"X-Agent-Purpose":             "conversation",
				"X-Agent-Type":                "main",
				"X-Agent-Runtime":             "other",
				"X-Stainless-Lang":            "js",
				"X-Stainless-Runtime":         "node",
				"X-Stainless-Package-Version": "7.39.0",
				"X-Stainless-Runtime-Version": "v26.7.0",
				"X-Stainless-Retry-Count":     "0",
			}
			for key, want := range stable {
				if got := h[key]; got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if h["X-Stainless-Arch"] != codebuddyStainlessArch() {
				t.Errorf("X-Stainless-Arch = %q, want %q", h["X-Stainless-Arch"], codebuddyStainlessArch())
			}
			if h["X-Stainless-OS"] != codebuddyStainlessOS() {
				t.Errorf("X-Stainless-OS = %q, want %q", h["X-Stainless-OS"], codebuddyStainlessOS())
			}

			// The official CLI sends none of these; their presence marks a
			// hand-rolled client.
			for _, removed := range []string{"X-App", "X-Product-Version", "X-Stainless-Helper-Method"} {
				if v, ok := h[removed]; ok {
					t.Errorf("%s = %q, want it absent", removed, v)
				}
			}
		})
	}
}

// TestCodebuddyHeaders_UserIDOnlyFromASubClaim pins the header to the token it
// travels with: an opaque key carries no account id, so the header is omitted
// rather than invented.
func TestCodeBuddyHeadersUserIDOnlyFromASubClaim(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	h := codebuddyHeaders(&Request{Provider: "codebuddy-intl", APIKey: "sk-opaque", Body: body}, nil, body)
	if v, ok := h["X-User-Id"]; ok {
		t.Errorf("X-User-Id = %q, want it omitted for a token without a sub claim", v)
	}

	h = codebuddyHeaders(&Request{Provider: "codebuddy-intl", APIKey: codebuddyTestToken("acct-42"), Body: body}, nil, body)
	if h["X-User-Id"] != "acct-42" {
		t.Errorf("X-User-Id = %q, want acct-42", h["X-User-Id"])
	}
}

// TestForwardCodebuddy_GzipBodyAndCLIHeaders asserts what the upstream actually
// receives: a gzip body whose Content-Length is the compressed byte count, and
// the CLI's Accept/X-User-Id instead of the forwarder's defaults.
func TestCodeBuddyForwardGzipBodyAndCLIHeaders(t *testing.T) {
	tests := []struct {
		name     string
		provider string
	}{
		{name: "intl lane", provider: "codebuddy-intl"},
		{name: "cn lane", provider: "codebuddy-cn"},
	}
	input := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"ping"}]}`)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantPlain, err := transformCodebuddyBodyFor(tt.provider, input)
			if err != nil {
				t.Fatalf("transform body: %v", err)
			}
			var gotHeader http.Header
			var rawBody []byte
			var contentLength int64
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeader = r.Header.Clone()
				contentLength = r.ContentLength
				rawBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
			}))
			defer ts.Close()

			rec := httptest.NewRecorder()
			req := &Request{
				Ctx:      context.Background(),
				Client:   ts.Client(),
				Config:   &providers.ProviderConfig{BaseURL: ts.URL},
				Provider: tt.provider,
				APIKey:   codebuddyTestToken(codebuddyTestSub),
				IsStream: true,
				Body:     input,
			}
			if err := ForwardCodebuddyCN(rec, req); err != nil {
				t.Fatalf("ForwardCodebuddyCN: %v", err)
			}

			if got := gotHeader.Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip", got)
			}
			if gotHeader.Get("Accept") != "application/json" {
				t.Errorf("Accept = %q, want application/json", gotHeader.Get("Accept"))
			}
			if gotHeader.Get("X-User-Id") != codebuddyTestSub {
				t.Errorf("X-User-Id = %q, want %q", gotHeader.Get("X-User-Id"), codebuddyTestSub)
			}
			if contentLength != int64(len(rawBody)) {
				t.Errorf("Content-Length = %d, want the compressed byte count %d", contentLength, len(rawBody))
			}
			if len(rawBody) >= len(wantPlain) {
				t.Errorf("wire body is %d bytes, not smaller than the %d-byte JSON", len(rawBody), len(wantPlain))
			}

			gz, gzErr := gzip.NewReader(bytes.NewReader(rawBody))
			if gzErr != nil {
				t.Fatalf("wire body is not gzip: %v", gzErr)
			}
			plain, gzErr := io.ReadAll(gz)
			if gzErr != nil {
				t.Fatalf("decompress wire body: %v", gzErr)
			}
			if !bytes.Equal(plain, wantPlain) {
				t.Fatalf("decompressed body differs from the plain JSON path\n got: %s\nwant: %s", plain, wantPlain)
			}
		})
	}
}

// TestCodebuddyGzipBody_IndependentBuffers guards the pool: the compressed
// payload must not alias the scratch buffer, or the next request's compression
// would rewrite the bytes of the request already in flight.
func TestCodeBuddyGzipBodyIndependentBuffers(t *testing.T) {
	first, err := codebuddyGzipBody([]byte(`{"model":"a","messages":[]}`))
	if err != nil {
		t.Fatalf("first gzip: %v", err)
	}
	want := append([]byte(nil), first...)
	if _, err := codebuddyGzipBody(bytes.Repeat([]byte("x"), 4096)); err != nil {
		t.Fatalf("second gzip: %v", err)
	}
	if !bytes.Equal(first, want) {
		t.Fatal("first compressed payload was overwritten by the pooled buffer")
	}
}
