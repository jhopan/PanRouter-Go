package proxy_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"panrouter/proxy/internal/providers"
	"panrouter/proxy/internal/proxy"
)

// bareRefusal is the answer the Antigravity backend gives when it refuses the
// request's shape: a detail-free RESOURCE_EXHAUSTED with no quota marker.
const bareRefusal = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`

// TestForwardGemini_MovesSystemInstructionToClearABareRefusal is the case that
// shipped as dead code: the fallback existed but read a shadowed variable, so a
// real refusal never reached it and the operator only saw the 429.
//
// The upstream answers the bare refusal once and then serves the retry. A
// passing run therefore proves both halves: the retry was sent at all, and it
// carried the system instruction folded into contents with no
// systemInstruction member left behind.
func TestForwardGemini_MovesSystemInstructionToClearABareRefusal(t *testing.T) {
	start := time.Now()
	var attempts int
	var retriedCarriesTheCallerText bool
	var retriedCarriesTheInstruction bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		attempts++
		// A bare refusal must not be repeated: the loop hands it straight to
		// the relocation fallback, so the second request is the relocation.
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(bareRefusal))
			return
		}
		var envelope struct {
			Request map[string]any `json:"request"`
		}
		_ = json.Unmarshal(body, &envelope)
		inner := envelope.Request

		// The lane re-stamps its own identity block on every wrap, so a
		// systemInstruction is expected — what must be gone is the CALLER's
		// text, which has to have moved into the first content turn.
		if si, ok := inner["systemInstruction"].(map[string]any); ok {
			parts, _ := si["parts"].([]any)
			for _, raw := range parts {
				if p, ok := raw.(map[string]any); ok {
					if text, _ := p["text"].(string); strings.Contains(text, "PERSONA_MARKER") {
						retriedCarriesTheCallerText = true
					}
				}
			}
		}
		if contents, ok := inner["contents"].([]any); ok && len(contents) > 0 {
			if c0, ok := contents[0].(map[string]any); ok {
				if parts, ok := c0["parts"].([]any); ok && len(parts) > 0 {
					if p0, ok := parts[0].(map[string]any); ok {
						if text, _ := p0["text"].(string); strings.Contains(text, "PERSONA_MARKER") {
							retriedCarriesTheInstruction = true
						}
					}
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}\n\n"))
	}))
	defer upstream.Close()

	cfg := providers.KnownProviders["antigravity"]
	cfg.BaseURL = upstream.URL
	// The largest system prompt the fallback exists for; the size is what makes
	// the refusal occur upstream, so the test sends a realistic one.
	prompt := "PERSONA_MARKER " + strings.Repeat("instruction line. ", 4000)
	body, _ := json.Marshal(map[string]any{
		"model":    "gemini-3.8-flash-high",
		"messages": []map[string]any{{"role": "system", "content": prompt}, {"role": "user", "content": "hai"}},
		"stream":   true,
	})

	resp, err := proxy.ForwardGemini(context.Background(), upstream.Client(), &cfg, "token-test", string(body), true, "proj-1", "gemini-3.8-flash-high")
	if err != nil {
		t.Fatalf("ForwardGemini: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		got, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		t.Fatalf("status = %d, want 200 after the relocation retry: %s", resp.StatusCode, got)
	}
	if attempts < 2 {
		t.Fatalf("upstream saw %d attempt(s); the relocation retry was never sent", attempts)
	}
	if retriedCarriesTheCallerText {
		t.Error("the retry still carries the caller's prompt in systemInstruction; the refusal would repeat")
	}
	if !retriedCarriesTheInstruction {
		t.Error("the retry dropped the caller's instructions instead of relocating them")
	}
	// Measured on the VPS: repeating the same bytes after 2s and 4s outlived the
	// caller's patience and surfaced as a 499. A bare refusal must go straight to
	// the relocation, so the whole call stays inside one short hop.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the bare refusal spent %s; it must not wait out the retry backoff", elapsed.Round(time.Millisecond))
	}
}
