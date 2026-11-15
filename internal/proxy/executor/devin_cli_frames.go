package executor

import (
	"bufio"
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
)

// devinCLIParseFrame decodes one stdout line.
//
// A line that is not JSON is not a frame — the CLI writes banners and progress
// text there — and is ignored, exactly as upstream ignores it. A line that IS
// JSON but not a JSON-RPC message is a protocol violation and is reported:
// silently dropping it would leave the request waiting for an answer that can
// no longer arrive.
func devinCLIParseFrame(line []byte) (*devinCLIRPCMessage, error) {
	var msg devinCLIRPCMessage
	err := json.Unmarshal(line, &msg)
	if err == nil && (msg.Method != "" || msg.Error != nil || len(msg.Result) > 0) {
		return &msg, nil
	}
	raw := jsontext.Value(line)
	if !raw.IsValid() {
		return nil, nil
	}
	if err != nil {
		return nil, &devinCLIError{status: 502, message: fmt.Sprintf("Devin ACP malformed frame: %v", err)}
	}
	return nil, &devinCLIError{status: 502, message: "Devin ACP malformed frame: not a JSON-RPC message"}
}

// devinCLITextContent reads a content field that is either a plain string
// (older devin) or a typed block ({"type":"text","text":"..."}).
func devinCLITextContent(value jsontext.Value) string {
	if len(value) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(value, &text); err == nil {
		return text
	}
	var block struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(value, &block); err != nil {
		return ""
	}
	return block.Text
}

// devinCLIPromptResult is the final session/prompt result, across the shapes
// the CLI has used for the answer it carries.
type devinCLIPromptResult struct {
	Content    string `json:"content"`
	Text       string `json:"text"`
	StopReason string `json:"stopReason"`
	Message    struct {
		Content string `json:"content"`
	} `json:"message"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// devinCLIResultText reads the answer out of a session/prompt result, across
// the shapes the CLI has used for it.
func devinCLIResultText(result devinCLIPromptResult) string {
	if result.Content != "" {
		return result.Content
	}
	if result.Text != "" {
		return result.Text
	}
	if result.Message.Content != "" {
		return result.Message.Content
	}
	var assistant []string
	for _, message := range result.Messages {
		if message.Role == "assistant" {
			assistant = append(assistant, message.Content)
		}
	}
	return strings.Join(assistant, "\n")
}

// drain reads frames from the child until the turn ends, the child closes its
// stdout, or the transport fails. onIdle runs when the stream ends without a
// terminal frame, so the caller decides what an early close means.
func (s *devinCLISession) drain(stdout io.Reader, onIdle func() error) error {
	reader := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, err := devinCLIReadFrame(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return onIdle()
			}
			return err
		}
		if len(line) == 0 {
			continue
		}
		done, err := s.consume(line)
		if err != nil || done {
			return err
		}
	}
}

// devinCLIReadFrame reads one newline-terminated frame with a bounded buffer.
func devinCLIReadFrame(reader *bufio.Reader) ([]byte, error) {
	var frame []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		frame = append(frame, chunk...)
		if len(frame) > devinCLIMaxFrameBytes {
			return nil, &devinCLIError{status: 502,
				message: fmt.Sprintf("Devin ACP frame exceeds %d bytes", devinCLIMaxFrameBytes)}
		}
		if err == nil {
			return bytes.TrimSpace(frame), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(bytes.TrimSpace(frame)) > 0 {
			return bytes.TrimSpace(frame), nil
		}
		return nil, err
	}
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
