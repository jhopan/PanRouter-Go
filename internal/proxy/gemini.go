package proxy

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"panrouter/proxy/internal/constants"
	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/providers"
	"panrouter/proxy/internal/translator"
)

// ForwardGemini sends an OpenAI-format request to a Gemini-native endpoint.
// projectID is non-empty for antigravity (cloudcode-pa.googleapis.com).
func ForwardGemini(ctx context.Context, client *http.Client, cfg *providers.ProviderConfig, apiKey, bodyStr string, isStream bool, projectID, modelName string) (*http.Response, error) {
	body := []byte(bodyStr)
	// The Antigravity lane rejects stream_options on a non-streaming call; the
	// streaming case keeps it (open-sse/executors/antigravity.js transformRequest).
	if projectID != "" && cfg != nil && cfg.Format != "gemini-cli" && !isStream {
		body = antigravityStripStreamOptions(body)
	}
	if projectID != "" {
		// Body-inspecting tier resolution (upstream getModelUpstreamId): an
		// unadorned id follows the client's reasoning effort when the catalog
		// has that variant.
		modelName = translator.AntigravityTieredModel(modelName, body)
	}

	var sendBody []byte
	// geminiBody stays in scope for the refusal fallback below, which rebuilds
	// the envelope with the system instruction folded into contents[0].
	var geminiBody []byte
	if projectID != "" && translator.IsAntigravityImageModel(modelName) {
		isStream = false
		cleanModel, aspectRatio := translator.ParseImageConfig(modelName)

		var oreq struct {
			Prompt   string `json:"prompt"`
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		prompt := ""
		if err := json.Unmarshal(body, &oreq); err == nil {
			if oreq.Prompt != "" {
				prompt = oreq.Prompt
			} else {
				for _, m := range oreq.Messages {
					if s, ok := m.Content.(string); ok && s != "" {
						prompt = s
					}
				}
			}
		}

		sessID := handlerutil.GetSessionID(ctx)
		wrapped, err := translator.WrapAntigravityImageRequest(prompt, "", projectID, cleanModel, aspectRatio, sessID)
		if err != nil {
			return nil, fmt.Errorf("wrap antigravity image: %w", err)
		}
		sendBody = wrapped
	} else {
		// Translate OpenAI → Gemini native. The result is assigned to the
		// function-scoped geminiBody, never to a new local: the refusal
		// fallback below rebuilds the envelope from this value, and a
		// shadowed copy leaves it with nothing to move — which is how the
		// fallback shipped as dead code.
		translated, err := translator.TranslateOpenAIToGeminiWithSession(body, handlerutil.GetSessionID(ctx))
		if err != nil {
			return nil, fmt.Errorf("translate to Gemini: %w", err)
		}
		geminiBody = translated
		if translator.GeminiSafetyRequested(ctx) {
			geminiBody, err = translator.ApplyGeminiSafetySettings(geminiBody)
			if err != nil {
				return nil, fmt.Errorf("apply safetySettings: %w", err)
			}
		}

		// Wrap for the lane that needs an envelope.
		sendBody = geminiBody
		switch {
		case cfg.Format == "gemini-cli":
			// Cloud Code Assist: {project, model, request} with no userId,
			// requestType or requestId — the CLI's own shape.
			wrapped, err := translator.WrapForGeminiCLI(geminiBody, projectID, modelName)
			if err != nil {
				return nil, fmt.Errorf("wrap for gemini-cli: %w", err)
			}
			sendBody = wrapped
		case projectID != "":
			sessID := handlerutil.GetSessionID(ctx)
			// The Antigravity lane speaks its own thinking dialect, per model
			// family: the Gemini families carry thinkingLevel, the Claude
			// family and gpt-oss carry thinkingBudget. The shipped client sends
			// that on every agent turn, and the level/budget comes from the
			// client's own effort or the model's own tier.
			wireModel := translator.AntigravityUpstreamModel(modelName)
			geminiBody = translator.AntigravityThinkingFor(geminiBody, body, wireModel)
			wrapped, err := translator.WrapForAntigravityWithIdentity(geminiBody, projectID, modelName,
				providers.AntigravityGenericSystemPromptForToken(apiKey), sessID)
			if err != nil {
				return nil, fmt.Errorf("wrap for antigravity: %w", err)
			}
			sendBody = wrapped
		}
	}

	// Build URL
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if idx := strings.Index(baseURL, "/v1beta/openai"); idx != -1 {
		baseURL = baseURL[:idx]
	} else if idx := strings.Index(baseURL, "/v1/"); idx != -1 {
		baseURL = baseURL[:idx]
	}

	action := "generateContent"
	if isStream {
		action = "streamGenerateContent?alt=sse"
	}
	var requestURL string
	switch {
	case cfg.Format == "gemini-cli":
		// The v1internal root takes the action directly; the model rides in
		// the envelope, not the path.
		requestURL = fmt.Sprintf("%s:%s", baseURL, action)
	case projectID != "":
		requestURL = fmt.Sprintf("%s/v1internal:%s", baseURL, action)
	default:
		requestURL = fmt.Sprintf("%s/v1beta/models/%s:%s", baseURL, modelName, action)
	}

	accept := "application/json"
	if isStream {
		accept = "text/event-stream"
	}

	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + apiKey,
		"User-Agent":    providers.AntigravityUserAgent(),
		"Accept":        accept,
	}
	if isStream {
		headers["Accept-Encoding"] = "identity"
	}
	if cfg.Format == "gemini-cli" {
		// Cloud Code Assist identifies the CLI in two more headers and answers
		// 403 without them.
		headers["User-Agent"] = translator.GeminiCLIUserAgent(modelName)
		headers["X-Goog-Api-Client"] = translator.GeminiCLIApiClient
	}

	// The Antigravity envelope (projectID set, not the gemini-cli lane) gets the
	// in-place retry the reference implements in computeRetryDelay: a 429/5xx, or a
	// 400 whose body names a transient fault, is repeated here while the provider's
	// own wait is short. Parking the account instead fails a turn that clears in
	// seconds on a single-account deployment.
	antigravityLane := projectID != "" && cfg.Format != "gemini-cli"

	var req *http.Request
	var resp *http.Response
	var err error
	for attempt := 0; ; attempt++ {
		req, err = http.NewRequestWithContext(ctx, "POST", requestURL, bytes.NewReader(sendBody))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err = client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			if isTransientGeminiTransportError(err) {
				return nil, UpstreamFailure(http.StatusBadGateway, fmt.Sprintf("ForwardGemini antigravity transport: %v", err))
			}
			return nil, fmt.Errorf("upstream request: %w", err)
		}
		UnwrapIdentityGzip(req, resp)

		if !antigravityLane || resp.StatusCode == http.StatusOK ||
			attempt >= antigravityMaxTransientRetries || !antigravityRetryableStatus(resp.StatusCode) {
			break
		}
		// Only a retryable status is inspected, and only its error body: a normal
		// answer is streamed to the caller untouched.
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, constants.UpstreamErrLimit))
		resp.Body.Close()
		// A bare RESOURCE_EXHAUSTED does not clear on its own: it is the
		// request-shape verdict, and repeating the same bytes after 2s and 4s
		// only spends the client's patience — measured as a 499, where the
		// caller hung up at 5.6s before the relocation fallback below could
		// run. Hand the body back so that fallback takes the case at once.
		if readErr == nil && bareQuotaRefusalBody(errBody) {
			resp.Body = io.NopCloser(bytes.NewReader(errBody))
			break
		}
		delay := antigravityRetryDelay(resp.StatusCode, resp.Header, string(errBody), attempt+1)
		if readErr != nil || delay <= 0 {
			// No repeat: hand the body back so the caller's error path sees what the
			// provider said.
			resp.Body = io.NopCloser(bytes.NewReader(errBody))
			break
		}
		log.Warn("gemini", "antigravity transient fault, retrying the same call",
			"model", modelName, "status", resp.StatusCode, "wait", delay, "attempt", attempt+1,
			"error", string(errBody[:min(200, len(errBody))]))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	// The last resort for the same filter: the backend reads the system
	// instruction itself as the competing-harness signal, and folding it into
	// contents[0] as a user turn is what clears a refusal with no quota detail
	// (9router-go c69c20d, MoveSystemInstructionToContents). It runs once, only
	// on the Antigravity lane and only when the body still carries a system
	// instruction, so a genuine quota exhaustion — which names the quota in its
	// body — is never retried and costs no extra request.
	if antigravityLane && resp.StatusCode == http.StatusTooManyRequests && antigravityBareQuotaRefusal(resp) {
		if moved, ok := translator.MoveSystemInstructionToContents(geminiBody); ok {
			if rewrapped, werr := translator.WrapForAntigravityWithIdentity(moved, projectID, modelName,
				providers.AntigravityGenericSystemPromptForToken(apiKey), handlerutil.GetSessionID(ctx)); werr == nil {
				retryReq, rerr := http.NewRequestWithContext(ctx, "POST", requestURL, bytes.NewReader(rewrapped))
				if rerr == nil {
					for k, v := range headers {
						retryReq.Header.Set(k, v)
					}
					if retryResp, derr := client.Do(retryReq); derr == nil {
						UnwrapIdentityGzip(retryReq, retryResp)
						if retryResp.StatusCode == http.StatusOK {
							log.Warn("gemini", "systemInstruction moved into contents[0] cleared the refusal", "model", modelName)
							resp.Body.Close()
							resp = retryResp
						} else {
							// The attempt names its own outcome: without this line an
							// operator cannot tell "the fallback never ran" from "it ran
							// and did not help", and the two need different fixes.
							log.Warn("gemini", "systemInstruction relocation did not clear the refusal",
								"model", modelName, "status", retryResp.StatusCode)
							retryResp.Body.Close()
						}
					}
				}
			}
		}
	}
	if resp.StatusCode != http.StatusOK {
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, constants.UpstreamErrLimit))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("upstream returned %d and body read failed: %w", resp.StatusCode, readErr)
		}
		// Always dump 400 INVALID_ARGUMENT payloads for post-mortem (covers
		// "items.items: missing field." and other schema rejections).
		if resp.StatusCode == http.StatusBadRequest {
			_ = os.WriteFile("/tmp/panrouter-gemini-400.json", sendBody, 0644)
			preview := sendBody
			if len(preview) > 8000 {
				preview = preview[:8000]
			}
			log.Warn("gemini", "dumped 400 request to /tmp/panrouter-gemini-400.json", "model", modelName, "bytes", len(sendBody), "error", string(errBody[:min(500, len(errBody))]), "preview", string(preview))
		}
		if bytes.Contains(errBody, []byte("thought signature")) || bytes.Contains(errBody, []byte("Thought signature")) {
			_ = os.WriteFile("/tmp/panrouter-ag-debug.json", sendBody, 0644)
			log.Warn("gemini", "dumped thought-signature request to /tmp/panrouter-ag-debug.json", "model", modelName, "bytes", len(sendBody), "error", string(errBody[:min(200, len(errBody))]))
			preview := sendBody
			if len(preview) > 4000 {
				preview = preview[:4000]
			}
			log.Warn("gemini", "thought-signature preview", "preview", string(preview))

			// Retry with a known-valid default signature. Stripping makes
			// Gemini 3 complain about missing signature; replacing with the
			// default (same as Next.js backfill) gives the backend a
			// signature it accepts for the cloaked tool history.
			replaced := translator.ReplaceThoughtSignatures(sendBody, translator.DefaultThinkingSignature)
			if !bytes.Equal(replaced, sendBody) {
				log.Warn("gemini", "retrying with default thoughtSignature", "model", modelName)
				req2, err := http.NewRequestWithContext(ctx, "POST", requestURL, bytes.NewReader(replaced))
				if err == nil {
					for k, v := range headers {
						req2.Header.Set(k, v)
					}
					if resp2, err := client.Do(req2); err == nil {
						UnwrapIdentityGzip(req2, resp2)
						if resp2.StatusCode == http.StatusOK {
							if sessID := handlerutil.GetSessionID(ctx); sessID != "" {
								translator.PurgeSessionThoughtSignatures(sessID)
							}
							return resp2, nil
						}
						errBody2, _ := io.ReadAll(io.LimitReader(resp2.Body, constants.UpstreamErrLimit))
						resp2.Body.Close()
						log.Warn("gemini", "retry with default thoughtSignature also failed", "status", resp2.StatusCode, "body", string(errBody2[:min(500, len(errBody2))]))
						// Fall through to try stripping as last resort
						if bytes.Contains(errBody2, []byte("thought signature")) || bytes.Contains(errBody2, []byte("Thought signature")) {
							stripped := translator.StripThoughtSignatures(replaced)
							if !bytes.Equal(stripped, replaced) {
								log.Warn("gemini", "retrying without thoughtSignature", "model", modelName)
								req3, err := http.NewRequestWithContext(ctx, "POST", requestURL, bytes.NewReader(stripped))
								if err == nil {
									for k, v := range headers {
										req3.Header.Set(k, v)
									}
									if resp3, err := client.Do(req3); err == nil {
										UnwrapIdentityGzip(req3, resp3)
										if resp3.StatusCode == http.StatusOK {
											if sessID := handlerutil.GetSessionID(ctx); sessID != "" {
												translator.PurgeSessionThoughtSignatures(sessID)
											}
											return resp3, nil
										}
										errBody3, _ := io.ReadAll(io.LimitReader(resp3.Body, constants.UpstreamErrLimit))
										resp3.Body.Close()
										log.Warn("gemini", "retry without thoughtSignature also failed", "status", resp3.StatusCode, "body", string(errBody3[:min(500, len(errBody3))]))
										return nil, &UpstreamError{StatusCode: resp3.StatusCode, Body: errBody3, Header: resp3.Header}
									}
								}
							}
						}
						return nil, &UpstreamError{StatusCode: resp2.StatusCode, Body: errBody2, Header: resp2.Header}
					}
				}
			}
		}
		return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: errBody, Header: resp.Header}
	}
	return resp, nil
}

// antigravityBareQuotaRefusal reports whether the response is the detail-free
// refusal the Antigravity backend answers when it rejects the request's shape:
// a 429 whose body says RESOURCE_EXHAUSTED but names none of the markers a real
// quota answer carries. A body with a reset time, "Individual quota reached" or
// a rate-limit status is the account's own exhaustion and must not be retried.
func antigravityBareQuotaRefusal(resp *http.Response) bool {
	body, err := io.ReadAll(io.LimitReader(resp.Body, constants.UpstreamErrLimit))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return false
	}
	return bareQuotaRefusalBody(body)
}

// bareQuotaRefusalBody is the body-only form, used where the bytes are already
// in hand (the retry loop) so the case costs no second read.
func bareQuotaRefusalBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	text := strings.ToLower(string(body))
	if !strings.Contains(text, "resource_exhausted") {
		return false
	}
	for _, marker := range []string{"quota_exhausted", "rate_limit_exceeded", "individual quota reached", "retryinfo", "reset"} {
		if strings.Contains(text, marker) {
			return false
		}
	}
	return true
}

// isTransientGeminiTransportError reports whether a client.Do failure is a
// transient upstream fault (header timeout, HTTP/2 stream timeout, EOF)
// that the combo layer should fail over on — mirroring upstream's
// ANTIGRAVITY_TRANSIENT_ERROR_PATTERNS (/timeout/i) and DEFAULT_RETRY_CONFIG
// 502 handling. A plain error here is not UpstreamError, so the combo loop
// treats it as a terminal 502 without the RetryableStatusCodes lock/
// exclusion path and without retrying.
func isTransientGeminiTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"http2: timeo",
		"timeout awaiting response headers",
		"awaiting response headers",
		"server closed idle connection",
		"eof",
		"connection reset by peer",
		"connection refused",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
