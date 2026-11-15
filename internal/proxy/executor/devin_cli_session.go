package executor

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// devinCLIClientToolTitle matches the ACP title the CLI gives an MCP call from
// our own bridge: "Calling mcp_<name> from clientTools".
var devinCLIClientToolTitle = regexp.MustCompile(`^Calling (mcp_\S+)\b`)

// consumeNotification handles everything the CLI pushes at us.
func (s *devinCLISession) consumeNotification(msg *devinCLIRPCMessage) (bool, error) {
	switch msg.Method {
	case "session/request_permission":
		return false, s.approvePermission(msg)
	case "_cognition.ai/agent_stopped", "$/agent_stopped":
		return true, s.stopped(msg)
	case "session/update", "$/update":
		return s.consumeUpdate(msg)
	}
	return false, nil
}

// stopped handles the CLI's own stop signal.
//
// The cause is read from `errorMessage` (devin 3000.2.x) with `message` and
// `error` as the older spellings; anything but an error cause ends the turn
// normally, which is what an agent that simply finished reports.
func (s *devinCLISession) stopped(msg *devinCLIRPCMessage) error {
	var params struct {
		Cause        string `json:"cause"`
		ErrorMessage string `json:"errorMessage"`
		Message      string `json:"message"`
		Error        string `json:"error"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	if params.Cause == "error" {
		text := firstNonEmpty(params.ErrorMessage, params.Message, params.Error, "Devin agent error")
		return &devinCLIError{status: 502, message: text}
	}
	return s.finish("stop")
}

// approvePermission grants the first allow option of a permission request.
//
// The CLI blocks on the answer, so a request left unanswered is a stream that
// never ends. DEVIN_PERMISSION_MODE=bypass suppresses most of them, and some
// tool kinds still prompt, which is why both exist (devin-cli.js:648-670).
func (s *devinCLISession) approvePermission(msg *devinCLIRPCMessage) error {
	if len(msg.ID) == 0 {
		return nil
	}
	var params struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	allowed := ""
	for _, option := range params.Options {
		if strings.Contains(strings.ToLower(option.Kind), "allow") {
			allowed = option.OptionID
			break
		}
	}
	if allowed == "" && len(params.Options) > 0 {
		allowed = params.Options[0].OptionID
	}
	if allowed == "" {
		return nil
	}
	reply := devinCLIPermissionReply{JSONRPC: "2.0", ID: msg.ID}
	reply.Result.Outcome.Outcome = "selected"
	reply.Result.Outcome.OptionID = allowed
	return s.reply(msg.ID, reply.Result)
}

// consumeUpdate folds one session/update notification into the turn.
func (s *devinCLISession) consumeUpdate(msg *devinCLIRPCMessage) (bool, error) {
	var params struct {
		Update struct {
			SessionUpdate string         `json:"sessionUpdate"`
			Content       jsontext.Value `json:"content"`
			ToolCallID    string         `json:"toolCallId"`
			Title         string         `json:"title"`
			RawInput      jsontext.Value `json:"rawInput"`
		} `json:"update"`
		Type    string         `json:"type"`
		Content jsontext.Value `json:"content"`
		Delta   string         `json:"delta"`
		Text    string         `json:"text"`
		Message string         `json:"message"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return false, nil
	}
	// devin 3000.2.x nests the payload under params.update.sessionUpdate;
	// older builds used a flat params.type.
	kind := firstNonEmpty(params.Update.SessionUpdate, params.Type)
	content := params.Update.Content
	if len(content) == 0 {
		content = params.Content
	}
	delta := firstNonEmpty(devinCLITextContent(content), params.Delta, params.Text)

	if s.clientTools && (kind == "tool_call" || kind == "tool_call_update") {
		return s.consumeClientToolCall(params.Update.ToolCallID, params.Update.Title, params.Update.RawInput)
	}

	switch kind {
	case "agent_message_chunk", "message_delta", "text_delta", "content_delta":
		if delta != "" {
			return false, s.emit.delta(delta)
		}
	case "agent_thought_chunk":
		// Internal reasoning, not surfaced to the client (upstream too).
	case "message_stop", "stop", "done":
		return true, s.finish("stop")
	case "error":
		return false, &devinCLIError{status: 502, message: firstNonEmpty(params.Message, "Devin ACP error")}
	}
	return false, nil
}

// consumeClientToolCall bridges a call the agent made against our exposed MCP
// server into an OpenAI tool call, and ends the turn: the client runs the tool
// and sends its result back on the next request, which re-seeds the bridge.
func (s *devinCLISession) consumeClientToolCall(toolCallID, title string, rawInput jsontext.Value) (bool, error) {
	if toolCallID != "" && title != "" && strings.Contains(title, "from "+DevinClientToolsMCPName) {
		if match := devinCLIClientToolTitle.FindStringSubmatch(title); match != nil {
			s.pendingClientTools[toolCallID] = devinCLIFromMCPName(match[1])
		}
	}
	name := s.pendingClientTools[toolCallID]
	if name == "" || len(rawInput) == 0 {
		return false, nil
	}
	delete(s.pendingClientTools, toolCallID)
	id := toolCallID
	if id == "" {
		id = "call_" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	}
	if err := s.emit.toolCall(id, name, string(rawInput)); err != nil {
		return true, err
	}
	return true, s.emit.finish("tool_calls")
}
