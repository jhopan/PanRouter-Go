// Claude → Kiro request translation — port of
// open-sse/translator/request/claude-to-kiro.js, the direct `claude:kiro` route.
//
// A Claude Messages body reaches Kiro without a pivot through OpenAI: the block
// shapes line up (text / image / tool_use / tool_result), and the pivot loses the
// things this route keeps — `system` as an array of blocks, tool results carrying
// screenshots, and the Anthropic tool-id spelling.
//
// Everything after the conversion is shared with the OpenAI route: session replay
// freezes the opening turn, the canonicalizer makes the conversation structurally
// shippable, and the envelope builder writes the payload.
package translator

import (
	"fmt"
	"strings"

	json "encoding/json/v2"
)

// IsClaudeRequestBody reports whether a body is an Anthropic Messages request
// rather than an OpenAI Chat Completions one.
//
// The discriminator is `system`: Anthropic carries the system prompt at the top
// level, OpenAI as a message role, so a top-level `system` can only be the
// Messages dialect. Bodies without one are deliberately reported as OpenAI — the
// router pivots `claude → kiro` before the executor whenever the client speaks
// Messages, so an OpenAI-shaped body is the normal case there, and the OpenAI
// converter reads Anthropic blocks without loss.
func IsClaudeRequestBody(raw []byte) bool {
	var probe struct {
		System *any `json:"system"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.System != nil
}

// ClaudeToKiro converts an Anthropic Messages body into the Kiro conversationState
// envelope.
func ClaudeToKiro(body []byte, opts KiroTranslateOptions) ([]byte, error) {
	var in struct {
		Messages    []map[string]any `json:"messages"`
		Tools       []map[string]any `json:"tools"`
		System      any              `json:"system"`
		MaxTokens   *float64         `json:"max_tokens"`
		Temperature *float64         `json:"temperature"`
		TopP        *float64         `json:"top_p"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("kiro: parse claude body: %w", err)
	}

	// The Claude system prompt is request material, not a message: it joins the
	// stable prefix after the thinking/agentic material, which is where upstream
	// puts it (and where the gateway accepts it — a top-level systemPrompt is
	// rejected).
	if systemText := ClaudeSystemText(in.System); systemText != "" {
		if strings.TrimSpace(opts.ContentPrefix) == "" {
			opts.ContentPrefix = systemText
		} else {
			opts.ContentPrefix = opts.ContentPrefix + "\n\n" + systemText
		}
	}
	history, current := kiroConvertClaudeMessages(in.Messages, opts.Model)
	maxTokens := kiroDefaultMaxTokens
	if in.MaxTokens != nil && *in.MaxTokens > 0 {
		maxTokens = int(*in.MaxTokens)
	}
	return kiroEnvelope(history, current, in.Tools, opts, maxTokens, in.Temperature, in.TopP)
}

// kiroConvertClaudeMessages maps Anthropic messages onto Kiro turns: system and
// tool_result blocks become user turns, tool_use blocks become assistant toolUses,
// and the trailing user turn is returned as the current message.
func kiroConvertClaudeMessages(messages []map[string]any, model string) ([]map[string]any, map[string]any) {
	var history []map[string]any
	var pendingUser, pendingAssistant []string
	var pendingResults, pendingImages []any
	currentRole := ""

	flush := func() {
		switch currentRole {
		case "user":
			content := strings.TrimSpace(strings.Join(pendingUser, "\n\n"))
			if content == "" {
				if len(pendingResults) > 0 {
					content = kiroToolResultsPlaceholder
				} else {
					content = kiroEmptyUserPlaceholder
				}
			}
			turn := map[string]any{
				"userInputMessage": map[string]any{"content": content, "modelId": ""},
			}
			user := turn["userInputMessage"].(map[string]any)
			if len(pendingImages) > 0 {
				user["images"] = pendingImages
			}
			if len(pendingResults) > 0 {
				user["userInputMessageContext"] = map[string]any{"toolResults": pendingResults}
			}
			history = append(history, turn)
		case "assistant":
			content := strings.TrimSpace(strings.Join(pendingAssistant, "\n\n"))
			if content == "" {
				content = kiroAssistantPlaceholder
			}
			history = append(history, map[string]any{
				"assistantResponseMessage": map[string]any{"content": content},
			})
		}
		pendingUser, pendingAssistant, pendingResults, pendingImages = nil, nil, nil, nil
	}

	for _, msg := range messages {
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}
		if currentRole != "" && role != currentRole {
			flush()
		}
		currentRole = role

		switch role {
		case "user":
			text, images, results := kiroClaudeUserBlocks(msg)
			pendingUser = append(pendingUser, text...)
			pendingImages = append(pendingImages, images...)
			pendingResults = append(pendingResults, results...)
		case "assistant":
			text, toolUses := kiroClaudeAssistantBlocks(msg)
			if text != "" {
				pendingAssistant = append(pendingAssistant, text)
			}
			if len(toolUses) > 0 {
				flush()
				if n := len(history); n > 0 {
					if arm, ok := history[n-1]["assistantResponseMessage"].(map[string]any); ok {
						arm["toolUses"] = toolUses
					}
				}
				currentRole = ""
			}
		}
	}
	if currentRole != "" {
		flush()
	}

	// Pop the last user turn as the current message.
	var current map[string]any
	for i := len(history) - 1; i >= 0; i-- {
		if user, ok := history[i]["userInputMessage"].(map[string]any); ok {
			current = user
			history = append(history[:i:i], history[i+1:]...)
			break
		}
	}
	if current == nil {
		current = map[string]any{"content": "", "modelId": model}
	}
	return history, current
}

// kiroClaudeUserBlocks reads one Anthropic user message: text parts, base64
// images, and tool results. Images a tool returned ride along as user images,
// because Kiro tool results are text-only.
func kiroClaudeUserBlocks(msg map[string]any) (text []string, images, results []any) {
	if raw, ok := msg["content"].(string); ok {
		return []string{raw}, nil, nil
	}
	blocks, _ := msg["content"].([]any)
	for _, item := range blocks {
		block, _ := item.(map[string]any)
		switch blockType, _ := block["type"].(string); blockType {
		case "text":
			if s, ok := block["text"].(string); ok && s != "" {
				text = append(text, s)
			}
		case "image":
			if img := kiroClaudeImage(block); img != nil {
				images = append(images, img)
			}
		case "tool_result":
			resultText, resultImages := kiroClaudeToolResultContent(block["content"])
			images = append(images, resultImages...)
			status := "success"
			if isErr, _ := block["is_error"].(bool); isErr {
				status = "error"
			}
			id, _ := block["tool_use_id"].(string)
			results = append(results, map[string]any{
				"toolUseId": id,
				"status":    status,
				"content":   []any{map[string]any{"text": resultText}},
			})
		}
	}
	return text, images, results
}

// kiroClaudeImage converts an Anthropic base64 image block.
func kiroClaudeImage(block map[string]any) any {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return nil
	}
	if kind, _ := source["type"].(string); kind != "base64" {
		return nil
	}
	data, _ := source["data"].(string)
	if data == "" {
		return nil
	}
	media, _ := source["media_type"].(string)
	if media == "" {
		media = "image/png"
	}
	return kiroImageBlock(media, data)
}

// kiroClaudeToolResultContent renders a tool_result's content, returning its text
// and any images it carried.
func kiroClaudeToolResultContent(content any) (string, []any) {
	switch value := content.(type) {
	case nil:
		return "", nil
	case string:
		return value, nil
	case []any:
		var parts []string
		var images []any
		for _, item := range value {
			block, _ := item.(map[string]any)
			switch blockType, _ := block["type"].(string); blockType {
			case "text":
				if s, ok := block["text"].(string); ok {
					parts = append(parts, s)
				}
			case "image":
				if img := kiroClaudeImage(block); img != nil {
					images = append(images, img)
				}
			}
		}
		joined := strings.Join(parts, "\n")
		if joined == "" && len(images) > 0 {
			return "(image attached)", images
		}
		if joined == "" {
			encoded, err := json.Marshal(value)
			if err != nil {
				return "", images
			}
			return string(encoded), images
		}
		return joined, images
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", nil
		}
		return string(encoded), nil
	}
}

// kiroClaudeAssistantBlocks reads one Anthropic assistant message: its text and
// its tool_use blocks.
func kiroClaudeAssistantBlocks(msg map[string]any) (string, []any) {
	if raw, ok := msg["content"].(string); ok {
		return strings.TrimSpace(raw), nil
	}
	blocks, _ := msg["content"].([]any)
	var text strings.Builder
	var toolUses []any
	for _, item := range blocks {
		block, _ := item.(map[string]any)
		switch blockType, _ := block["type"].(string); blockType {
		case "text":
			if s, ok := block["text"].(string); ok {
				text.WriteString(s)
			}
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			if name == "" {
				continue
			}
			input := block["input"]
			if input == nil {
				input = map[string]any{}
			}
			toolUses = append(toolUses, map[string]any{
				"toolUseId": id,
				"name":      name,
				"input":     input,
			})
		}
	}
	return strings.TrimSpace(text.String()), toolUses
}

// ClaudeSystemText flattens an Anthropic `system` field — a string or an array of
// blocks — into the single string the Kiro prefix carries.
func ClaudeSystemText(system any) string {
	switch value := system.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(value)
	case []any:
		var parts []string
		for _, item := range value {
			switch block := item.(type) {
			case string:
				parts = append(parts, block)
			case map[string]any:
				if s, ok := block["text"].(string); ok && s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}
