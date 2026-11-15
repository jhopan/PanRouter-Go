package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"encoding/json/jsontext"
	json "encoding/json/v2"

	"panrouter/proxy/internal/providers"
	"panrouter/proxy/internal/proxy"
)

// --- fake ACP peer -------------------------------------------------------

// devinFakePeer is the child a test stands in for the `devin` binary: two pipes
// per direction, a handler that answers frames, and a lifecycle that reports
// exactly what the real child reports (an exit code, a stderr tail, a kill).
//
// Every field is touched from the executor's request goroutine and from the
// peer's own goroutine, so the mutexes here are load-bearing.
type devinFakePeer struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stderrR *io.PipeReader
	stderrW *io.PipeWriter

	mu      sync.Mutex
	frames  []string
	stderr  string
	code    int
	killed  bool
	waited  bool
	handled func(*devinFakePeer, devinCLIRPCMessage)

	// handlers decouples reading stdin from answering it. The real CLI reads
	// and writes at the same time, and so must the fake: an answer written
	// while the reader is parked on the same stdout pipe deadlocks the moment
	// the executor answers a request of ours (the permission reply).
	handlers chan devinCLIRPCMessage

	exited     chan struct{}
	closeOne   sync.Once
	closeQueue sync.Once
	peerWG     sync.WaitGroup
}

func newDevinFakePeer(handled func(*devinFakePeer, devinCLIRPCMessage)) *devinFakePeer {
	peer := &devinFakePeer{
		exited:   make(chan struct{}),
		handled:  handled,
		handlers: make(chan devinCLIRPCMessage, 64),
	}
	peer.stdinR, peer.stdinW = io.Pipe()
	peer.stdoutR, peer.stdoutW = io.Pipe()
	peer.stderrR, peer.stderrW = io.Pipe()

	peer.peerWG.Add(1)
	go func() {
		defer peer.peerWG.Done()
		defer peer.closeHandlerQueue()
		scanner := bufio.NewScanner(peer.stdinR)
		scanner.Buffer(make([]byte, 0, 64<<10), devinCLIMaxFrameBytes)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var msg devinCLIRPCMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			peer.mu.Lock()
			peer.frames = append(peer.frames, line)
			peer.mu.Unlock()
			peer.handlers <- msg
		}
		// stdin closed: the real binary exits, so the peer does too.
		peer.exit(0, "")
	}()

	peer.peerWG.Add(1)
	go func() {
		defer peer.peerWG.Done()
		for msg := range peer.handlers {
			peer.handled(peer, msg)
		}
	}()
	return peer
}

// closeHandlerQueue stops the answer worker.
func (p *devinFakePeer) closeHandlerQueue() {
	p.closeQueue.Do(func() { close(p.handlers) })
}

// stop ends the peer and waits for both of its goroutines, so a test can prove
// nothing was left running.
func (p *devinFakePeer) stop() {
	p.exit(0, "")
	p.closeHandlerQueue()
	p.peerWG.Wait()
}

func (p *devinFakePeer) Stdin() io.WriteCloser { return p.stdinW }
func (p *devinFakePeer) Stdout() io.ReadCloser { return p.stdoutR }
func (p *devinFakePeer) Stderr() io.ReadCloser { return p.stderrR }

// Wait blocks until the peer is gone, and reports the exit status it was given.
func (p *devinFakePeer) Wait() error {
	<-p.exited
	p.mu.Lock()
	p.waited = true
	code := p.code
	p.mu.Unlock()
	if code == 0 {
		return nil
	}
	return &devinCLIProcessExit{code: code, err: fmt.Errorf("exit status %d", code)}
}

// wasWaited reports whether the request reaped the child.
func (p *devinFakePeer) wasWaited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waited
}

// Kill is what the request does on cancellation.
func (p *devinFakePeer) Kill() error {
	p.mu.Lock()
	p.killed = true
	p.code = -1
	p.mu.Unlock()
	p.exit(-1, "")
	return nil
}

// exit closes the peer's streams, which is what ends the executor's read.
func (p *devinFakePeer) exit(code int, stderr string) {
	p.closeOne.Do(func() {
		p.mu.Lock()
		if stderr != "" {
			p.stderr = stderr
		}
		if code != 0 {
			p.code = code
		}
		p.mu.Unlock()
		if stderr != "" {
			_, _ = io.WriteString(p.stderrW, stderr)
		}
		_ = p.stderrW.Close()
		_ = p.stdoutW.Close()
		_ = p.stdinR.Close()
		close(p.exited)
	})
}

// send writes one frame to the executor.
func (p *devinFakePeer) send(payload map[string]any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = p.stdoutW.Write(append(encoded, '\n'))
}

// sendRaw writes bytes verbatim, for the malformed-frame cases.
func (p *devinFakePeer) sendRaw(line string) {
	_, _ = io.WriteString(p.stdoutW, line+"\n")
}

func (p *devinFakePeer) writtenFrames() []devinCLIRPCMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]devinCLIRPCMessage, 0, len(p.frames))
	for _, frame := range p.frames {
		var msg devinCLIRPCMessage
		if err := json.Unmarshal([]byte(frame), &msg); err != nil {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func (p *devinFakePeer) wasKilled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

// --- fake ACP conversations ---------------------------------------------

// devinFakeAnswer is a well-behaved agent: it answers initialize and
// session/new, validates the two fields devin 3000.2.x rejects when they are
// wrong, asks for permission once, streams a thought plus an answer, and stops.
func devinFakeAnswer(peer *devinFakePeer, msg devinCLIRPCMessage) {
	switch msg.Method {
	case "initialize":
		peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID), "result": map[string]any{"protocolVersion": 1}})
	case "session/new":
		var params struct {
			MCPServers jsonIDList `json:"mcpServers"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		if !params.MCPServers.ok {
			peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID),
				"error": map[string]any{"code": -32602, "message": "Invalid params",
					"data": map[string]any{"error": "missing field `mcpServers`"}}})
			return
		}
		peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID), "result": map[string]any{"sessionId": "fake-session"}})
	case "session/prompt":
		var params struct {
			Prompt []map[string]any `json:"prompt"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		if len(params.Prompt) == 0 {
			peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID),
				"error": map[string]any{"code": -32602, "message": "Invalid params",
					"data": map[string]any{"error": "missing field `prompt`"}}})
			return
		}
		peer.send(map[string]any{"jsonrpc": "2.0", "id": 777, "method": "session/request_permission",
			"params": map[string]any{"sessionId": "fake-session", "options": []map[string]any{
				{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"},
				{"optionId": "reject-once", "name": "Reject", "kind": "reject_once"},
			}}})
		peer.send(map[string]any{"jsonrpc": "2.0", "method": "session/update",
			"params": map[string]any{"sessionId": "fake-session", "update": map[string]any{
				"sessionUpdate": "agent_thought_chunk",
				"content":       map[string]any{"type": "text", "text": "(thinking)"},
			}}})
		peer.send(map[string]any{"jsonrpc": "2.0", "method": "session/update",
			"params": map[string]any{"sessionId": "fake-session", "update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": "hello "},
			}}})
		peer.send(map[string]any{"jsonrpc": "2.0", "method": "session/update",
			"params": map[string]any{"sessionId": "fake-session", "update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": "world"},
			}}})
		peer.send(map[string]any{"jsonrpc": "2.0", "method": "_cognition.ai/agent_stopped",
			"params": map[string]any{"cause": "complete"}})
	}
}

// jsonIDList records whether a JSON array was present at all, which is the
// difference devin cares about between "missing mcpServers" and "wrong type".
type jsonIDList struct {
	ok bool
}

func (l *jsonIDList) UnmarshalJSON(data []byte) error {
	l.ok = len(data) > 0 && data[0] == '['
	return nil
}

// rawID keeps an id verbatim so an answer echoes the exact number or string.
func rawID(id []byte) any {
	if len(id) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(id, &value); err != nil {
		return nil
	}
	return value
}

// --- harness -------------------------------------------------------------

type devinCLITestRun struct {
	rec      *httptest.ResponseRecorder
	err      error
	spec     devinCLISpec
	config   string // $XDG_CONFIG_HOME/devin/config.json, read at spawn time
	peer     *devinFakePeer
	finished chan struct{}
}

// devinCLIHarness runs one request to completion against a fake peer.
func devinCLIHarness(t *testing.T, ctx context.Context, body string, stream bool, handle func(*devinFakePeer, devinCLIRPCMessage)) *devinCLITestRun {
	t.Helper()
	run := devinCLIStart(t, ctx, body, stream, handle)
	<-run.finished
	return run
}

// devinCLIStart starts one request in its own goroutine. A test that has to act
// while the turn is in flight — cancellation — needs the request off its own
// goroutine, and every assertion then reads the run after <-run.finished, which
// is the happens-before edge.
func devinCLIStart(t *testing.T, ctx context.Context, body string, stream bool, handle func(*devinFakePeer, devinCLIRPCMessage)) *devinCLITestRun {
	t.Helper()
	t.Setenv("CLI_DEVIN_BIN", filepath.Join(t.TempDir(), "devin"))
	t.Setenv("CLI_DEVIN_AGENT_TYPE", "")
	t.Setenv("DEVIN_MCP_SERVERS", "")
	t.Setenv("DEVIN_PERMISSION_MODE", "")

	run := &devinCLITestRun{peer: newDevinFakePeer(handle), finished: make(chan struct{})}
	previous := devinCLISpawn
	devinCLISpawn = func(spec devinCLISpec) (devinCLIProcess, error) {
		run.spec = spec
		for _, entry := range spec.Env {
			if configDir, ok := strings.CutPrefix(entry, "XDG_CONFIG_HOME="); ok {
				raw, err := os.ReadFile(filepath.Join(configDir, "devin", "config.json"))
				if err == nil {
					run.config = string(raw)
				}
			}
		}
		return run.peer, nil
	}
	t.Cleanup(func() {
		devinCLISpawn = previous
		run.peer.stop()
	})

	req := &Request{
		Ctx:       ctx,
		Config:    &providers.ProviderConfig{BaseURL: "devin://acp/stdio", NoAuth: true},
		Provider:  "devin-cli",
		APIKey:    "public",
		Body:      []byte(body),
		IsStream:  stream,
		ModelName: "swe-1.6-fast",
		StartTime: time.Now(),
	}
	run.rec = httptest.NewRecorder()
	go func() {
		defer close(run.finished)
		run.err = ForwardDevinCLI(run.rec, req)
	}()
	return run
}

// sseFrames splits a body into the JSON payloads of its SSE data frames.
func sseFrames(t *testing.T, body string) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if !strings.HasPrefix(block, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(block, "data: ")
		if payload == "[DONE]" {
			frames = append(frames, map[string]any{"done": true})
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatalf("frame is not JSON: %v (%s)", err, payload)
		}
		frames = append(frames, decoded)
	}
	return frames
}

// sseDeltaText concatenates the content deltas of an OpenAI SSE body.
func sseDeltaText(t *testing.T, body string) string {
	t.Helper()
	var builder strings.Builder
	for _, frame := range sseFrames(t, body) {
		choices, _ := frame["choices"].([]any)
		for _, choice := range choices {
			entry, _ := choice.(map[string]any)
			delta, _ := entry["delta"].(map[string]any)
			if text, ok := delta["content"].(string); ok {
				builder.WriteString(text)
			}
		}
	}
	return builder.String()
}

// --- protocol ------------------------------------------------------------

// TestForwardDevinCLI_ProtocolSequence pins the ACP conversation: the three
// requests, in order, with the params the CLI requires — `mcpServers` as a
// sequence and the prompt as a sequence being the two devin 3000.2.x refuses
// with -32602 when they are missing.
func TestForwardDevinCLI_ProtocolSequence(t *testing.T) {
	run := devinCLIHarness(t, context.Background(),
		`{"model":"swe-1.6-fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		true, devinFakeAnswer)

	if run.err != nil {
		t.Fatalf("ForwardDevinCLI() error = %v", run.err)
	}
	if run.spec.Args[0] != "acp" {
		t.Errorf("argv = %v, want it to start with acp", run.spec.Args)
	}
	if run.spec.Dir != os.TempDir() {
		t.Errorf("spawn cwd = %q, want the temp dir when the request names no workspace", run.spec.Dir)
	}
	if !strings.Contains(strings.Join(run.spec.Env, "\x00"), "DEVIN_PERMISSION_MODE=bypass") {
		t.Error("child environment lacks DEVIN_PERMISSION_MODE=bypass, so the agent would block on the first tool prompt")
	}
	for _, entry := range run.spec.Env {
		if strings.HasPrefix(entry, "WINDSURF_API_KEY=") {
			t.Error("a provider key was injected; the CLI must fall back to the credentials `devin auth login` stored")
		}
	}

	var methods []string
	for _, msg := range run.peer.writtenFrames() {
		if msg.Method != "" {
			methods = append(methods, msg.Method)
		}
	}
	want := []string{"initialize", "session/new", "session/prompt"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("requests = %v, want %v", methods, want)
	}

	initialize := devinCLIFrame(t, run.peer, "initialize")
	var initParams struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	decodeParams(t, initialize, &initParams)
	if initParams.ProtocolVersion != "0.3" || initParams.ClientInfo.Name != "9router" {
		t.Errorf("initialize params = %+v, want protocolVersion 0.3 from 9router", initParams)
	}

	newSession := devinCLIFrame(t, run.peer, "session/new")
	var newParams struct {
		Cwd        string         `json:"cwd"`
		Model      string         `json:"model"`
		MCPServers jsontext.Value `json:"mcpServers"`
	}
	decodeParams(t, newSession, &newParams)
	if string(newParams.MCPServers) != "[]" {
		t.Errorf("session/new mcpServers = %s, want a sequence (devin rejects a missing one with -32602)", newParams.MCPServers)
	}
	if newParams.Model != "swe-1.6-fast" || newParams.Cwd != os.TempDir() {
		t.Errorf("session/new = %+v, want the requested model and the workspace cwd", newParams)
	}

	prompt := devinCLIFrame(t, run.peer, "session/prompt")
	var promptParams struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	decodeParams(t, prompt, &promptParams)
	if promptParams.SessionID != "fake-session" {
		t.Errorf("session/prompt sessionId = %q, want the one session/new returned", promptParams.SessionID)
	}
	if len(promptParams.Prompt) != 1 || promptParams.Prompt[0].Text != "[User]\nhi" {
		t.Errorf("session/prompt prompt = %+v, want the rendered conversation as one text block", promptParams.Prompt)
	}
}

// TestForwardDevinCLI_StreamFrames pins the OpenAI stream: role chunk first,
// the answer's deltas in order, the terminal chunk, [DONE] last, and the
// agent's private thinking nowhere in it.
func TestForwardDevinCLI_StreamFrames(t *testing.T) {
	run := devinCLIHarness(t, context.Background(),
		`{"messages":[{"role":"user","content":"hi"}]}`, true, devinFakeAnswer)
	if run.err != nil {
		t.Fatalf("ForwardDevinCLI() error = %v", run.err)
	}
	body := run.rec.Body.String()
	if got := run.rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	frames := sseFrames(t, body)
	if len(frames) < 4 {
		t.Fatalf("only %d frames: %s", len(frames), body)
	}
	role := frames[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if role["role"] != "assistant" {
		t.Errorf("first delta = %v, want the assistant role chunk", role)
	}
	// The two agent_message_chunks arrive as two content deltas, in order.
	var deltas []string
	for _, frame := range frames {
		choices, ok := frame["choices"].([]any)
		if !ok {
			continue
		}
		delta := choices[0].(map[string]any)["delta"].(map[string]any)
		if text, ok := delta["content"].(string); ok {
			deltas = append(deltas, text)
		}
	}
	if strings.Join(deltas, "|") != "|hello |world" {
		t.Errorf("content deltas = %v, want the empty role delta then the chunks in order", deltas)
	}
	if got := sseDeltaText(t, body); got != "hello world" {
		t.Errorf("streamed text = %q, want %q", got, "hello world")
	}
	if strings.Contains(body, "(thinking)") {
		t.Error("agent_thought_chunk reached the client; internal thinking is not an answer")
	}
	terminal := frames[len(frames)-2]
	choices := terminal["choices"].([]any)[0].(map[string]any)
	if choices["finish_reason"] != "stop" {
		t.Errorf("terminal frame = %v, want finish_reason stop", terminal)
	}
	if frames[len(frames)-1]["done"] != true {
		t.Errorf("last frame = %v, want [DONE]", frames[len(frames)-1])
	}
	usage, _ := terminal["usage"].(map[string]any)
	if usage == nil || usage["estimated"] != true {
		t.Errorf("terminal usage = %v, want the flagged estimate upstream reports", usage)
	}
}

// TestForwardDevinCLI_PermissionAutoApproved pins the reply that keeps a tool
// call from hanging: the CLI waits for an answer to its permission request.
func TestForwardDevinCLI_PermissionAutoApproved(t *testing.T) {
	run := devinCLIHarness(t, context.Background(),
		`{"messages":[{"role":"user","content":"hi"}]}`, true, devinFakeAnswer)
	if run.err != nil {
		t.Fatalf("ForwardDevinCLI() error = %v", run.err)
	}
	for _, msg := range run.peer.writtenFrames() {
		if msg.Method != "" || len(msg.ID) == 0 {
			continue
		}
		var id any
		_ = json.Unmarshal(msg.ID, &id)
		if id != float64(777) {
			continue
		}
		var reply struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		if err := json.Unmarshal(msg.Result, &reply); err != nil {
			t.Fatalf("permission reply is not a JSON-RPC result: %v", err)
		}
		if reply.Outcome.Outcome != "selected" || reply.Outcome.OptionID != "allow-once" {
			t.Fatalf("permission reply = %+v, want the first allow option selected", reply.Outcome)
		}
		return
	}
	t.Fatal("no answer to session/request_permission: the CLI would block on it forever")
}

// TestForwardDevinCLI_WorkspaceCwdFromEnvContext pins the workspace the child is
// spawned in: an agent client's `<cwd>` block decides it, so its file tools land
// in the caller's project instead of the temp directory.
func TestForwardDevinCLI_WorkspaceCwdFromEnvContext(t *testing.T) {
	workspace := t.TempDir()
	// The client writes the path in the block, so the path the request carries
	// back is the one it sent — separators included.
	want := filepath.ToSlash(workspace)
	body := fmt.Sprintf(`{"messages":[{"role":"user","content":"<environment_context>\n  <cwd>%s</cwd>\n</environment_context>\nhi"}]}`, want)
	run := devinCLIHarness(t, context.Background(), body, true, devinFakeAnswer)
	if run.err != nil {
		t.Fatalf("ForwardDevinCLI() error = %v", run.err)
	}
	if run.spec.Dir != want {
		t.Errorf("spawn cwd = %q, want %q", run.spec.Dir, want)
	}
	newSession := devinCLIFrame(t, run.peer, "session/new")
	var params struct {
		Cwd string `json:"cwd"`
	}
	decodeParams(t, newSession, &params)
	if params.Cwd != want {
		t.Errorf("session/new cwd = %q, want %q", params.Cwd, want)
	}
}

// TestForwardDevinCLI_NonStreamAggregatesOneCompletion pins the non-streaming
// shape: one chat.completion document, not the SSE frames a streaming caller
// gets.
func TestForwardDevinCLI_NonStreamAggregatesOneCompletion(t *testing.T) {
	run := devinCLIHarness(t, context.Background(),
		`{"messages":[{"role":"user","content":"hi"}]}`, false, devinFakeAnswer)
	if run.err != nil {
		t.Fatalf("ForwardDevinCLI() error = %v", run.err)
	}
	body := run.rec.Body.String()
	if strings.Contains(body, "chat.completion.chunk") {
		t.Fatalf("a non-streaming caller got SSE chunks:\n%s", body)
	}
	var completion struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &completion); err != nil {
		t.Fatalf("body is not a completion: %v (%s)", err, body)
	}
	if completion.Object != "chat.completion" || len(completion.Choices) != 1 {
		t.Fatalf("completion = %+v, want one chat.completion choice", completion)
	}
	if completion.Choices[0].Message.Content != "hello world" || completion.Choices[0].FinishReason != "stop" {
		t.Errorf("choice = %+v, want the whole answer and a stop reason", completion.Choices[0])
	}
	if completion.Usage["estimated"] != true {
		t.Errorf("usage = %v, want the flagged estimate", completion.Usage)
	}
}

// TestForwardDevinCLI_ACPPromptResultSurfaced covers the agent that answers in
// its final result instead of streaming: without this the caller got an empty
// completion.
func TestForwardDevinCLI_ACPPromptResultSurfaced(t *testing.T) {
	run := devinCLIHarness(t, context.Background(),
		`{"messages":[{"role":"user","content":"hi"}]}`, true,
		func(peer *devinFakePeer, msg devinCLIRPCMessage) {
			switch msg.Method {
			case "initialize":
				peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID), "result": map[string]any{}})
			case "session/new":
				peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID), "result": map[string]any{"sessionId": "s1"}})
			case "session/prompt":
				peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID),
					"result": map[string]any{"content": "buffered answer", "stopReason": "end_turn"}})
			}
		})
	if run.err != nil {
		t.Fatalf("ForwardDevinCLI() error = %v", run.err)
	}
	if got := sseDeltaText(t, run.rec.Body.String()); got != "buffered answer" {
		t.Errorf("streamed text = %q, want the final ACP result's content", got)
	}
}

// --- tool bridge ---------------------------------------------------------

// TestForwardDevinCLI_ClientToolsExposedAsMCP pins the bridge configuration: the
// caller's OpenAI tools become a `clientTools` MCP server the CLI can discover,
// written into a throwaway devin config that XDG_CONFIG_HOME points at.
func TestForwardDevinCLI_ClientToolsExposedAsMCP(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function",
		"function":{"name":"get_weather","description":"Get weather",
		"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`
	run := devinCLIHarness(t, context.Background(), body, true, devinFakeAnswer)
	if run.err != nil {
		t.Fatalf("ForwardDevinCLI() error = %v", run.err)
	}
	if !strings.Contains(strings.Join(run.spec.Env, "\x00"), "XDG_CONFIG_HOME=") {
		t.Fatal("no XDG_CONFIG_HOME: the CLI would never load the generated MCP config")
	}
	if run.config == "" {
		t.Fatal("no config.json was written next to XDG_CONFIG_HOME")
	}
	var config struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(run.config), &config); err != nil {
		t.Fatalf("config is not JSON: %v (%s)", err, run.config)
	}
	bridge, ok := config.MCPServers[DevinClientToolsMCPName]
	if !ok {
		t.Fatalf("config has no %s server: %s", DevinClientToolsMCPName, run.config)
	}
	if bridge.Command == "" || len(bridge.Args) != 1 || bridge.Args[0] != DevinClientToolsMCPArg {
		t.Errorf("bridge server = %+v, want this binary in its MCP helper mode", bridge)
	}
	var declarations []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema jsontext.Value `json:"inputSchema"`
	}
	if err := json.Unmarshal([]byte(bridge.Env["DEVIN_MCP_TOOLS"]), &declarations); err != nil {
		t.Fatalf("DEVIN_MCP_TOOLS is not a declaration list: %v (%s)", err, bridge.Env["DEVIN_MCP_TOOLS"])
	}
	if len(declarations) != 1 || declarations[0].Name != "mcp_get_weather" {
		t.Fatalf("declarations = %+v, want the OpenAI tool under its mcp_ name", declarations)
	}
	if !strings.Contains(string(declarations[0].InputSchema), `"city"`) {
		t.Errorf("inputSchema = %s, want the caller's JSON schema", declarations[0].InputSchema)
	}
}

// TestForwardDevinCLI_SeedsClientToolResults pins the second half of the
// round-trip: a tool result the client already produced is seeded into the MCP
// server, and the replayed history tells the agent what its tool returned.
func TestForwardDevinCLI_SeedsClientToolResults(t *testing.T) {
	body := `{"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function",
			"function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"28C sunny"}],
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
	run := devinCLIHarness(t, context.Background(), body, true, devinFakeAnswer)
	if run.err != nil {
		t.Fatalf("ForwardDevinCLI() error = %v", run.err)
	}
	var config struct {
		MCPServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(run.config), &config); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	seeded := map[string]string{}
	if err := json.Unmarshal([]byte(config.MCPServers[DevinClientToolsMCPName].Env["DEVIN_MCP_RESULTS"]), &seeded); err != nil {
		t.Fatalf("DEVIN_MCP_RESULTS is not a result map: %v (%s)", err,
			config.MCPServers[DevinClientToolsMCPName].Env["DEVIN_MCP_RESULTS"])
	}
	if seeded["mcp_get_weather"] != "28C sunny" {
		t.Fatalf("seeded results = %v, want mcp_get_weather seeded with the client's answer", seeded)
	}

	prompt := devinCLIFrame(t, run.peer, "session/prompt")
	var params struct {
		Prompt []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}
	decodeParams(t, prompt, &params)
	text := params.Prompt[0].Text
	for _, want := range []string{"get_weather", `{"city":"Paris"}`, "call_1", "28C sunny"} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt lacks %q:\n%s", want, text)
		}
	}
}

// TestForwardDevinCLI_ToolCallBecomesOpenAIToolCall pins the bridge's other
// direction: the agent calling our exposed MCP tool surfaces as the OpenAI tool
// call the client already knows how to run, and the turn ends on it.
func TestForwardDevinCLI_ToolCallBecomesOpenAIToolCall(t *testing.T) {
	run := devinCLIHarness(t, context.Background(),
		`{"messages":[{"role":"user","content":"weather?"}],
			"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`,
		true, func(peer *devinFakePeer, msg devinCLIRPCMessage) {
			switch msg.Method {
			case "initialize":
				peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID), "result": map[string]any{}})
			case "session/new":
				peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID), "result": map[string]any{"sessionId": "s1"}})
			case "session/prompt":
				// The CLI splits a call in two: title first, rawInput on the update.
				peer.send(map[string]any{"jsonrpc": "2.0", "method": "session/update",
					"params": map[string]any{"sessionId": "s1", "update": map[string]any{
						"sessionUpdate": "tool_call", "toolCallId": "call_abc",
						"title": "Calling mcp_get_weather from clientTools",
					}}})
				peer.send(map[string]any{"jsonrpc": "2.0", "method": "session/update",
					"params": map[string]any{"sessionId": "s1", "update": map[string]any{
						"sessionUpdate": "tool_call_update", "toolCallId": "call_abc",
						"rawInput": map[string]any{"city": "Paris"},
					}}})
			}
		})
	if run.err != nil {
		t.Fatalf("ForwardDevinCLI() error = %v", run.err)
	}
	body := run.rec.Body.String()
	frames := sseFrames(t, body)
	var call map[string]any
	var finish any
	for _, frame := range frames {
		choices, ok := frame["choices"].([]any)
		if !ok {
			continue
		}
		for _, choice := range choices {
			entry := choice.(map[string]any)
			delta, _ := entry["delta"].(map[string]any)
			if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
				if call != nil {
					t.Fatal("two tool_call frames for one ACP call")
				}
				call = calls[0].(map[string]any)
			}
			if reason, ok := entry["finish_reason"]; ok && reason != nil {
				finish = reason
			}
		}
	}
	if call == nil {
		t.Fatalf("no tool_calls delta in:\n%s", body)
	}
	if call["id"] != "call_abc" || call["type"] != "function" {
		t.Errorf("tool call = %v, want the ACP id and the OpenAI function type", call)
	}
	function := call["function"].(map[string]any)
	if function["name"] != "get_weather" {
		t.Errorf("tool name = %v, want the mcp_ prefix stripped", function["name"])
	}
	if function["arguments"] != `{"city":"Paris"}` {
		t.Errorf("arguments = %v, want the ACP rawInput", function["arguments"])
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", finish)
	}
	if frames[len(frames)-1]["done"] != true {
		t.Error("the turn did not end with [DONE]")
	}
}

// TestForwardDevinCLI_ToolCallNonStream pins the same call in the buffered shape.
func TestForwardDevinCLI_ToolCallNonStream(t *testing.T) {
	run := devinCLIHarness(t, context.Background(),
		`{"messages":[{"role":"user","content":"weather?"}],
			"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`,
		false, func(peer *devinFakePeer, msg devinCLIRPCMessage) {
			switch msg.Method {
			case "initialize":
				peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID), "result": map[string]any{}})
			case "session/new":
				peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID), "result": map[string]any{"sessionId": "s1"}})
			case "session/prompt":
				peer.send(map[string]any{"jsonrpc": "2.0", "method": "session/update",
					"params": map[string]any{"sessionId": "s1", "update": map[string]any{
						"sessionUpdate": "tool_call", "toolCallId": "call_abc",
						"title": "Calling mcp_get_weather from clientTools", "rawInput": map[string]any{"city": "Paris"},
					}}})
			}
		})
	if run.err != nil {
		t.Fatalf("ForwardDevinCLI() error = %v", run.err)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content   any `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(run.rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("body is not a completion: %v (%s)", err, run.rec.Body.String())
	}
	if len(completion.Choices) != 1 || len(completion.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("completion = %+v, want one choice with one tool call", completion)
	}
	if completion.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", completion.Choices[0].FinishReason)
	}
	if completion.Choices[0].Message.Content != nil {
		t.Errorf("content = %v, want null alongside a tool call", completion.Choices[0].Message.Content)
	}
	if completion.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("tool = %+v, want the client's own tool name", completion.Choices[0].Message.ToolCalls[0])
	}
}

// --- failure paths -------------------------------------------------------

// TestForwardDevinCLI_MissingBinary pins the lane's own error: a CLI that is not
// installed is the expected failure here, so the client is told which binary was
// looked for and how to get it instead of a blanket gateway error.
func TestForwardDevinCLI_MissingBinary(t *testing.T) {
	t.Setenv("CLI_DEVIN_BIN", "devin-not-installed")
	previous := devinCLISpawn
	devinCLISpawn = func(devinCLISpec) (devinCLIProcess, error) {
		return nil, fmt.Errorf("exec: %q: %w", "devin-not-installed", fs.ErrNotExist)
	}
	t.Cleanup(func() { devinCLISpawn = previous })

	rec := httptest.NewRecorder()
	req := &Request{
		Ctx: context.Background(), Config: &providers.ProviderConfig{BaseURL: "devin://acp/stdio", NoAuth: true},
		Body: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), IsStream: true, ModelName: "swe-1.6-fast",
	}
	err := ForwardDevinCLI(rec, req)
	var ue *proxy.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("error = %v, want the lane's typed upstream error", err)
	}
	if ue.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", ue.StatusCode)
	}
	body := string(ue.Body)
	for _, want := range []string{"Devin CLI not found: devin-not-installed", "spawn_failed", "CLI_DEVIN_BIN"} {
		if !strings.Contains(body, want) {
			t.Errorf("error body lacks %q: %s", want, body)
		}
	}
	if strings.Contains(body, "no active connections") {
		t.Errorf("error body is the generic no-connection message: %s", body)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a failed spawn already wrote a body: %s", rec.Body.String())
	}
}

// TestForwardDevinCLI_ChildExitMidSession pins the exit path: the child died
// before answering, so its exit status and the tail of what it said on stderr
// are the failure the client sees.
func TestForwardDevinCLI_ChildExitMidSession(t *testing.T) {
	run := devinCLIHarness(t, context.Background(),
		`{"messages":[{"role":"user","content":"hi"}]}`, true,
		func(peer *devinFakePeer, msg devinCLIRPCMessage) {
			// initialize is answered, then the child dies without a session.
			if msg.Method == "initialize" {
				peer.exit(3, "devin: not authenticated — run `devin auth login`")
			}
		})
	var ue *proxy.UpstreamError
	if !errors.As(run.err, &ue) {
		t.Fatalf("error = %v, want the lane's typed upstream error", run.err)
	}
	body := string(ue.Body)
	for _, want := range []string{"Devin CLI exited with code 3", "devin auth login", "devin_cli_error"} {
		if !strings.Contains(body, want) {
			t.Errorf("error body lacks %q: %s", want, body)
		}
	}
	if run.rec.Body.Len() != 0 {
		t.Errorf("a turn that never started wrote frames: %s", run.rec.Body.String())
	}
}

// TestForwardDevinCLI_MalformedFrame pins the protocol error, and that a line
// which is not JSON at all is still tolerated: the CLI writes banners there.
func TestForwardDevinCLI_MalformedFrame(t *testing.T) {
	run := devinCLIHarness(t, context.Background(),
		`{"messages":[{"role":"user","content":"hi"}]}`, true,
		func(peer *devinFakePeer, msg devinCLIRPCMessage) {
			switch msg.Method {
			case "initialize":
				peer.sendRaw("devin acp 3000.2.1 — warming up")
				peer.sendRaw(`[1,2,3]`)
			}
		})
	var ue *proxy.UpstreamError
	if !errors.As(run.err, &ue) {
		t.Fatalf("error = %v, want the lane's typed upstream error", run.err)
	}
	if !strings.Contains(string(ue.Body), "malformed frame") {
		t.Errorf("error body = %s, want it to name the malformed frame", ue.Body)
	}
}

// TestForwardDevinCLI_BannerLinesAreIgnored is the other half of the frame
// rule: a non-JSON line is not a protocol error, so the turn still completes.
func TestForwardDevinCLI_BannerLinesAreIgnored(t *testing.T) {
	run := devinCLIHarness(t, context.Background(),
		`{"messages":[{"role":"user","content":"hi"}]}`, true,
		func(peer *devinFakePeer, msg devinCLIRPCMessage) {
			if msg.Method == "initialize" {
				// Banner text and a torn write belong on the same stream as the
				// frames; neither is a protocol error.
				peer.sendRaw("devin acp 3000.2.1")
				peer.sendRaw(`{"partial":`)
			}
			devinFakeAnswer(peer, msg)
		})
	if run.err != nil {
		t.Fatalf("ForwardDevinCLI() error = %v", run.err)
	}
	if got := sseDeltaText(t, run.rec.Body.String()); got != "hello world" {
		t.Errorf("streamed text = %q, want the turn to complete after the banner", got)
	}
}

// TestForwardDevinCLI_MidStreamErrorKeepsTheLaneMessage pins the in-band failure:
// once frames are out the status can no longer change, so the CLI's own sentence
// travels as the error frame upstream emits, followed by [DONE] — not as a
// generic "connection lost" that names nothing.
func TestForwardDevinCLI_MidStreamErrorKeepsTheLaneMessage(t *testing.T) {
	run := devinCLIHarness(t, context.Background(),
		`{"messages":[{"role":"user","content":"hi"}]}`, true,
		func(peer *devinFakePeer, msg devinCLIRPCMessage) {
			switch msg.Method {
			case "initialize":
				peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID), "result": map[string]any{}})
			case "session/new":
				peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID), "result": map[string]any{"sessionId": "s1"}})
			case "session/prompt":
				peer.send(map[string]any{"jsonrpc": "2.0", "method": "session/update",
					"params": map[string]any{"sessionId": "s1", "update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": "partial"},
					}}})
				peer.send(map[string]any{"jsonrpc": "2.0", "method": "_cognition.ai/agent_stopped",
					"params": map[string]any{"cause": "error", "errorMessage": "tool runtime crashed"}})
			}
		})
	if run.err == nil {
		t.Fatal("the turn reported success after the agent errored")
	}
	body := run.rec.Body.String()
	if !strings.Contains(body, "tool runtime crashed") {
		t.Fatalf("the error frame does not carry what the agent said:\n%s", body)
	}
	if !strings.Contains(body, `"type":"devin_cli_error"`) {
		t.Errorf("the error frame is not the lane's own type:\n%s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("the stream did not end with [DONE]:\n%s", body)
	}
	if !strings.Contains(body, `"content":"partial"`) {
		t.Errorf("the frames written before the failure were lost:\n%s", body)
	}
}

// TestForwardDevinCLI_ContextCancelKillsChild pins cancellation: the child is
// killed, it is reaped, and the read that was blocked on its stdout returns. The
// assertions are channels, not sleeps — the peer's own goroutine proves it ran
// to the end.
func TestForwardDevinCLI_ContextCancelKillsChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	answered := make(chan struct{})
	var once sync.Once
	run := devinCLIStart(t, ctx, `{"messages":[{"role":"user","content":"hi"}]}`, true,
		func(peer *devinFakePeer, msg devinCLIRPCMessage) {
			switch msg.Method {
			case "initialize":
				peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID), "result": map[string]any{}})
			case "session/new":
				peer.send(map[string]any{"jsonrpc": "2.0", "id": rawID(msg.ID), "result": map[string]any{"sessionId": "s1"}})
			case "session/prompt":
				// The agent is thinking and never answers: the request is
				// blocked on this child's stdout.
				once.Do(func() { close(answered) })
			}
		})

	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("the fake peer never saw session/prompt")
	}
	cancel()

	// The request returns only once the child is gone and reaped.
	select {
	case <-run.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not return after cancellation")
	}
	if !errors.Is(run.err, context.Canceled) {
		t.Fatalf("error = %v, want the context cancellation", run.err)
	}
	if !run.peer.wasKilled() {
		t.Error("the child was not killed on cancellation")
	}
	if !run.peer.wasWaited() {
		t.Error("the child was not reaped: a request that returns without Wait leaves a zombie")
	}
	select {
	case <-run.peer.exited:
	default:
		t.Error("the child is still running after the request returned")
	}
}

// --- MCP helper ----------------------------------------------------------

// TestDevinClientToolsMCP_AnswersInitializeListAndCall pins the helper the CLI
// spawns: the three requests a real MCP client makes, and the seeded result the
// tools/call answer carries.
func TestDevinClientToolsMCP_AnswersInitializeListAndCall(t *testing.T) {
	tools := `[{"name":"mcp_get_weather","description":"Get weather","inputSchema":{"type":"object"}}]`
	results := `{"mcp_get_weather":"28C sunny"}`
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":"two","method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"mcp_get_weather"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"mcp_unknown"}}`,
		`not json`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	}, "\n")

	var out strings.Builder
	if err := serveDevinClientToolsMCP(strings.NewReader(input), &out, tools, results); err != nil {
		t.Fatalf("serveDevinClientToolsMCP() error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("answered %d requests, want 4: %v", len(lines), lines)
	}
	var initialize struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initialize); err != nil {
		t.Fatalf("initialize answer is not JSON: %v", err)
	}
	if initialize.Result.ProtocolVersion != devinMCPProtocolVersion || initialize.Result.ServerInfo.Name != DevinClientToolsMCPName {
		t.Errorf("initialize answer = %+v, want the upstream bridge's own announcement", initialize.Result)
	}
	if !strings.Contains(lines[1], `"mcp_get_weather"`) || !strings.Contains(lines[1], `"inputSchema"`) {
		t.Errorf("tools/list answer = %s, want the declared tools", lines[1])
	}
	if !strings.Contains(lines[2], "28C sunny") {
		t.Errorf("seeded call answer = %s, want the recorded tool result", lines[2])
	}
	if !strings.Contains(lines[3], "awaiting client tool_result") {
		t.Errorf("unseeded call answer = %s, want the upstream placeholder", lines[3])
	}
}

// TestServeDevinClientToolsMCP_IgnoresOtherArgv pins the dispatch: the helper
// runs only for its own mode, so an ordinary server start is untouched.
func TestServeDevinClientToolsMCP_IgnoresOtherArgv(t *testing.T) {
	for _, argv := range [][]string{nil, {}, {"start"}, {"--rtk=false"}} {
		if ServeDevinClientToolsMCP(argv) {
			t.Errorf("ServeDevinClientToolsMCP(%v) claimed the process", argv)
		}
	}
}

// --- helpers -------------------------------------------------------------

// devinCLIFrame returns the single request with the given method.
func devinCLIFrame(t *testing.T, peer *devinFakePeer, method string) devinCLIRPCMessage {
	t.Helper()
	var found []devinCLIRPCMessage
	for _, msg := range peer.writtenFrames() {
		if msg.Method == method {
			found = append(found, msg)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d frames with method %q, want exactly 1", len(found), method)
	}
	return found[0]
}

func decodeParams(t *testing.T, msg devinCLIRPCMessage, into any) {
	t.Helper()
	if err := json.Unmarshal(msg.Params, into); err != nil {
		t.Fatalf("params of %s are not decodable: %v", msg.Method, err)
	}
}
