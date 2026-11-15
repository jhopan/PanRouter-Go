package executor

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
)

// writeCompletion writes the buffered turn as a single chat.completion.
func (e *devinCLIEmitter) writeCompletion(reason string) error {
	message := map[string]any{"role": "assistant", "content": e.text.String()}
	if len(e.toolCalls) > 0 {
		message["content"] = nil
		message["tool_calls"] = e.toolCallPayload()
	}
	if reason == "" {
		reason = "stop"
	}
	body := map[string]any{
		"id":      e.id,
		"object":  "chat.completion",
		"created": e.created,
		"model":   e.model,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": reason}},
		"usage":   e.usage(),
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("devin-cli: marshal completion: %w", err)
	}
	e.roleOut = true
	return jsonResponse(e.req.Ctx, e.w, bytes.NewReader(encoded), e.req.TranslateResp, e.req.ResponseBuf)
}

// toolCallPayload renders the held calls in the OpenAI shape.
func (e *devinCLIEmitter) toolCallPayload() []any {
	calls := make([]any, 0, len(e.toolCalls))
	for _, call := range e.toolCalls {
		calls = append(calls, map[string]any{
			"id":   call.ID,
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	return calls
}

// usage is the token estimate upstream reports: this transport has no token
// accounting of its own, so the counts are the 4-characters-per-token estimate
// and are flagged as such.
func (e *devinCLIEmitter) usage() map[string]any {
	completion := devinCLITokenEstimate(e.text.String())
	return map[string]any{
		"prompt_tokens":     e.promptTokens,
		"completion_tokens": completion,
		"total_tokens":      e.promptTokens + completion,
		"estimated":         true,
	}
}

// devinCLITokenEstimate mirrors upstream's Math.ceil(length / 4).
func devinCLITokenEstimate(text string) int {
	return (len(text) + 3) / 4
}
