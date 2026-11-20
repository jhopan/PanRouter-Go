package translator

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
)

// MoveSystemInstructionToContents folds the systemInstruction of a Gemini
// request body into contents[0] as a user-role turn and clears the member.
// It reports whether the rewrite applied; the input bytes are never mutated.
//
// The Antigravity backend answers a detail-free 429 RESOURCE_EXHAUSTED when it
// rejects the system instruction's shape rather than the account's quota, and
// folding that instruction into the first user turn is what clears the refusal
// (9router-go c69c20d, ported from the same finding that produced the Hermes
// and requestType rows). A body with no system instruction, or an empty one,
// is returned unchanged so the caller pays no second request.
//
// The body is decoded into jsontext.Value members: every member the rewrite
// does not touch keeps its original bytes, so a functionCall's thought
// signature is never re-encoded. Both spellings are handled —
// systemInstruction, which GeminiRequest marshals today, and
// system_instruction, which older clients still send.
func MoveSystemInstructionToContents(body []byte) ([]byte, bool) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return body, false
	}
	rawSystem := fields["systemInstruction"]
	if snake, ok := fields["system_instruction"]; ok {
		rawSystem = snake
	}
	if len(rawSystem) == 0 || rawSystem.Kind() == jsontext.KindNull {
		return body, false
	}
	var sys GeminiContent
	if err := json.Unmarshal(rawSystem, &sys); err != nil || len(sys.Parts) == 0 {
		return body, false
	}
	var contents []GeminiContent
	if raw := fields["contents"]; len(raw) > 0 && raw.Kind() != jsontext.KindNull {
		if err := json.Unmarshal(raw, &contents); err != nil {
			return body, false
		}
	}
	// The instruction becomes the opening user turn, keeping the caller's
	// order: the system text first, then whatever the conversation said.
	moved := GeminiContent{Role: "user", Parts: sys.Parts}
	if len(contents) > 0 && contents[0].Role == "user" {
		moved.Parts = append(moved.Parts, contents[0].Parts...)
		contents = contents[1:]
	}
	contents = append([]GeminiContent{moved}, contents...)
	encoded, err := json.Marshal(contents, json.Deterministic(true))
	if err != nil {
		return body, false
	}
	fields["contents"] = encoded
	delete(fields, "systemInstruction")
	delete(fields, "system_instruction")
	out, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return body, false
	}
	return out, true
}
