package executor

import (
	"bytes"
	"compress/gzip"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"panrouter/proxy/internal/constants"
	"panrouter/proxy/internal/providers"
	"panrouter/proxy/internal/proxy"

	"github.com/google/uuid"
)

const codebuddySystemPrompt = "You are CodeBuddy Code."

// codebuddyJSONOpts sorts JSON object members by name.
//
// The gzipped body skips proxy.canonicalChatBody (it cannot parse compressed
// bytes), so the executor sorts the members itself. Without it the same logical
// request would reach the upstream with its keys in Go's randomized map order,
// and a prefix-based prompt cache upstream would miss on every turn after the
// first — the property upstream's JS keeps by construction
// (internal/proxy/deterministic_body.go).
var codebuddyJSONOpts = json.Deterministic(true)

var codebuddyRateLimitRegex = regexp.MustCompile(`(?i)超出频率限制|frequency limit|限额`)

// ForwardCodebuddyCN forwards to Tencent CodeBuddy with force-stream
// and reasoning_summary injection.
//
// CodeBuddy is OpenAI-compatible but rejects non-stream chat requests
// (HTTP 400, code 11101 "Non-stream chat request is currently not supported").
// This executor forces stream=true in the request body, mirroring the
// JS version's CodeBuddyExecutor.transformRequest behaviour.
//
// It also handles reasoning_effort:
//   - "none"/"off" → stripped (CodeBuddy gateway has no "none")
//   - any other value → injects reasoning_summary="auto" so CodeBuddy
//     surfaces the model's reasoning output.
func ForwardCodebuddyCN(w http.ResponseWriter, req *Request) error {
	// The entry point *is* the lane: a caller that reached this function wants
	// the CN identity even when the request carries no provider name (the
	// dashboard's own test dispatch does not set one).
	if req != nil && req.Provider == "" {
		req.Provider = "codebuddy-cn"
	}
	body, err := transformCodebuddyBodyFor(req.Provider, req.Body)
	if err != nil {
		return fmt.Errorf("transform codebuddy body: %w", err)
	}

	ctx := req.Ctx
	cfg := req.Config
	encoded := body
	if cfg != nil {
		merged := make(map[string]string, len(cfg.StaticHeaders)+25)
		for k, v := range cfg.StaticHeaders {
			merged[k] = v
		}
		delete(merged, "x-requested-with")
		delete(merged, "x-codebuddy-request")

		// The header builder reads the opening user message to derive the
		// conversation id, so it runs while the body is still JSON.
		for k, v := range codebuddyHeaders(req, cfg, body) {
			merged[k] = v
		}
		cfgCopy := *cfg
		cfgCopy.StaticHeaders = merged
		cfg = &cfgCopy

		// The CLI gzips the chat body (upstream encodeRequestBody: body →
		// gzipSync, Content-Encoding: gzip). Compressing here, once per outgoing
		// request, also lets the transport derive Content-Length from the
		// compressed byte count instead of the JSON length.
		compressed, err := codebuddyGzipBody(body)
		if err != nil {
			return err
		}
		encoded = compressed
	}

	// Always forward as stream — CodeBuddy rejects stream=false
	resp, err := proxy.ForwardOpenAI(ctx, req.Client, cfg, req.APIKey, encoded, true)
	if err != nil {
		var ue *proxy.UpstreamError
		if errors.As(err, &ue) && isCodebuddyRateLimit(ue.Body) {
			ue.StatusCode = http.StatusTooManyRequests
		}
		return fmt.Errorf("ForwardCodebuddyCN upstream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	forwardUpstreamResponseHeaders(w, resp.Header)

	if req.IsStream {
		if !isEventStreamResponse(resp.Header) {
			// Head sniff instead of a whole-body read: a CodeBuddy stream
			// that arrives with a JSON content-type keeps streaming frame by
			// frame, and only a genuine document is read whole.
			head := sniffStreamHead(resp.Body)
			if proxy.LooksLikeSSE(head) {
				// The upstream body stays the Closer: the stall watchdog ends
				// a stuck stream by closing the reader, so a NopCloser here
				// would leave it unable to.
				resp.Body = replayBody{Reader: io.MultiReader(bytes.NewReader(head), resp.Body), Closer: resp.Body}
			} else {
				rest, rerr := io.ReadAll(io.LimitReader(resp.Body, constants.MaxUpstreamBodyBytes-int64(len(head))))
				if rerr != nil {
					return fmt.Errorf("read codebuddy upstream body: %w", rerr)
				}
				data := append(head, rest...)
				if isCodebuddyRateLimit(data) {
					return proxy.UpstreamFailure(http.StatusTooManyRequests, "CodeBuddy frequency limit (6004)")
				}
				return jsonResponse(req.Ctx, w, bytes.NewReader(data), req.TranslateResp, req.ResponseBuf)
			}
		}
		stallReader := proxy.NewStallReaderWithContext(req.Ctx, resp.Body, proxy.ResolveStallBudgets(req.Config), "codebuddy-cn")
		defer func() { _ = stallReader.Close() }() // stops the shutdown watcher + stall timer
		return execSSEStream(w, stallReader, req)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, constants.MaxUpstreamBodyBytes))
	if err != nil {
		return fmt.Errorf("read codebuddy response: %w", err)
	}
	if converted, ok := sseToOpenAIJSON(data); ok {
		data = converted
	} else if isCodebuddyRateLimit(data) {
		return proxy.UpstreamFailure(http.StatusTooManyRequests, "CodeBuddy frequency limit (6004)")
	}
	return jsonResponse(req.Ctx, w, bytes.NewReader(data), req.TranslateResp, req.ResponseBuf)
}

func copyMap(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func isCodebuddyRateLimit(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var data struct {
		Code    int    `json:"code"`
		Msg     string `json:"msg"`
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return false
	}
	msg := data.Msg
	if msg == "" {
		msg = data.Message
	}
	if msg == "" {
		msg = data.Error.Message
	}
	return data.Code == 6004 || codebuddyRateLimitRegex.MatchString(msg)
}

// transformCodebuddyBody applies the CN lane's body rules.
func transformCodebuddyBody(body []byte) ([]byte, error) {
	return transformCodebuddyBodyFor("codebuddy-cn", body)
}

// transformCodebuddyBodyFor applies the body rules of one CodeBuddy lane.
//
// Both lanes force stream=true (the gateway answers 11101 "Non-stream chat
// request is currently not supported" otherwise) and both mirror the CLI's
// reasoning parameters. They differ in how the system prompt is handled: the CN
// gateway rejects agent-harness prompts outright, while the intl gateway requires
// exactly ONE leading system message and takes user content as typed blocks.
func transformCodebuddyBodyFor(provider string, body []byte) ([]byte, error) {
	var reqMap map[string]any
	if err := json.Unmarshal(body, &reqMap); err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}

	reqMap["stream"] = true
	applyCodebuddyReasoningParams(reqMap)

	if provider == "codebuddy-intl" || provider == "codebuddy" || provider == "cbai" {
		foldCodebuddyIntlMessages(reqMap)
	} else {
		neutraliseCodebuddyCNSystemMessages(reqMap)
	}

	return json.Marshal(reqMap, codebuddyJSONOpts)
}

// withCodebuddyReasoning renames a history turn's reasoning_content to the
// `reasoning` field CodeBuddy reads, so a replayed turn keeps the reasoning the
// client already received. The field is dropped rather than duplicated: sending
// both spellings is one more thing the gateway has to reconcile.
func withCodebuddyReasoning(message map[string]any) map[string]any {
	reasoning, _ := message["reasoning_content"].(string)
	if reasoning == "" {
		return message
	}
	renamed := copyMap(message)
	renamed["reasoning"] = reasoning
	delete(renamed, "reasoning_content")
	return renamed
}

// applyCodebuddyReasoningParams mirrors the CLI's OpenAI-style reasoning params.
// The gateway only surfaces model reasoning when the request carries
// reasoning_effort + reasoning_summary:"auto"; a request with neither gets
// reasoning_effort:"medium" forced, which trips its content filter (#2071), so
// nothing is added when the client asked for no reasoning.
func applyCodebuddyReasoningParams(reqMap map[string]any) {
	effort, ok := reqMap["reasoning_effort"].(string)
	if !ok {
		return
	}
	switch effort {
	case "none", "off":
		// The gateway has no "none" value: omit the field.
		delete(reqMap, "reasoning_effort")
	default:
		reqMap["reasoning_summary"] = "auto"
	}
}

// neutraliseCodebuddyCNSystemMessages replaces an agent-harness system prompt with
// the neutral one, preserving the content shape the client sent (string or typed
// blocks). The CN gateway answers 11128 to agent identity strings and restricts
// the account (11140) on repetition.
//
// The length catch-all exists because the pattern cannot know every harness
// prompt: a system prompt past the limit is treated as one.
func neutraliseCodebuddyCNSystemMessages(reqMap map[string]any) {
	messages, ok := reqMap["messages"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		if role, _ := message["role"].(string); role != "system" {
			out = append(out, withCodebuddyReasoning(message))
			continue
		}
		text := flattenCodebuddySystemContent(message["content"])
		if text == "" {
			out = append(out, message)
			continue
		}
		if len(text) <= codebuddySystemPromptLengthLimit && !codebuddyAgentIdentityPattern.MatchString(text) {
			out = append(out, message)
			continue
		}
		replaced := copyMap(message)
		if _, isString := message["content"].(string); isString {
			replaced["content"] = codebuddyNeutralSystemPrompt
		} else {
			replaced["content"] = []any{map[string]any{"type": "text", "text": codebuddyNeutralSystemPrompt}}
		}
		out = append(out, replaced)
	}
	reqMap["messages"] = out
}

// foldCodebuddyIntlMessages folds every system/developer message into ONE leading
// system message and turns string user content into typed blocks, which is the
// shape the intl gateway requires (it answers 11128 to anything else).
//
// The client's own system text is preserved — only a third-party agent identity is
// replaced. Dropping the prompt instead lost every user and project instruction on
// every request; the default below is used only when the client sent none.
func foldCodebuddyIntlMessages(reqMap map[string]any) {
	messages, _ := reqMap["messages"].([]any)

	clientPrompts := make([]string, 0, 2)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		text := strings.TrimSpace(neutraliseCodebuddySystemText(flattenCodebuddySystemContent(message["content"])))
		if text != "" {
			clientPrompts = append(clientPrompts, text)
		}
	}
	leading := codebuddySystemPrompt
	if len(clientPrompts) > 0 {
		leading = strings.Join(clientPrompts, "\n\n")
	}

	out := make([]any, 0, len(messages)+1)
	out = append(out, map[string]any{"role": "system", "content": leading})
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		role, _ := message["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		forwarded := withCodebuddyReasoning(message)
		role, _ = forwarded["role"].(string)
		if role == "user" {
			if text, isString := forwarded["content"].(string); isString {
				typed := copyMap(forwarded)
				typed["content"] = []any{map[string]any{"type": "text", "text": text}}
				out = append(out, typed)
				continue
			}
		}
		out = append(out, forwarded)
	}
	reqMap["messages"] = out
}

func codebuddyHeaders(req *Request, cfg *providers.ProviderConfig, body []byte) map[string]string {
	reqID := strings.ReplaceAll(uuid.NewString(), "-", "")
	convReqID := strings.ReplaceAll(uuid.NewString(), "-", "")

	// The conversation key, in upstream's order (codebuddy-intl.js buildHeaders):
	// the body's own session fields first — the CLI sends session_id — then the
	// request's session id, then the connection's, and only then the opening user
	// message. The key decides the conversation id, so a client that names its
	// session must not be given an id derived from its first message.
	sessionKey := codebuddyBodySessionKey(body)
	if sessionKey == "" {
		sessionKey = req.SessionID
	}
	if sessionKey == "" && req.ConnData != nil {
		if m, ok := req.ConnData["sessionId"].(string); ok {
			sessionKey = m
		}
	}
	if sessionKey == "" {
		sessionKey = firstUserText(body)
	}
	// A time-based id pinned per conversation, like the CLI's: a content hash or a
	// random id would carry neither the opening time nor a stable value across the
	// turns of one conversation.
	var convID string
	if sessionKey != "" {
		convID = codebuddyStableConversationID("codebuddy:" + sessionKey)
	} else {
		convID = codebuddyPerTurnID()
	}

	domain := "www.codebuddy.ai"
	if req.ConnData != nil {
		if d, ok := req.ConnData["domain"].(string); ok && d != "" {
			domain = d
		}
	}
	if domain == "www.codebuddy.ai" && cfg != nil && (strings.Contains(cfg.BaseURL, "tencent.com") || strings.Contains(cfg.BaseURL, "copilot.")) {
		domain = "copilot.tencent.com"
	}
	if req != nil && req.Provider == "codebuddy-cn" && domain == "www.codebuddy.ai" {
		domain = "www.codebuddy.cn"
	}

	// The intl fingerprint is opt-in by provider name; the helper is also used
	// for conversation ids alone, where the classic (CN) shapes apply.
	intl := req != nil && (req.Provider == "codebuddy-intl" || req.Provider == "codebuddy" || req.Provider == "cbai")
	// One CLI build for both lanes: the gateway compares whatever the client
	// reports against the build it currently serves, and a lane pinned to an
	// older build reads as a parked client (AGENTS.md §1). Every version on the
	// wire therefore comes from providers.CodeBuddy* — no literal here.
	h := map[string]string{
		"Content-Type": "application/json; charset=utf-8",
		// The CLI asks for JSON, not an event stream, and still receives SSE
		// back (upstream buildHeaders: "Accept: the CLI asks for JSON, not an
		// event stream. The base executor sets text/event-stream for every
		// streaming call.").
		"Accept": "application/json",
		// The CLI gzips the chat body. ForwardCodebuddyCN compresses it; the
		// byte count of that payload is what the transport stamps as
		// Content-Length.
		"Content-Encoding":            "gzip",
		"User-Agent":                  providers.CodeBuddyUserAgent,
		"X-Product":                   "SaaS",
		"X-IDE-Type":                  "CLI",
		"X-IDE-Name":                  "CLI",
		"X-IDE-Version":               providers.CodeBuddyCLIVersion,
		"X-Private-Data":              "false",
		"X-Requested-With":            "XMLHttpRequest",
		"X-CodeBuddy-Request":         "1",
		"X-Domain":                    domain,
		"X-Stainless-Lang":            "js",
		"X-Stainless-Package-Version": providers.CodeBuddySDKVersion,
		"X-Stainless-OS":              codebuddyStainlessOS(),
		"X-Stainless-Arch":            codebuddyStainlessArch(),
		"X-Stainless-Runtime":         "node",
		"X-Stainless-Runtime-Version": providers.CodeBuddyNodeVersion,
		"X-Stainless-Retry-Count":     "0",
		"X-Request-ID":                reqID,
		"X-Conversation-ID":           convID,
		"X-Conversation-Request-ID":   convReqID,
		"X-Conversation-Message-ID":   reqID,
		// Agent identity: the CLI describes every chat turn as a "craft" intent
		// for a "conversation" purpose, attributed to the MAIN agent type, with
		// a non-IDE runtime context.
		"X-Agent-Intent":  "craft",
		"X-Agent-Purpose": "conversation",
		"X-Agent-Type":    "main",
		"X-Agent-Runtime": "other",
	}
	// X-User-Id is the token's `sub` claim: the gateway uses it to tie the
	// request to the account behind the bearer. Omitted when the token carries
	// no account id (upstream userIdFromCredentials returns null there too).
	if sub := codebuddyUserID(req); sub != "" {
		h["X-User-Id"] = sub
	}
	if intl {
		// The official intl CLI sends a uuid request id. X-App,
		// X-Product-Version and X-Stainless-Helper-Method are never sent by the
		// official client on either lane — sending a header the real CLI does
		// not send is itself the tell.
		// One per-turn uuid for the whole family: the CLI reuses the same value for
		// the conversation request id, the message id, the request id and the root
		// request id, and only the baggage carries it without dashes.
		turnID := codebuddyPerTurnID()
		h["X-Conversation-Request-ID"] = turnID
		h["X-Request-ID"] = turnID
		h["X-Conversation-Message-ID"] = turnID
		h["X-Root-Request-ID"] = turnID

		// ── OpenTelemetry distributed tracing ─────────────────────────────
		// The CLI sends Zipkin B3 + W3C traceparent on every chat call. The ids
		// are per-request randoms and are not validated server-side, but their
		// absence is one more thing a non-SDK caller does not have.
		traceID := strings.ReplaceAll(uuid.NewString(), "-", "")
		spanID := traceID[:16]
		parentSpanID := traceID[16:]
		h["traceparent"] = "00-" + traceID + "-" + spanID + "-01"
		h["b3"] = traceID + "-" + spanID + "-1-" + parentSpanID
		h["X-B3-TraceId"] = traceID
		h["X-B3-SpanId"] = spanID
		h["X-B3-ParentSpanId"] = parentSpanID
		h["X-B3-Sampled"] = "1"
		h["X-Trace-ID"] = traceID
		// baggage carries the stable session id and the per-turn id in the CLI's
		// format: codebuddy.session_id=<uuid>,codebuddy.conversation_request_id=<hex>
		h["baggage"] = "codebuddy.session_id=" + h["X-Conversation-ID"] +
			",codebuddy.conversation_request_id=" + strings.ReplaceAll(h["X-Conversation-Message-ID"], "-", "")
	}
	if req != nil && strings.HasPrefix(req.APIKey, "ck_") {
		h["Authorization"] = "Bearer " + req.APIKey
		h["X-API-Key"] = req.APIKey
		delete(h, "X-Domain")
		h["X-User-Id"] = "anonymous_" + last8(req.APIKey)
		h["X-IDE-Type"] = "WorkBuddy"
		h["X-Agent-Runtime"] = "local-agent"
	}
	return h
}

// codebuddyUserID reads the account id X-User-Id carries: the `sub` claim of
// the bearer token the request is sent with. The parse is memoized per token in
// the providers package, so a turn does not decode its JWT again.
func codebuddyUserID(req *Request) string {
	if req == nil {
		return ""
	}
	return providers.CodeBuddyUserIDFromToken(req.APIKey)
}

// codebuddyGzipScratch is one reusable compressor plus its output buffer.
//
// A fresh gzip.Writer allocates a 32 KiB deflate window and its own state; the
// chat path compresses every CodeBuddy turn, so both are pooled rather than
// re-allocated per request.
type codebuddyGzipScratch struct {
	w   *gzip.Writer
	buf bytes.Buffer
}

var codebuddyGzipPool = sync.Pool{
	New: func() any {
		s := &codebuddyGzipScratch{}
		s.w = gzip.NewWriter(&s.buf)
		return s
	},
}

// codebuddyGzipBody compresses the request body the way the official CLI does
// (upstream `encodeRequestBody`: `gzipSync(Buffer.from(bodyStr, "utf8"))` with
// `Content-Encoding: gzip`).
//
// The returned bytes do not alias the pooled buffer: the buffer is reused by
// the next request, so the compressed payload is copied out before the scratch
// struct goes back to the pool.
func codebuddyGzipBody(body []byte) ([]byte, error) {
	s := codebuddyGzipPool.Get().(*codebuddyGzipScratch)
	defer codebuddyGzipPool.Put(s)

	s.buf.Reset()
	s.w.Reset(&s.buf)
	if _, err := s.w.Write(body); err != nil {
		return nil, fmt.Errorf("gzip codebuddy body: %w", err)
	}
	if err := s.w.Close(); err != nil {
		return nil, fmt.Errorf("gzip codebuddy body: %w", err)
	}
	return bytes.Clone(s.buf.Bytes()), nil
}

func last8(s string) string {
	if len(s) > 8 {
		return s[len(s)-8:]
	}
	return s
}

func codebuddyConversationHeaders(req *Request, body []byte) map[string]string {
	return codebuddyHeaders(req, nil, body)
}

// codebuddyBodySessionKey reads the session identifier the client put in the body,
// under any of the spellings the CLI and its wrappers use.
func codebuddyBodySessionKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	for _, key := range []string{"session_id", "prompt_cache_key", "conversation_id"} {
		if value, ok := parsed[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// firstUserText extracts the opening user message text from an OpenAI-shaped
// body (string or typed blocks) to use as a conversation-key fallback.
func firstUserText(body []byte) string {
	var reqMap map[string]any
	if err := json.Unmarshal(body, &reqMap); err != nil {
		return ""
	}
	msgs, _ := reqMap["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role != "user" {
			continue
		}
		switch content := m["content"].(type) {
		case string:
			return content
		case []any:
			var sb strings.Builder
			for _, blk := range content {
				if bm, ok := blk.(map[string]any); ok {
					if t, ok := bm["text"].(string); ok {
						sb.WriteString(t)
					}
				}
			}
			return sb.String()
		}
		return ""
	}
	return ""
}

// sseToOpenAIJSON re-aggregates OpenAI chat-completions SSE chunks into a
// single chat.completion JSON object. Returns (nil, false) when raw is not
// SSE-shaped (e.g. an upstream error JSON that should pass through).
func sseToOpenAIJSON(raw []byte) ([]byte, bool) {
	var chunks []map[string]any
	var streamErr map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(trimmed[5:])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if e, ok := chunk["error"]; ok {
			streamErr, _ = e.(map[string]any)
			continue
		}
		chunks = append(chunks, chunk)
	}
	if streamErr != nil {
		b, _ := json.Marshal(map[string]any{"error": streamErr})
		return b, true
	}
	if len(chunks) == 0 {
		return nil, false
	}

	var contentParts, reasoningParts []string
	toolCallIdx := map[int]map[string]any{}
	var toolCalls []map[string]any
	finishReason := "stop"
	var usage any
	var first map[string]any

	for _, chunk := range chunks {
		if first == nil {
			first = chunk
		}
		// usage is read before the choices guard on purpose. An upstream told
		// stream_options.include_usage sends a FINAL chunk carrying usage and an
		// empty choices array; reading usage after the guard skipped exactly
		// that chunk and threw the counts away, so a caller that asked for them
		// still got none. Reading it first also keeps the last write winning,
		// which is the trailing summary chunk when there is one.
		if u, ok := chunk["usage"]; ok {
			usage = u
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if c, ok := delta["content"].(string); ok && c != "" {
			contentParts = append(contentParts, c)
		}
		// Vendor aliases (`reasoning`, `reasoning_details[]`) count as
		// reasoning too: without this an alias lane's thinking is silently
		// dropped before the empty-content rescue below could keep it.
		if r := reasoningTextFromDeltaMap(delta); r != "" {
			reasoningParts = append(reasoningParts, r)
		}
		if fr, ok := choice["finish_reason"].(string); ok {
			finishReason = fr
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tcAny := range tcs {
				tc, _ := tcAny.(map[string]any)
				idx, _ := tc["index"].(float64)
				entry, ok := toolCallIdx[int(idx)]
				if !ok {
					entry = map[string]any{
						"id":       "",
						"type":     "function",
						"function": map[string]any{"name": "", "arguments": ""},
					}
					toolCallIdx[int(idx)] = entry
					toolCalls = append(toolCalls, entry)
				}
				if id, ok := tc["id"].(string); ok && id != "" {
					entry["id"] = id
				}
				if fn, ok := tc["function"].(map[string]any); ok {
					fEntry, _ := entry["function"].(map[string]any)
					if n, ok := fn["name"].(string); ok && n != "" {
						if existing, _ := fEntry["name"].(string); existing == "" {
							fEntry["name"] = n
						} else {
							fEntry["name"] = existing + n
						}
					}
					if a, ok := fn["arguments"].(string); ok && a != "" {
						existing, _ := fEntry["arguments"].(string)
						fEntry["arguments"] = existing + a
					}
				}
			}
		}
	}

	msg := map[string]any{"role": "assistant"}
	content := strings.Join(contentParts, "")
	if content == "" {
		if len(toolCalls) > 0 {
			msg["content"] = nil
		} else {
			msg["content"] = ""
		}
	} else {
		msg["content"] = content
	}
	var validToolCalls []map[string]any
	for _, tc := range toolCalls {
		if fn, ok := tc["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok && strings.TrimSpace(n) != "" {
				validToolCalls = append(validToolCalls, tc)
			}
		}
	}
	if len(validToolCalls) > 0 {
		msg["tool_calls"] = validToolCalls
	}
	if len(reasoningParts) > 0 {
		msg["reasoning_content"] = strings.Join(reasoningParts, "")
	}

	result := map[string]any{
		"id":      first["id"],
		"object":  "chat.completion",
		"created": first["created"],
		"model":   first["model"],
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	if result["id"] == nil {
		result["id"] = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if result["created"] == nil {
		result["created"] = time.Now().Unix()
	}
	if result["model"] == nil {
		result["model"] = ""
	}
	b, err := json.Marshal(result)
	if err != nil {
		return nil, false
	}
	return b, true
}

// codebuddyStainlessArch maps the host architecture the way the official
// CLI's Stainless runtime reports it (upstream mapStainlessArch).
func codebuddyStainlessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "x86"
	default:
		return "other::" + runtime.GOARCH
	}
}

// codebuddyStainlessOS maps the host OS the way the CLI reports it.
func codebuddyStainlessOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "freebsd":
		return "FreeBSD"
	default:
		return "Other::" + runtime.GOOS
	}
}
