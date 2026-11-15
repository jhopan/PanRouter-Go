package executor

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"panrouter/proxy/internal/proxy"
	"panrouter/proxy/internal/translator"
)

// devinCLIToolCall is one function call the agent asked its client to run.
type devinCLIToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// devinCLIEmitter turns the ACP turn into what the caller asked for: OpenAI
// chat-completion chunks when the request streams, one chat.completion document
// when it does not. The CLI has exactly one shape of its own (deltas of text
// plus tool calls), and both client shapes are projections of it.
//
// A Claude or Responses client is one hop further out: the chunks built here
// are translated on the way out, because a /v1/messages client cannot read an
// OpenAI chunk and a /v1/responses client cannot read either.
type devinCLIEmitter struct {
	req *Request
	w   http.ResponseWriter

	stream bool
	model  string
	id     string
	// created is the unix second every frame reports; the id's own timestamp
	// is separate so a frame written in the same second as the request is
	// still unique per request.
	created      int64
	promptTokens int

	hw      *proxy.HeartbeatWriter
	flusher http.Flusher
	opened  bool
	roleOut bool

	claude     bool
	sessionKey string
	bridge     *ResponsesBridge

	text      strings.Builder
	toolCalls []devinCLIToolCall
}

func newDevinCLIEmitter(req *Request, w http.ResponseWriter, model, prompt string) *devinCLIEmitter {
	e := &devinCLIEmitter{
		req:          req,
		w:            w,
		stream:       req.IsStream,
		model:        model,
		id:           "chatcmpl-devin-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		created:      time.Now().Unix(),
		promptTokens: devinCLITokenEstimate(prompt),
		claude:       req.TranslateResp,
		sessionKey:   fmt.Sprintf("devin-cli-stream-%d", time.Now().UnixNano()),
	}
	if e.stream && translator.NeedsResponsesBridge(req.Ctx) {
		e.bridge = NewResponsesBridge(
			translator.RequestedModelFromContext(req.Ctx),
			translator.CustomToolNamesFrom(req.Ctx),
			e.writeRawFrame,
		)
	}
	return e
}

// answered reports whether any content has been sent for this turn, which is
// also what the response branches use to decide whether the final ACP result
// still has to be surfaced (upstream's `roleEmitted`).
func (e *devinCLIEmitter) answered() bool { return e.roleOut }

// delta sends the next stretch of the answer.
func (e *devinCLIEmitter) delta(text string) error {
	e.text.WriteString(text)
	if !e.stream {
		return nil
	}
	// The response buffer is what the usage recorder and the request-details log
	// read the answer out of (forward.go, gemini_handler.go). The bridge path
	// records its own frames instead, so this must not run there or the log
	// would carry the answer twice in two shapes.
	if e.req.ResponseBuf != nil && e.bridge == nil {
		if _, err := e.req.ResponseBuf.Write([]byte(text)); err != nil {
			return err
		}
	}
	if err := e.openTurn(""); err != nil {
		return err
	}
	return e.writeChunk(map[string]any{"content": text}, nil, nil)
}

// openTurn writes the leading role chunk once: the empty-content chunk every
// OpenAI stream opens with, or the null-content one a tool-calling turn opens
// with. A client records the role from this frame alone.
func (e *devinCLIEmitter) openTurn(content any) error {
	if e.roleOut {
		return nil
	}
	e.roleOut = true
	return e.writeChunk(map[string]any{"role": "assistant", "content": content}, nil, nil)
}

// toolCall sends one function call and holds it for the non-streaming body.
// ACP calls are upsert-by-id, so a repeat of the same id replaces the earlier
// entry rather than queueing a second call the client would run twice.
func (e *devinCLIEmitter) toolCall(id, name, arguments string) error {
	call := devinCLIToolCall{ID: id, Name: name, Arguments: arguments}
	if arguments == "" {
		call.Arguments = "{}"
	}
	replaced := false
	for i := range e.toolCalls {
		if e.toolCalls[i].ID == id {
			e.toolCalls[i] = call
			replaced = true
			break
		}
	}
	if !replaced {
		e.toolCalls = append(e.toolCalls, call)
	}
	if !e.stream {
		return nil
	}
	if err := e.openTurn(nil); err != nil {
		return err
	}
	return e.writeChunk(map[string]any{
		"tool_calls": []any{map[string]any{
			"index": 0,
			"id":    id,
			"type":  "function",
			"function": map[string]any{
				"name":      name,
				"arguments": call.Arguments,
			},
		}},
	}, nil, nil)
}

// finish ends the turn: the terminal frame plus the usage estimate for a
// streaming client, or the whole completion document for a non-streaming one.
func (e *devinCLIEmitter) finish(reason string) error {
	if !e.stream {
		return e.writeCompletion(reason)
	}
	if err := e.writeChunk(map[string]any{}, reason, e.usage()); err != nil {
		return err
	}
	return e.closeStream()
}

// abort reports a failure.
//
// Before the first frame the failure is still a status: it is returned so the
// caller's handler answers with the lane's own error instead of a 200 that
// carries nothing. Afterwards HTTP 200 is already on the wire, and the failure
// travels as an error frame — a client left waiting for a terminator that never
// comes is the alternative.
func (e *devinCLIEmitter) abort(cause error) error {
	if cause == nil {
		return nil
	}
	if !e.opened {
		if e.hw != nil {
			_ = e.hw.Close()
		}
		return cause
	}
	format := proxy.SSEFormatOpenAI
	if e.claude {
		format = proxy.SSEFormatClaude
	}
	status, message := devinCLIAbortDetail(cause)
	// A Chat Completions client gets the lane's own frame, which is exactly what
	// upstream emits: the error object followed by [DONE]. The shared builder
	// types the failure as a generic upstream fault, and its "connection lost"
	// default would hide what the CLI actually said.
	if !e.claude && e.bridge == nil {
		if _, err := e.hw.Write(devinCLIErrorFrame(message)); err != nil {
			return cause
		}
	} else if _, err := e.hw.Write(proxy.BuildStreamErrorBytes(status, message, format)); err != nil {
		return cause
	}
	if e.flusher != nil {
		e.flusher.Flush()
	}
	_ = e.hw.Close()
	return cause
}

// devinCLIErrorFrame renders the terminal error frame upstream sends.
func devinCLIErrorFrame(message string) []byte {
	payload := (&devinCLIError{message: message}).body()
	return fmt.Appendf(nil, "data: %s\n\ndata: [DONE]\n\n", payload)
}

// devinCLIAbortDetail keeps the lane's own sentence for a failure it owns.
//
// ClassifyStreamAbort answers a generic status and "upstream connection lost"
// for anything that is not a stall or a cancel, which would replace the CLI's
// actual complaint ("Devin ACP error -32602: Invalid params", the exit code and
// stderr tail) with a sentence that names nothing.
func devinCLIAbortDetail(cause error) (int, string) {
	status, message := proxy.ClassifyStreamAbort(cause)
	var lane *devinCLIError
	switch {
	case errors.As(cause, &lane) && lane.message != "":
		if lane.status > 0 {
			status = lane.status
		}
		return status, lane.message
	case errors.Is(cause, context.Canceled):
		return status, message
	case cause.Error() != "":
		return status, cause.Error()
	}
	return status, message
}

// writeChunk writes one OpenAI chunk in whichever shape the client asked for.
func (e *devinCLIEmitter) writeChunk(delta map[string]any, finish any, usage map[string]any) error {
	if delta == nil {
		delta = map[string]any{}
	}
	chunk := map[string]any{
		"id":      e.id,
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("devin-cli: marshal chunk: %w", err)
	}
	if e.bridge != nil {
		e.bridge.Feed(encoded)
		return e.bridge.Err()
	}
	frame := fmt.Appendf(nil, "data: %s\n\n", encoded)
	if e.claude {
		translated, terr := translator.TranslateOpenAIToClaudeStreamSession(e.sessionKey, encoded)
		if terr != nil {
			return terr
		}
		if translated != nil {
			frame = translated
		}
	}
	if err := e.open(); err != nil {
		return err
	}
	markTTFT(e.req, e.req.StartTime)
	if _, err := e.hw.Write(frame); err != nil {
		return err
	}
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return nil
}

// writeRawFrame hands one Responses event to the client.
func (e *devinCLIEmitter) writeRawFrame(frame []byte) error {
	if err := e.open(); err != nil {
		return err
	}
	if e.req.ResponseBuf != nil {
		_, _ = e.req.ResponseBuf.Write(frame)
	}
	_, err := e.hw.Write(frame)
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return err
}

// open writes the SSE headers on the first frame the client will see, so a turn
// that fails first still has a status to report.
func (e *devinCLIEmitter) open() error {
	if e.opened {
		return nil
	}
	e.opened = true
	e.hw = proxy.NewHeartbeatWriter(e.req.Ctx, e.w, 0)
	e.flusher = proxy.WriteSSEHeaders(e.hw)
	return nil
}

// closeStream writes the terminator the client's format requires.
func (e *devinCLIEmitter) closeStream() error {
	if e.bridge != nil {
		e.bridge.StopCompletionWatchdog()
		e.bridge.Close()
		return e.hw.Close()
	}
	if e.claude {
		if end := translator.EnsureStreamClosed(e.sessionKey); len(end) > 0 {
			if _, err := e.hw.Write(end); err != nil {
				return err
			}
		}
		translator.ClearStreamState(e.sessionKey)
		return e.hw.Close()
	}
	if _, err := e.hw.Write([]byte("data: [DONE]\n\n")); err != nil {
		return err
	}
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return e.hw.Close()
}
