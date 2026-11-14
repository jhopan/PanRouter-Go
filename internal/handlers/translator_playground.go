package handlers

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"panrouter/proxy/internal/config"
	"panrouter/proxy/internal/handlers/chat"
	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/providers"
	"panrouter/proxy/internal/proxy/executor"
	"panrouter/proxy/internal/translator"
)

// Translator playground — the port of src/app/api/translator/{load,save,
// translate,send}/route.js. Upstream ships a small editor in the dashboard that
// walks one request through the wire pipeline: the client body, the OpenAI
// intermediate, the provider-target body, then a live send. The files it edits
// are the same eight dumps the Next proxy writes under logs/translator; this
// gateway writes them under DATA_DIR/logs/translator because it has no stable
// process cwd of its own.
//
// Every response is the flat `{success, ...}` envelope upstream returns, and
// every upstream refusal keeps its exact message so an operator comparing the
// two dashboards sees the same text.

// translatorAllowedFiles is upstream's whitelist, verbatim. The names carry no
// path separator, so the allowlist is also the traversal guard.
var translatorAllowedFiles = []string{
	"1_req_client.json",
	"2_req_source.json",
	"3_req_openai.json",
	"4_req_target.json",
	"5_res_provider.txt",
	"6_res_openai.txt",
	"7_res_client.txt",
	"7_res_client.json",
}

const (
	// translatorMaxFileBytes bounds one editor file. Upstream reads and writes
	// whatever it is handed; the gateway never buffers an unbounded body.
	translatorMaxFileBytes = 8 << 20
	// translatorMaxBodyBytes bounds one JSON request to the playground routes.
	translatorMaxBodyBytes = 8 << 20
)

// TranslatorHandler serves the translator playground endpoints.
type TranslatorHandler struct {
	// Dir is the directory the load/save pair edits. Empty means
	// DATA_DIR/logs/translator, resolved per request so a test can point it at
	// a t.TempDir and an operator can move the data dir without a rebuild.
	Dir string
	// Chat resolves models and connections for the translate/send steps. Nil
	// (a handler built without a boot) makes those two steps answer 503 rather
	// than pretending to translate.
	Chat *chat.ChatHandler
}

// NewTranslatorHandler builds the playground handler.
func NewTranslatorHandler(dir string, chatH *chat.ChatHandler) *TranslatorHandler {
	return &TranslatorHandler{Dir: dir, Chat: chatH}
}

func (h *TranslatorHandler) logsDir() string {
	if h.Dir != "" {
		return h.Dir
	}
	return filepath.Join(config.ResolveDataDir(), "logs", "translator")
}

// writeTranslatorResult writes upstream's flat envelope.
func writeTranslatorResult(w http.ResponseWriter, status int, payload map[string]any) {
	handlerutil.WriteJSON(w, status, payload)
}

func writeTranslatorError(w http.ResponseWriter, status int, message string) {
	writeTranslatorResult(w, status, map[string]any{"success": false, "error": message})
}

// HandleLoad serves GET /api/translator/load?file=<name>.
func (h *TranslatorHandler) HandleLoad(w http.ResponseWriter, r *http.Request) {
	file := strings.TrimSpace(r.URL.Query().Get("file"))
	if file == "" {
		writeTranslatorError(w, http.StatusBadRequest, "File parameter required")
		return
	}
	if !translatorFileAllowed(file) {
		writeTranslatorError(w, http.StatusBadRequest, "Invalid file name")
		return
	}
	content, err := os.ReadFile(filepath.Join(h.logsDir(), file))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeTranslatorError(w, http.StatusNotFound, "File not found")
			return
		}
		writeTranslatorError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeTranslatorResult(w, http.StatusOK, map[string]any{"success": true, "content": string(content)})
}

// HandleSave serves POST /api/translator/save.
func (h *TranslatorHandler) HandleSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		File    string  `json:"file"`
		Content *string `json:"content"`
	}
	if !decodeTranslatorBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.File) == "" || body.Content == nil {
		writeTranslatorError(w, http.StatusBadRequest, "File and content required")
		return
	}
	if !translatorFileAllowed(body.File) {
		writeTranslatorError(w, http.StatusBadRequest, "Invalid file name")
		return
	}
	dir := h.logsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeTranslatorError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(dir, body.File), []byte(*body.Content), 0o644); err != nil {
		writeTranslatorError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeTranslatorResult(w, http.StatusOK, map[string]any{"success": true})
}

// translatorTranslateRequest is upstream's `{step, body}`. Body is kept raw
// because upstream accepts either the wrapper (`{body, provider, model}`) or a
// bare request body, and the step decides which one it reads.
type translatorTranslateRequest struct {
	Step int            `json:"step"`
	Body jsontext.Value `json:"body"`
}

// HandleTranslate serves POST /api/translator/translate.
func (h *TranslatorHandler) HandleTranslate(w http.ResponseWriter, r *http.Request) {
	var req translatorTranslateRequest
	if !decodeTranslatorBody(w, r, &req) {
		return
	}
	if req.Step == 0 || len(req.Body) == 0 || string(req.Body) == "null" {
		writeTranslatorError(w, http.StatusBadRequest, "Step and body required")
		return
	}
	if h.Chat == nil {
		writeTranslatorError(w, http.StatusServiceUnavailable, "translator needs a chat handler")
		return
	}

	switch req.Step {
	case 1, 2:
		// Upstream: `const clientBody = body.body || body` — the wrapper's
		// inner body wins when present, and model resolution reads whichever
		// body it picked.
		inner, innerRaw, ok := translatorInnerBody(req.Body)
		if !ok {
			writeTranslatorError(w, http.StatusInternalServerError, "request body must be a JSON object")
			return
		}
		modelStr, _ := inner["model"].(string)
		info, err := h.Chat.ResolveModel(modelStr)
		if err != nil {
			writeTranslatorError(w, http.StatusInternalServerError, err.Error())
			return
		}
		sourceFormat := translatorDetectFormat(inner)
		stream, _ := inner["stream"].(bool)
		if _, present := inner["stream"]; !present {
			stream = true
		}

		if req.Step == 1 {
			writeTranslatorResult(w, http.StatusOK, map[string]any{
				"success": true,
				"result": map[string]any{
					"provider":     info.Provider,
					"model":        info.Model,
					"sourceFormat": sourceFormat,
					"targetFormat": translatorTargetFormat(info.Provider, info.Model),
				},
			})
			return
		}

		translated, err := translatorToOpenAI(sourceFormat, innerRaw, info.Model, stream)
		if err != nil {
			writeTranslatorError(w, http.StatusInternalServerError, err.Error())
			return
		}
		var decoded any
		if err := json.Unmarshal(translated, &decoded); err != nil {
			writeTranslatorError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeTranslatorResult(w, http.StatusOK, map[string]any{
			"success": true,
			"result":  map[string]any{"body": decoded},
		})
	case 3:
		h.translateToTarget(w, r, req.Body)
	default:
		writeTranslatorError(w, http.StatusBadRequest, "Invalid step (1-3)")
	}
}

// translateToTarget is step 3: OpenAI intermediate -> provider body plus the
// URL and headers the provider would receive.
func (h *TranslatorHandler) translateToTarget(w http.ResponseWriter, r *http.Request, raw jsontext.Value) {
	// Upstream reads provider/model from the wrapper and the OpenAI body from
	// `body.body || body`; they are different objects, so they are parsed apart.
	var outer map[string]any
	if err := json.Unmarshal(raw, &outer); err != nil {
		writeTranslatorError(w, http.StatusInternalServerError, "request body must be a JSON object")
		return
	}
	inner, openaiRaw, ok := translatorInnerBody(raw)
	if !ok {
		writeTranslatorError(w, http.StatusInternalServerError, "request body must be a JSON object")
		return
	}
	provider, _ := outer["provider"].(string)
	model, _ := outer["model"].(string)
	if provider == "" || model == "" {
		writeTranslatorError(w, http.StatusBadRequest, "provider and model required")
		return
	}
	stream := true
	if v, present := inner["stream"]; present {
		if b, isBool := v.(bool); isBool {
			stream = b
		}
	}

	targetFormat := translatorTargetFormat(provider, model)
	translated, err := translatorFromOpenAI(targetFormat, openaiRaw, model, stream)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errTranslatorUnsupportedPair) {
			status = http.StatusBadRequest
		}
		writeTranslatorError(w, status, err.Error())
		return
	}

	if h.Chat.Repo == nil {
		writeTranslatorError(w, http.StatusServiceUnavailable, "translator needs a chat handler")
		return
	}
	conns, err := h.Chat.Repo.GetProviderConnections(provider, true)
	if err != nil {
		writeTranslatorError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(conns) == 0 {
		writeTranslatorError(w, http.StatusBadRequest,
			fmt.Sprintf("No active connection for provider: %s", provider))
		return
	}
	_, connData, err := h.Chat.GetBestConnection(provider, conns[0].ID, nil, model)
	if err != nil {
		writeTranslatorError(w, http.StatusBadRequest,
			fmt.Sprintf("No active connection for provider: %s", provider))
		return
	}
	cfg, err := h.Chat.GetProviderConfig(provider, connData)
	if err != nil || cfg == nil {
		writeTranslatorError(w, http.StatusInternalServerError, fmt.Sprintf("no provider config for %s", provider))
		return
	}

	var decoded any
	if err := json.Unmarshal(translated, &decoded); err != nil {
		writeTranslatorError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeTranslatorResult(w, http.StatusOK, map[string]any{
		"success": true,
		"result": map[string]any{
			"url":     cfg.BaseURL,
			"headers": translatorHeaders(provider, cfg, connData),
			"body":    decoded,
		},
	})
}

// translatorSendRequest is upstream's `{provider, model, body}`.
type translatorSendRequest struct {
	Provider string         `json:"provider"`
	Model    string         `json:"model"`
	Body     jsontext.Value `json:"body"`
}

// HandleSend serves POST /api/translator/send: one live provider call with the
// body the editor produced. The upstream response is relayed as-is, matching
// upstream (an executor writes its own status and stream).
func (h *TranslatorHandler) HandleSend(w http.ResponseWriter, r *http.Request) {
	var req translatorSendRequest
	if !decodeTranslatorBody(w, r, &req) {
		return
	}
	if req.Provider == "" || req.Model == "" || len(req.Body) == 0 || string(req.Body) == "null" {
		writeTranslatorError(w, http.StatusBadRequest, "provider, model, and body required")
		return
	}
	if h.Chat == nil || h.Chat.Repo == nil {
		writeTranslatorError(w, http.StatusServiceUnavailable, "translator needs a chat handler")
		return
	}
	conn, connData, err := h.Chat.GetBestConnection(req.Provider, "", nil, req.Model)
	if err != nil {
		writeTranslatorError(w, http.StatusBadRequest,
			fmt.Sprintf("No active connection for provider: %s", req.Provider))
		return
	}
	cfg, err := h.Chat.GetProviderConfig(req.Provider, connData)
	if err != nil || cfg == nil {
		writeTranslatorError(w, http.StatusInternalServerError, fmt.Sprintf("no provider config for %s", req.Provider))
		return
	}
	apiKey := chat.NormalizeProviderToken(req.Provider, chat.ExtractAPIKey(connData))
	if apiKey == "" {
		apiKey = cfg.DefaultAPIKey
	}
	client, err := h.Chat.GetClientForConnection(connData)
	if err != nil {
		writeTranslatorError(w, http.StatusInternalServerError, err.Error())
		return
	}

	stream := true
	var probe struct {
		Stream *bool `json:"stream"`
	}
	if json.Unmarshal(req.Body, &probe) == nil && probe.Stream != nil {
		stream = *probe.Stream
	}

	exec := executor.Get(req.Provider)
	if exec == nil {
		exec = executor.Default()
	}
	tracked := &translatorWriteTracker{ResponseWriter: w}
	execErr := exec(tracked, &executor.Request{
		Ctx:          r.Context(),
		Client:       client,
		Config:       cfg,
		Provider:     req.Provider,
		APIKey:       apiKey,
		Body:         req.Body,
		IsStream:     stream,
		ConnectionID: conn.ID,
		ConnData:     connData.ProviderSpecificData,
	})
	if execErr != nil {
		if tracked.wrote {
			// The executor already sent a status/body; appending JSON would
			// corrupt it. The failure surfaces the way it does for a real
			// request: whatever the executor wrote stands.
			log.Warn("translator", "send failed after response started", "provider", req.Provider, "error", execErr)
			return
		}
		writeTranslatorError(w, http.StatusInternalServerError, execErr.Error())
	}
}

// translatorWriteTracker records whether the executor started a response so a
// late error does not append a JSON envelope to an already-streamed body.
type translatorWriteTracker struct {
	http.ResponseWriter
	wrote bool
}

func (t *translatorWriteTracker) WriteHeader(status int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(status)
}

func (t *translatorWriteTracker) Write(p []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(p)
}

func (t *translatorWriteTracker) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// decodeTranslatorBody reads a bounded JSON body. Upstream tolerates a missing
// body (`req.json().catch(() => ({}))`), so an empty body decodes to the zero
// value rather than a 400.
func decodeTranslatorBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, translatorMaxBodyBytes+1))
	if err != nil {
		writeTranslatorError(w, http.StatusBadRequest, "failed to read request body")
		return false
	}
	if len(raw) > translatorMaxBodyBytes {
		writeTranslatorError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return false
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		// Upstream's `await request.json()` throws and the route's catch turns
		// that into a 500 with the parser message.
		writeTranslatorError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	return true
}

func translatorFileAllowed(name string) bool {
	for _, allowed := range translatorAllowedFiles {
		if name == allowed {
			return true
		}
	}
	return false
}

// translatorInnerBody reproduces upstream's `body.body || body`: the wrapper's
// inner object when the payload has one, otherwise the payload itself. It
// returns the selected object and its exact bytes, so a converter sees the
// request the gateway would have forwarded rather than a re-marshal.
func translatorInnerBody(raw jsontext.Value) (map[string]any, []byte, bool) {
	var outer map[string]any
	if err := json.Unmarshal(raw, &outer); err != nil {
		return nil, nil, false
	}
	inner, ok := outer["body"].(map[string]any)
	if !ok {
		return outer, raw, true
	}
	innerRaw, err := json.Marshal(inner)
	if err != nil {
		return nil, nil, false
	}
	return inner, innerRaw, true
}

// translatorDetectFormat is the port of open-sse/services/provider.js
// detectFormat. The order matters: Responses and Gemini bodies are recognised
// before the OpenAI/Claude split, and a message array alone is not enough to
// call a body Claude.
func translatorDetectFormat(body map[string]any) string {
	if input, ok := body["input"]; ok && body["messages"] == nil {
		switch input.(type) {
		case []any, string:
			return providers.FormatOpenAIResponses
		}
	}
	if request, ok := body["request"].(map[string]any); ok {
		if _, has := request["contents"]; has && body["userAgent"] == "antigravity" {
			return "antigravity"
		}
	}
	if contents, ok := body["contents"].([]any); ok && contents != nil {
		return providers.FormatGemini
	}
	for _, key := range []string{
		"stream_options", "response_format", "logprobs", "top_logprobs", "n",
		"presence_penalty", "frequency_penalty", "logit_bias", "user",
	} {
		if _, has := body[key]; has {
			return providers.FormatOpenAI
		}
	}
	if messages, ok := body["messages"].([]any); ok && len(messages) > 0 {
		if first, ok := messages[0].(map[string]any); ok {
			if content, ok := first["content"].([]any); ok && len(content) > 0 {
				if firstPart, ok := content[0].(map[string]any); ok {
					model, _ := body["model"].(string)
					if firstPart["type"] == "text" && !strings.Contains(model, "/") {
						if _, has := body["system"]; has {
							return providers.FormatClaude
						}
						if _, has := body["anthropic_version"]; has {
							return providers.FormatClaude
						}
						for _, partRaw := range content {
							part, ok := partRaw.(map[string]any)
							if !ok {
								continue
							}
							if part["type"] == "image" {
								if source, ok := part["source"].(map[string]any); ok && source["type"] == "base64" {
									return providers.FormatClaude
								}
							}
							if part["type"] == "image_url" {
								return providers.FormatOpenAI
							}
							if part["type"] == "tool_use" || part["type"] == "tool_result" {
								return providers.FormatClaude
							}
						}
					}
				}
			}
			if _, has := body["system"]; has {
				return providers.FormatClaude
			}
			if _, has := body["anthropic_version"]; has {
				return providers.FormatClaude
			}
		}
	}
	return providers.FormatOpenAI
}

// translatorTargetFormat mirrors getTargetFormat: a model's declared target
// lane wins, then the provider's registry wire, then Chat Completions.
func translatorTargetFormat(provider, model string) string {
	if formats, ok := providers.GetModelFormats(provider, model); ok && formats.TargetFormat != "" {
		return formats.TargetFormat
	}
	// Anthropic's own two ids are the Messages lane by name; their registry
	// entries point at /v1/messages without declaring Format.
	if provider == "claude" || provider == "anthropic" {
		return providers.FormatClaude
	}
	if cfg, ok := providers.KnownProviders[provider]; ok {
		switch cfg.Format {
		case providers.FormatClaude:
			return providers.FormatClaude
		case "gemini-native", "gemini-cli":
			return providers.FormatGemini
		}
	}
	if strings.HasPrefix(provider, providers.CustomAnthropicPrefix) {
		return providers.FormatClaude
	}
	return providers.FormatOpenAI
}

// errTranslatorUnsupportedPair marks a source→target pair this port has no
// converter for. It is a deliberate 400, not a 500: the editor asked for a
// translation the gateway cannot produce, and the message names both formats.
var errTranslatorUnsupportedPair = errors.New("unsupported translation pair")

// translatorToOpenAI is the first half of upstream's translateRequest: source
// format -> OpenAI intermediate.
func translatorToOpenAI(source string, raw []byte, model string, stream bool) ([]byte, error) {
	switch source {
	case "", providers.FormatOpenAI:
		return raw, nil
	case providers.FormatClaude:
		return translator.TranslateClaudeToOpenAI(raw)
	case providers.FormatGemini:
		return translator.GeminiClientToChatRequest(raw, model, stream)
	case providers.FormatOpenAIResponses:
		result, err := translator.ResponsesToChatRequest(raw)
		if err != nil {
			return nil, err
		}
		return result.Body, nil
	default:
		return nil, fmt.Errorf("%w: %s -> %s", errTranslatorUnsupportedPair, source, providers.FormatOpenAI)
	}
}

// translatorFromOpenAI is the second half: OpenAI intermediate -> target wire.
func translatorFromOpenAI(target string, raw []byte, model string, stream bool) ([]byte, error) {
	switch target {
	case "", providers.FormatOpenAI:
		return raw, nil
	case providers.FormatGemini:
		return translator.TranslateOpenAIToGemini(raw)
	default:
		return nil, fmt.Errorf("%w: %s -> %s", errTranslatorUnsupportedPair, providers.FormatOpenAI, target)
	}
}

// translatorHeaders builds the request headers the provider would receive. It
// is the playground's read-only view: the registry's static headers, the
// operator's overrides, and the credential in the scheme that provider expects.
func translatorHeaders(provider string, cfg *providers.ProviderConfig, connData *chat.ConnectionData) map[string]string {
	headers := make(map[string]string, len(cfg.StaticHeaders)+len(cfg.HeaderOverrides)+1)
	for k, v := range cfg.StaticHeaders {
		headers[k] = v
	}
	for k, v := range cfg.HeaderOverrides {
		headers[k] = v
	}
	if cfg.NoAuth || cfg.AuthHeader == "" || connData == nil {
		return headers
	}
	token := chat.NormalizeProviderToken(provider, chat.ExtractAPIKey(connData))
	if token == "" {
		token = cfg.DefaultAPIKey
	}
	if token == "" {
		return headers
	}
	if cfg.AuthScheme == "raw" {
		headers[cfg.AuthHeader] = token
	} else {
		headers[cfg.AuthHeader] = "Bearer " + token
	}
	return headers
}
