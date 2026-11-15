package translator

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Port of open-sse/translator/request/openai-to-kiro.js: OpenAI Chat
// Completions -> Kiro/CodeWhisperer `conversationState` envelope.
//
// The kiro.dev gateway answers 400 {"message":"Improperly formed
// request.","reason":"REQUEST_BODY_INVALID"} for any body that is not a
// conversationState envelope — a top-level `systemPrompt` is also rejected, so
// the system text travels inside the first user turn's content.
//
// Session replay, thinking budgets and tool-spec name canonicalization from the
// JS version are NOT ported: this covers text/image conversations plus tool
// calls and tool results, which is what the dashboard and CLI clients send.

const kiroDefaultMaxTokens = 32000

// kiroEmptyUserText mirrors kiroEmptyUserContent in kiroConversation.js: an
// empty user turn would make the model answer "nothing to continue", so a
// neutral placeholder is injected (decolua/panrouter #4109).
const (
	kiroToolResultsPlaceholder = "Tool results provided."
	kiroEmptyUserPlaceholder   = "continue"
	kiroAssistantPlaceholder   = "..."
)

// KiroTranslateOptions carries the per-connection material the envelope needs.
type KiroTranslateOptions struct {
	// Model is the upstream model id (provider prefix already stripped).
	Model string
	// ProfileArn is the connection's resolved CodeWhisperer profile ARN.
	ProfileArn string
	// ContentPrefix is the stable system material prepended to the session's
	// opening turn (thinking prefix, agentic protocol). Empty means only the
	// time context is added.
	ContentPrefix string
	// ConversationID ties consecutive turns of one client session together, so
	// the frozen opening turn can be replayed (upstream resolveSessionIdentity).
	// Empty means every request gets a fresh conversation.
	ConversationID string
	// ConnectionID scopes the replay store to the account that serves the
	// session: two accounts must not share a cache prefix they never had.
	ConnectionID string
	// ToolNameMap receives the reverse of the catalog's name sanitization
	// (sanitized → original), so the response side can restore the client's own
	// tool names. Nil when the caller has no such map.
	ToolNameMap map[string]string
	// AdditionalFields is the native effort envelope the newer Claude/GPT models
	// accept (upstream buildKiroAdditionalModelRequestFieldsForModel). Nil means
	// the model takes none.
	AdditionalFields map[string]any
}

// OpenAIToKiro converts an OpenAI chat-completions body into the Kiro
// conversationState envelope. It returns an error when the conversation cannot
// be shaped into a valid envelope, so callers fail locally instead of spending
// an upstream call that is guaranteed to 400.
func OpenAIToKiro(body []byte, opts KiroTranslateOptions) ([]byte, error) {
	var in struct {
		Messages    []map[string]any `json:"messages"`
		Tools       []map[string]any `json:"tools"`
		Temperature *float64         `json:"temperature"`
		TopP        *float64         `json:"top_p"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("kiro: parse openai body: %w", err)
	}

	history, current, err := kiroConvertMessages(in.Messages, opts.Model)
	if err != nil {
		return nil, err
	}

	maxTokens := kiroDefaultMaxTokens
	return kiroEnvelope(history, current, in.Tools, opts, maxTokens, in.Temperature, in.TopP)
}

// kiroEnvelope assembles the conversationState payload both request directions
// send: session replay freezes the opening turn, the canonicalizer makes the
// conversation structurally shippable, and the Kiro fields travel where the
// gateway reads them.
//
// A conversation that still cannot be shaped fails here rather than upstream:
// Kiro answers 400 REQUEST_BODY_INVALID for it, and that 400 costs a request and a
// per-account cooldown for a body this process already knew was malformed.
func kiroEnvelope(history []map[string]any, current map[string]any, tools []map[string]any, opts KiroTranslateOptions, maxTokens int, temperature, topP *float64) ([]byte, error) {
	conversationID := opts.ConversationID
	if conversationID == "" {
		conversationID = newKiroConversationID()
	}
	// The time context is the volatile part of the prefix, so the session replay
	// freezes the opening turn with the time it was built and puts the fresh one
	// only on the current turn. That is what keeps the upstream cache prefix
	// byte-identical across turns of one conversation.
	timePrefix := "[Context: Current time is " + time.Now().UTC().Format(time.RFC3339) + "]"
	// The replay works on wrapped turns (upstream's currentMessage shape); the
	// converter hands the inner userInputMessage, so wrap, replay, unwrap.
	replayedHistory, replayedCurrent := kiroApplySessionReplay(conversationID, opts.ConnectionID, opts.Model, opts.ContentPrefix, timePrefix,
		history, map[string]any{"userInputMessage": current})
	history = replayedHistory
	if user, ok := replayedCurrent["userInputMessage"].(map[string]any); ok {
		current = user
	}

	specs, nameMap := kiroNormalizeToolSpecs(tools)
	history, current, _, valid, canonErrs := KiroCanonicalizeConversation(history, current, opts.Model, specs, nameMap)
	if !valid {
		return nil, fmt.Errorf("kiro: refusing invalid conversation: %s", strings.Join(canonErrs, ", "))
	}

	currentContent, _ := current["content"].(string)
	userInput := map[string]any{
		"content": currentContent,
		"modelId": opts.Model,
		"origin":  "AI_EDITOR",
	}
	if imgs, ok := current["images"].([]any); ok && len(imgs) > 0 {
		userInput["images"] = imgs
	}
	// Kiro has no top-level `tools` array: the catalogue travels on the current
	// user turn, which the canonicalizer placed there. Without it the model
	// invents tools in plain text instead of emitting toolUseEvent, and clients
	// never get tool_calls.
	if ctx, ok := current["userInputMessageContext"].(map[string]any); ok && len(ctx) > 0 {
		userInput["userInputMessageContext"] = ctx
	}

	payload := map[string]any{
		"conversationState": map[string]any{
			"chatTriggerType": "MANUAL",
			"conversationId":  conversationID,
			"currentMessage": map[string]any{
				"userInputMessage": userInput,
			},
			"history": history,
		},
	}
	if opts.ProfileArn != "" {
		payload["profileArn"] = opts.ProfileArn
	}
	if len(opts.AdditionalFields) > 0 {
		payload["additionalModelRequestFields"] = opts.AdditionalFields
	}

	inference := map[string]any{"maxTokens": maxTokens}
	if temperature != nil {
		inference["temperature"] = *temperature
	}
	if topP != nil {
		inference["topP"] = *topP
	}
	payload["inferenceConfig"] = inference

	// Kiro sanitizes tool names (`mcp__a__b` → `mcp_a_b`), so the reverse map has
	// to travel with the payload: the response side restores the client's own
	// names, or the client cannot dispatch a call it never declared.
	for original, sanitized := range nameMap {
		if original != sanitized {
			if opts.ToolNameMap != nil {
				opts.ToolNameMap[sanitized] = original
			}
		}
	}

	out, err := json.Marshal(payload, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("kiro: marshal envelope: %w", err)
	}
	return out, nil
}

// kiroConvertMessages maps OpenAI messages onto Kiro turns. system/tool become
// user turns, consecutive same-role turns merge, and the trailing user turn is
// returned separately as the current message.
func kiroConvertMessages(messages []map[string]any, model string) ([]map[string]any, map[string]any, error) {
	var history []map[string]any
	currentRole := ""
	var pendingUser, pendingAssistant []string
	var pendingToolResults, pendingImages []any
	flush := func() {
		switch currentRole {
		case "user":
			content := strings.TrimSpace(strings.Join(pendingUser, "\n\n"))
			if content == "" {
				if len(pendingToolResults) > 0 {
					content = kiroToolResultsPlaceholder
				} else {
					content = kiroEmptyUserPlaceholder
				}
			}
			turn := map[string]any{
				"userInputMessage": map[string]any{
					"content": content,
					"modelId": "",
				},
			}
			if len(pendingImages) > 0 {
				turn["userInputMessage"].(map[string]any)["images"] = pendingImages
			}
			if len(pendingToolResults) > 0 {
				turn["userInputMessage"].(map[string]any)["userInputMessageContext"] = map[string]any{
					"toolResults": pendingToolResults,
				}
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
		pendingUser = nil
		pendingAssistant = nil
		pendingToolResults = nil
		pendingImages = nil
	}

	for _, msg := range messages {
		if msg == nil {
			continue
		}
		rawRole, _ := msg["role"].(string)
		wasSystem := rawRole == "system" || rawRole == "developer"
		role := rawRole
		if role == "system" || role == "tool" {
			role = "user"
		}

		if currentRole != "" && role != currentRole {
			flush()
		}
		currentRole = role

		switch role {
		case "user":
			content, images, toolResults := kiroUserContent(msg)
			pendingImages = append(pendingImages, images...)
			pendingToolResults = append(pendingToolResults, toolResults...)
			if content != "" {
				if wasSystem {
					content = "<instructions>\n" + content + "\n</instructions>"
				}
				pendingUser = append(pendingUser, content)
			}
		case "assistant":
			text := kiroAssistantText(msg)
			toolUses := kiroAssistantToolUses(msg)
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

	// The last user turn is the current message; the rest stays in history.
	// Return the inner userInputMessage (callers read content/images/context off it).
	var current map[string]any
	for i := len(history) - 1; i >= 0; i-- {
		if uim, ok := history[i]["userInputMessage"].(map[string]any); ok {
			current = uim
			history = append(history[:i:i], history[i+1:]...)
			break
		}
	}
	if current == nil {
		current = map[string]any{"content": "", "modelId": model}
	}
	// Merging same-role turns is the canonicalizer's job; returning them raw keeps
	// the repair accounting in one place.
	return history, current, nil
}

// kiroUserContent flattens one user/tool message into text, Kiro image blocks
// and tool-result blocks.
func kiroUserContent(msg map[string]any) (string, []any, []any) {
	var textParts []string
	var images, toolResults []any

	appendToolResult := func(id string, isErr bool, content string) {
		if id == "" {
			return
		}
		status := "success"
		if isErr {
			status = "error"
		}
		toolResults = append(toolResults, map[string]any{
			"toolUseId": id,
			"status":    status,
			"content":   []any{map[string]any{"text": content}},
		})
	}

	switch content := msg["content"].(type) {
	case string:
		if msg["tool_call_id"] != nil {
			isErr, _ := msg["is_error"].(bool)
			appendToolResult(fmt.Sprint(msg["tool_call_id"]), isErr, content)
			return "", images, toolResults
		}
		return content, images, toolResults
	case []any:
		for _, raw := range content {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch blockType, _ := block["type"].(string); blockType {
			case "text":
				if s, ok := block["text"].(string); ok {
					textParts = append(textParts, s)
				}
			case "image_url":
				url := ""
				if iu, ok := block["image_url"].(map[string]any); ok {
					url, _ = iu["url"].(string)
				}
				if img := kiroImageFromDataURI(url); img != nil {
					images = append(images, img)
				} else if url != "" {
					textParts = append(textParts, "[Image: "+url+"]")
				}
			case "image":
				// Claude shape: source.type=base64, source.media_type, source.data
				src, ok := block["source"].(map[string]any)
				if !ok {
					continue
				}
				if t, _ := src["type"].(string); t != "base64" {
					continue
				}
				data, _ := src["data"].(string)
				media := "image/png"
				if mt, ok := src["media_type"].(string); ok && mt != "" {
					media = mt
				}
				if data != "" {
					images = append(images, kiroImageBlock(media, data))
				}
			case "tool_result":
				text := ""
				switch inner := block["content"].(type) {
				case string:
					text = inner
				case []any:
					var b strings.Builder
					for _, ir := range inner {
						if m, ok := ir.(map[string]any); ok {
							if s, ok := m["text"].(string); ok {
								b.WriteString(s)
							}
						}
					}
					text = b.String()
				}
				isErr, _ := block["is_error"].(bool)
				id, _ := block["tool_use_id"].(string)
				appendToolResult(id, isErr, text)
			}
		}
	}
	return strings.Join(textParts, "\n"), images, toolResults
}

func kiroAssistantText(msg map[string]any) string {
	switch content := msg["content"].(type) {
	case string:
		return strings.TrimSpace(content)
	case []any:
		var parts []string
		for _, raw := range content {
			if block, ok := raw.(map[string]any); ok {
				if t, _ := block["type"].(string); t == "text" {
					if s, ok := block["text"].(string); ok {
						parts = append(parts, s)
					}
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

// kiroAssistantToolUses normalizes OpenAI tool_calls (and Claude tool_use
// blocks) into Kiro toolUses.
func kiroAssistantToolUses(msg map[string]any) []any {
	var out []any
	add := func(id, name string, input any) {
		if name == "" {
			return
		}
		if id == "" {
			id = newKiroUUID()
		}
		if input == nil {
			input = map[string]any{}
		}
		out = append(out, map[string]any{
			"toolUseId": id,
			"name":      name,
			"input":     input,
		})
	}

	if raw, ok := msg["tool_calls"].([]any); ok {
		for _, item := range raw {
			tc, ok := item.(map[string]any)
			if !ok {
				continue
			}
			id, _ := tc["id"].(string)
			fn, ok := tc["function"].(map[string]any)
			if !ok {
				continue
			}
			name, _ := fn["name"].(string)
			add(id, name, kiroParseToolInput(fn["arguments"]))
		}
	}
	if raw, ok := msg["content"].([]any); ok {
		for _, item := range raw {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t != "tool_use" {
				continue
			}
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			add(id, name, block["input"])
		}
	}
	return out
}

// kiroParseToolInput decodes a JSON-encoded arguments string, tolerating the
// malformed payloads real clients emit.
func kiroParseToolInput(raw any) any {
	s, ok := raw.(string)
	if !ok {
		if raw == nil {
			return map[string]any{}
		}
		return raw
	}
	var parsed any
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return map[string]any{}
	}
	return parsed
}

func kiroImageFromDataURI(uri string) any {
	if !strings.HasPrefix(uri, "data:") {
		return nil
	}
	rest := strings.TrimPrefix(uri, "data:")
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return nil
	}
	media := rest[:comma]
	if i := strings.IndexByte(media, ';'); i >= 0 {
		media = media[:i]
	}
	if media == "" {
		media = "image/png"
	}
	return kiroImageBlock(media, rest[comma+1:])
}

func kiroImageBlock(mediaType, base64Data string) map[string]any {
	format := mediaType
	if i := strings.IndexByte(format, '/'); i >= 0 {
		format = format[i+1:]
	}
	return map[string]any{
		"format": format,
		"source": map[string]any{"bytes": base64Data},
	}
}

func newKiroUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:16])
}

func newKiroConversationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
