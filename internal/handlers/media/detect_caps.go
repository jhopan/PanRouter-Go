package media

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/providers"
)

// Port of upstream src/app/api/models/detect-caps/route.js.
//
// The endpoint answers two questions the model editor asks: does this model
// see images, and does it reason. A heuristic pass (name patterns + the
// capability tables in internal/providers) answers both without a request; a
// live probe then confirms or corrects it, through the gateway's own chat lane
// so the verdict is what a real request would see.
//
// Two behaviours are load-bearing and copied from upstream:
//   - A heuristic "vision: true" is trusted, so no image is ever re-probed. The
//     table entries (deepseek, claude, ...) are data, and one short reply is not
//     evidence against them.
//   - An image reply attributed to a different model (the Vision Adapter
//     switching a vision-less target to a combo member) is proof the TARGET
//     cannot see, so vision stays false and the detail names who answered.

// tinyPNGBase64 is the 8x8 red PNG upstream sends: the smallest real image most
// vision APIs accept (some reject 1x1).
const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAYAAADED76LAAAAFklEQVR4nGP8z8Dwn4GBgYGJgQoAAF9rAgOQ0tG7AAAAAElFTkSuQmCC"

// detectCapsProbeTimeout mirrors upstream's AbortSignal.timeout(20000).
const detectCapsProbeTimeout = 20 * time.Second

const detectCapsNote = "heuristic = nama/tabel dikenal; probe = terverifikasi lewat request nyata"

type detectCapsRequest struct {
	Model string `json:"model"`
}

type detectCapsFlags struct {
	Vision    bool `json:"vision"`
	Reasoning bool `json:"reasoning"`
}

type detectCapsDetail struct {
	Vision         string  `json:"vision"`
	Reasoning      string  `json:"reasoning"`
	ContextWindow  int     `json:"contextWindow,omitempty"`
	ThinkingFormat *string `json:"thinkingFormat,omitempty"`
}

type detectCapsResponse struct {
	OK     bool             `json:"ok"`
	Caps   detectCapsFlags  `json:"caps"`
	Detail detectCapsDetail `json:"detail"`
	Note   string           `json:"note"`
	Error  string           `json:"error,omitempty"`
}

// chatProbeMessage is the assistant message both probes inspect.
type chatProbeMessage struct {
	Content          string `json:"content"`
	Reasoning        any    `json:"reasoning"`
	ReasoningContent any    `json:"reasoning_content"`
	Thinking         any    `json:"thinking"`
	ThinkingContent  any    `json:"thinking_content"`
}

// chatCompletionProbe is the subset of a non-streaming chat completion both
// probes read.
type chatCompletionProbe struct {
	Model   string `json:"model"`
	Choices []struct {
		Message chatProbeMessage `json:"message"`
	} `json:"choices"`
}

// sendChatProbe runs one JSON body through the chat lane.
//
// Production dispatches to the ChatHandler the same way the dashboard's Test
// button does (probeModel), so credentials, fallback and routing are the real
// ones. A nil ChatHandler (a handler built for tooling) reports "no answer"
// rather than panicking; the caller keeps its heuristic verdict.
func (h *MediaHandler) sendChatProbe(ctx context.Context, body []byte) (int, []byte) {
	if h.chatProbe != nil {
		return h.chatProbe(ctx, body)
	}
	if h.ChatH == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, detectCapsProbeTimeout)
	defer cancel()
	probe, err := http.NewRequestWithContext(handlerutil.WithProbeContext(ctx), http.MethodPost, "/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, nil
	}
	probe.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ChatH.HandleChatCompletions(rec, probe)
	out, _ := io.ReadAll(io.LimitReader(rec.Body, probeBodyReadCap))
	return rec.Code, out
}

// reasoningFieldSet reports whether the response carries a thinking field with
// content, mirroring upstream detectReasoningFromResponse: a non-empty trimmed
// string, or any other truthy value.
func reasoningFieldSet(msg chatProbeMessage) bool {
	for _, value := range []any{msg.Reasoning, msg.ReasoningContent, msg.Thinking, msg.ThinkingContent} {
		switch v := value.(type) {
		case nil:
		case string:
			if strings.TrimSpace(v) != "" {
				return true
			}
		case bool:
			if v {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// HandleDetectCaps handles POST /api/models/detect-caps.
//
// Body: {"model":"alias/model-id"}.
// Returns {ok, caps:{vision,reasoning}, detail:{...}, note}.
func (h *MediaHandler) HandleDetectCaps(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		handlerutil.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read body"})
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req detectCapsRequest
	_ = json.Unmarshal(body, &req)
	if req.Model == "" || !strings.Contains(req.Model, "/") {
		handlerutil.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "Model required (alias/model-id)"})
		return
	}

	slash := strings.Index(req.Model, "/")
	providerAlias, modelID := req.Model[:slash], req.Model[slash+1:]

	heur := providers.GetCapabilitiesForModel(providerAlias, modelID)
	caps := detectCapsFlags{Vision: heur.Vision, Reasoning: heur.Reasoning}
	detail := detectCapsDetail{
		Vision:        "heuristic",
		Reasoning:     "heuristic",
		ContextWindow: heur.ContextWindow,
	}
	if heur.ThinkingFormat != "" {
		format := heur.ThinkingFormat
		detail.ThinkingFormat = &format
	}

	textOK := h.probeText(r.Context(), req.Model, &caps, &detail)

	visionOK := false
	if !caps.Vision {
		if switchedTo, answered := h.probeVision(r.Context(), req.Model); switchedTo != "" {
			caps.Vision = false
			detail.Vision = "combo-switched (answered by " + switchedTo + ", not the target)"
		} else if answered {
			visionOK = true
			caps.Vision = true
			detail.Vision = "probe (answered about the image)"
		}
	} else {
		visionOK = true
		detail.Vision = "heuristic (known vision model)"
	}

	handlerutil.WriteJSON(w, http.StatusOK, detectCapsResponse{
		OK:     textOK || visionOK,
		Caps:   caps,
		Detail: detail,
		Note:   detectCapsNote,
	})
}

// probeText runs the text-only probe: is the model alive, and does it emit a
// thinking field? It returns whether a usable completion came back.
func (h *MediaHandler) probeText(ctx context.Context, model string, caps *detectCapsFlags, detail *detectCapsDetail) bool {
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": probeMaxTokens,
		"stream":     false,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		return false
	}
	status, body := h.sendChatProbe(ctx, payload)
	if status < 200 || status >= 300 {
		return false
	}
	var parsed chatCompletionProbe
	if json.Unmarshal(body, &parsed) != nil || len(parsed.Choices) == 0 {
		return false
	}

	if reasoningFieldSet(parsed.Choices[0].Message) {
		caps.Reasoning = true
		detail.Reasoning = "probe (thinking field in response)"
	} else if caps.Reasoning {
		// Absence in one short "hi" reply is not proof of no-reasoning; the
		// heuristic table stays trusted (upstream keeps the same wording).
		detail.Reasoning = "heuristic (no thinking field in short response)"
	} else {
		detail.Reasoning = "none detected"
	}
	return true
}

// probeVision runs the image probe. switchedTo is non-empty when a different
// model answered; answered reports a non-empty content from the target itself.
func (h *MediaHandler) probeVision(ctx context.Context, model string) (switchedTo string, answered bool) {
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 64,
		"stream":     false,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "What color is this image? One word."},
				{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64," + tinyPNGBase64,
				}},
			},
		}},
	})
	if err != nil {
		return "", false
	}
	status, body := h.sendChatProbe(ctx, payload)
	if status < 200 || status >= 300 {
		return "", false
	}
	var parsed chatCompletionProbe
	if json.Unmarshal(body, &parsed) != nil || len(parsed.Choices) == 0 {
		return "", false
	}

	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	targetID := model[strings.LastIndex(model, "/")+1:]
	responded := parsed.Model
	if responded != "" && !strings.Contains(responded, targetID) {
		return responded, false
	}
	return "", content != ""
}
