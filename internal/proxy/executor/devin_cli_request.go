package executor

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// devinCLIMessage is one inbound message. Content and tool_calls stay raw JSON:
// this prompt builder reads them lazily, so a long agent transcript costs no
// intermediate allocation.
type devinCLIMessage struct {
	Role       string         `json:"role"`
	Type       string         `json:"type"`
	Content    jsontext.Value `json:"content"`
	ToolCalls  jsontext.Value `json:"tool_calls"`
	ToolCallID string         `json:"tool_call_id"`
}

// devinCLIRequest is the subset of the caller's body that changes what is sent
// to the CLI.
type devinCLIRequest struct {
	Model            string             `json:"model"`
	Messages         []devinCLIMessage  `json:"messages"`
	Input            jsontext.Value     `json:"input"`
	Tools            []deepSeekToolSpec `json:"tools"`
	Cwd              string             `json:"cwd"`
	WorkingDirectory string             `json:"working_directory"`
	Workdir          string             `json:"workdir"`
	Workspace        string             `json:"workspace"`
	Metadata         struct {
		Cwd              string `json:"cwd"`
		WorkingDirectory string `json:"working_directory"`
	} `json:"metadata"`
}

// devinCLIPromptText inlines the whole conversation into the single prompt the
// ACP session takes, including past tool calls and their results: the CLI is
// told in prose what the client already knows, because a re-spawned agent has
// no memory of the previous turn.
func devinCLIPromptText(messages []devinCLIMessage) string {
	lines := make([]string, 0, len(messages))
	for _, message := range messages {
		role := message.Role
		if role == "" {
			role = "user"
		}
		text := devinCLIContentText(message.Content)
		switch role {
		case "assistant":
			if calls := devinCLIOpenAIToolCalls(message.ToolCalls); len(calls) > 0 {
				parts := make([]string, 0, len(calls)+1)
				if text != "" {
					parts = append(parts, text)
				}
				for _, call := range calls {
					name := call.Name
					if name == "" {
						name = "tool"
					}
					parts = append(parts, fmt.Sprintf("[Tool call %s id=%s]\n%s", name, call.ID, call.Arguments))
				}
				text = strings.Join(parts, "\n\n")
			}
		case "tool":
			text = fmt.Sprintf("[Tool result id=%s]\n%s", message.ToolCallID, devinCLIRawText(message.Content))
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		switch role {
		case "system":
			lines = append(lines, "[System]\n"+text)
		case "assistant":
			lines = append(lines, "[Assistant]\n"+text)
		case "tool":
			lines = append(lines, "[Tool]\n"+text)
		default:
			lines = append(lines, "[User]\n"+text)
		}
	}
	if len(lines) == 0 {
		return "(empty)"
	}
	return strings.Join(lines, "\n\n")
}

// devinCLIInputMessages reads the Responses-style `input` array. A plain string
// input (the Responses shorthand for a single user turn) is folded into one
// message; anything else that does not decode as a message list yields nothing,
// which is also what upstream's Array.isArray check does.
func devinCLIInputMessages(input jsontext.Value) []devinCLIMessage {
	if len(input) == 0 || input[0] != '[' {
		return nil
	}
	var messages []devinCLIMessage
	if err := json.Unmarshal(input, &messages); err != nil {
		return nil
	}
	return messages
}

// devinCLIContentBlock is one Claude-shaped content block.
type devinCLIContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	InputText string         `json:"input_text"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Input     jsontext.Value `json:"input"`
	ToolUseID string         `json:"tool_use_id"`
	Content   jsontext.Value `json:"content"`
}

// devinCLIBlockText is the text a block carries, in either spelling (Chat
// Completions `text` or the Responses `input_text`).
func devinCLIBlockText(block devinCLIContentBlock) string {
	return firstNonEmpty(block.Text, block.InputText)
}

// devinCLIContentBlocks decodes a content field that may be an array of blocks.
func devinCLIContentBlocks(content jsontext.Value) []devinCLIContentBlock {
	if len(content) == 0 {
		return nil
	}
	var blocks []devinCLIContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}
	return blocks
}

// devinCLIContentText renders a content field as prompt text: a plain string
// stays as it is, and an array of blocks contributes its text plus the
// tool_use and tool_result markers upstream writes.
func devinCLIContentText(content jsontext.Value) string {
	if len(content) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return text
	}
	var builder strings.Builder
	for _, block := range devinCLIContentBlocks(content) {
		switch block.Type {
		case "text":
			builder.WriteString(block.Text)
		case "tool_use":
			fmt.Fprintf(&builder, "\n[Tool call %s id=%s]\n%s\n", block.Name, block.ID, devinCLIRawText(block.Input))
		case "tool_result":
			fmt.Fprintf(&builder, "\n[Tool result id=%s]\n%s\n", block.ToolUseID, devinCLIRawText(block.Content))
		}
	}
	return builder.String()
}

// devinCLIRawText renders a raw JSON value as upstream does: a JSON string
// contributes its contents, everything else is serialised, and an absent value
// becomes "".
func devinCLIRawText(value jsontext.Value) string {
	if len(value) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(value, &text); err == nil {
		return text
	}
	return string(value)
}

// devinCLIOpenAIToolCalls decodes an assistant turn's tool_calls array.
func devinCLIOpenAIToolCalls(raw jsontext.Value) []devinCLIToolCall {
	if len(raw) == 0 {
		return nil
	}
	var calls []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Function struct {
			Name      string         `json:"name"`
			Arguments jsontext.Value `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil
	}
	out := make([]devinCLIToolCall, 0, len(calls))
	for _, call := range calls {
		name := call.Function.Name
		if name == "" {
			name = call.Name
		}
		out = append(out, devinCLIToolCall{
			ID:        call.ID,
			Name:      name,
			Arguments: devinCLIRawText(call.Function.Arguments),
		})
	}
	return out
}

// devinCLICwdPattern is the `<cwd>` environment block agent clients embed in
// the user message (Codex and friends).
var devinCLICwdPattern = regexp.MustCompile(`(?i)<cwd>\s*([^<]+?)\s*</cwd>`)

// devinCLIWorkspaceCwd resolves the workspace the child is spawned in.
//
// An absolute existing directory wins; anything else falls back to the temp
// directory, because the agent's file tools resolve relative paths against this
// directory and a made-up one would make create/delete land somewhere the caller
// cannot see (upstream hit exactly that with os.tmpdir()).
func devinCLIWorkspaceCwd(body *devinCLIRequest, messages []devinCLIMessage) string {
	candidates := []string{
		body.Cwd, body.WorkingDirectory, body.Workdir, body.Workspace,
		body.Metadata.Cwd, body.Metadata.WorkingDirectory,
	}
	for _, message := range messages {
		for _, text := range devinCLIContentTexts(message.Content) {
			for _, match := range devinCLICwdPattern.FindAllStringSubmatch(text, -1) {
				candidates = append(candidates, strings.TrimSpace(match[1]))
			}
		}
	}
	for _, candidate := range candidates {
		if candidate == "" || !filepath.IsAbs(candidate) {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return os.TempDir()
}

// devinCLIContentTexts lists the text a content field carries, in either
// spelling and including Claude-shaped blocks, so the `<cwd>` scan sees all of
// it.
func devinCLIContentTexts(content jsontext.Value) []string {
	if len(content) == 0 {
		return nil
	}
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return []string{text}
	}
	var texts []string
	for _, block := range devinCLIContentBlocks(content) {
		if blockText := devinCLIBlockText(block); blockText != "" {
			texts = append(texts, blockText)
		}
		if raw := devinCLIRawText(block.Content); raw != "" {
			texts = append(texts, raw)
		}
	}
	return texts
}
