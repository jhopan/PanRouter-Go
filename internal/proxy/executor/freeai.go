package executor

import (
	json "encoding/json/v2"
	"net/http"
	"strings"
)

// Free.ai splits its endpoints by model family: chat and code live on the
// native /v1/chat/ path, vision models (qwen-vl, qwen25-vl, moondream2) on the
// OpenAI-compatible /v1/chat/completions path. One provider id, one API key,
// both endpoint sets — ported from upstream open-sse/executors/freeai.js, which
// switches the URL per model so an operator needs no second connection (the
// legacy `freeai-vl` entry exists only for connections created before the
// merge).
const (
	freeAIChatBase   = "https://api.free.ai/v1/chat/"
	freeAIVisionBase = "https://api.free.ai/v1/chat/completions"
)

// freeAIVisionModels is upstream FREEAI_VISION_MODELS.
var freeAIVisionModels = map[string]bool{
	"qwen-vl":    true,
	"qwen25-vl":  true,
	"moondream2": true,
}

// ForwardFreeAI picks the endpoint family the requested model answers on and
// forwards through the shared OpenAI passthrough.
//
// The rewrite only happens while the connection is still pointed at the
// registry default: getProviderConfig replaces BaseURL with the connection's
// providerSpecificData.baseUrl when one is set, and upstream honours that
// runtime transport override ahead of the per-model split
// (executors/freeai.js: if credentials.runtimeTransport.baseUrl -> super).
func ForwardFreeAI(w http.ResponseWriter, req *Request) error {
	if req.Config != nil && req.Config.BaseURL == freeAIChatBase &&
		freeAIVisionModels[freeAIRequestModel(req.Body)] {
		req.Config.BaseURL = freeAIVisionBase
	}
	return ForwardOpenAI(w, req)
}

// freeAIRequestModel returns the model id the body asks for, with any provider
// prefix removed — upstream compares `String(model).split("/").pop()` against
// its vision set, so `fai/qwen-vl` and `qwen-vl` both match.
func freeAIRequestModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	model := strings.TrimSpace(probe.Model)
	if idx := strings.LastIndex(model, "/"); idx >= 0 {
		model = model[idx+1:]
	}
	return model
}
