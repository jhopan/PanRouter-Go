package executor

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
)

// devinCLIMaxFrameBytes bounds one ACP frame. A frame is one newline-terminated
// JSON-RPC document on the child's stdout; the tools it reports carry file
// contents, so the ceiling is generous but it is a ceiling: an unbounded read
// of a child's stdout is a request that cannot be cancelled and a buffer no
// bound ever releases.
const devinCLIMaxFrameBytes = 4 << 20

// devinCLIProtocolVersion and devinCLIClientInfo are what the CLI is told about
// its caller, verbatim from upstream (devin-cli.js:585-589). The CLI refuses an
// initialize that omits them.
const (
	devinCLIProtocolVersion = "0.3"
	devinCLIClientName      = "9router"
	devinCLIClientVersion   = "1.0"
)

// devinCLIRPCError is a JSON-RPC error object.
type devinCLIRPCError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    jsontext.Value `json:"data"`
}

func (e *devinCLIRPCError) Error() string {
	return fmt.Sprintf("Devin ACP error %d: %s", e.Code, e.Message)
}

// devinCLIRPCMessage is any JSON-RPC document the transport sees, in either
// direction: a request, a notification, a response or an error.
type devinCLIRPCMessage struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      jsontext.Value    `json:"id"`
	Method  string            `json:"method"`
	Params  jsontext.Value    `json:"params"`
	Result  jsontext.Value    `json:"result"`
	Error   *devinCLIRPCError `json:"error"`
}

// devinCLIRPCResponse is a JSON-RPC result this side sends.
type devinCLIRPCResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id"`
	Result  any            `json:"result"`
}

// devinCLIPermissionReply answers a session/request_permission.
type devinCLIPermissionReply struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id"`
	Result  struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	} `json:"result"`
}

// devinCLISession is one ACP conversation: the JSON-RPC state machine that
// walks initialize → session/new → session/prompt and folds the notifications
// that arrive in between into the emitter.
//
// It holds no goroutine of its own. Every method runs on the request's own
// goroutine, which is also the one reading the child's stdout, so the session
// state needs no lock — and the child is killed by the request's cancellation
// watcher, which is what unblocks that read.
type devinCLISession struct {
	proc  devinCLIProcess
	emit  *devinCLIEmitter
	model string
	cwd   string
	// prompt is the rendered conversation, sent as one text block.
	prompt string

	idCounter int
	initDone  bool
	sessionID string
	// sessionCreated gates the response branches: ACP answers are correlated
	// only by order here, exactly as upstream correlates them.
	sessionCreated bool
	finished       bool

	// clientTools enables the MCP bridge. pendingClientTools carries the ACP
	// tool_call id → original tool name across the two notifications the CLI
	// splits a call into (title first, rawInput on the update), because the
	// title is not repeated on the update.
	clientTools        bool
	pendingClientTools map[string]string
}

func newDevinCLISession(proc devinCLIProcess, emit *devinCLIEmitter, model, cwd string, clientTools bool) *devinCLISession {
	return &devinCLISession{
		proc: proc, emit: emit, model: model, cwd: cwd, idCounter: 1,
		clientTools:        clientTools,
		pendingClientTools: map[string]string{},
	}
}

// start opens the conversation.
func (s *devinCLISession) start() error {
	return s.send("initialize", map[string]any{
		"protocolVersion": devinCLIProtocolVersion,
		"clientInfo":      map[string]any{"name": devinCLIClientName, "version": devinCLIClientVersion},
		"capabilities":    map[string]any{},
	})
}

// send writes one request.
func (s *devinCLISession) send(method string, params any) error {
	id := s.idCounter
	s.idCounter++
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": id})
	if err != nil {
		return fmt.Errorf("devin-cli: marshal %s: %w", method, err)
	}
	if _, err := s.proc.Stdin().Write(append(frame, '\n')); err != nil {
		return fmt.Errorf("devin-cli: write %s: %w", method, err)
	}
	return nil
}

// reply writes one JSON-RPC result for a request the CLI made of us.
func (s *devinCLISession) reply(id jsontext.Value, payload any) error {
	frame, err := json.Marshal(devinCLIRPCResponse{JSONRPC: "2.0", ID: id, Result: payload})
	if err != nil {
		return fmt.Errorf("devin-cli: marshal reply: %w", err)
	}
	if _, err := s.proc.Stdin().Write(append(frame, '\n')); err != nil {
		return fmt.Errorf("devin-cli: write reply: %w", err)
	}
	return nil
}

// consume handles one frame from the child's stdout. The bool reports whether
// the turn is over, so the caller stops reading.
func (s *devinCLISession) consume(line []byte) (bool, error) {
	msg, err := devinCLIParseFrame(line)
	if err != nil {
		return false, err
	}
	if msg == nil {
		return false, nil // not a frame (banner text, a partial write)
	}
	if msg.Error != nil {
		return false, msg.Error
	}
	if msg.Method != "" {
		return s.consumeNotification(msg)
	}
	return s.consumeResponse(msg)
}

// consumeResponse walks the three responses the conversation waits for, in
// order: initialize, session/new, session/prompt.
func (s *devinCLISession) consumeResponse(msg *devinCLIRPCMessage) (bool, error) {
	switch {
	case !s.initDone:
		s.initDone = true
		// `mcpServers` is required by devin 3000.2.x and must be a sequence:
		// omitting it answers -32602 "missing field mcpServers". The servers
		// themselves come from the throwaway devin config file, not from here.
		return false, s.send("session/new", map[string]any{
			"cwd":        s.cwd,
			"mcpServers": []any{},
			"model":      s.model,
		})
	case !s.sessionCreated:
		var result struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(msg.Result, &result)
		if result.SessionID == "" {
			return false, &devinCLIError{status: 502, message: "Devin ACP: session/new returned no sessionId"}
		}
		s.sessionID = result.SessionID
		s.sessionCreated = true
		// `prompt` is a sequence of content blocks; devin 3000.2.x rejects the
		// older `content` field with -32602 "missing field prompt".
		return false, s.send("session/prompt", map[string]any{
			"sessionId": s.sessionID,
			"prompt":    []any{map[string]any{"type": "text", "text": s.prompt}},
		})
	}
	// The prompt is acknowledged with the final result. When the CLI streamed
	// nothing at all, the answer is recovered from that result instead.
	if !s.emit.answered() {
		var result devinCLIPromptResult
		_ = json.Unmarshal(msg.Result, &result)
		if text := devinCLIResultText(result); text != "" {
			if err := s.emit.delta(text); err != nil {
				return true, err
			}
		}
		if result.StopReason != "" && result.StopReason != "cancelled" {
			return true, s.emit.finish("stop")
		}
	}
	return false, nil
}

// done reports whether the turn already reached its terminal frame.
func (s *devinCLISession) done() bool { return s.finished }

// finish writes the terminal frames and closes the child's stdin, which is how
// the CLI is told the session is over.
func (s *devinCLISession) finish(reason string) error {
	if s.finished {
		return nil
	}
	s.finished = true
	return s.emit.finish(reason)
}
