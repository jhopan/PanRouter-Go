package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	json "encoding/json/v2"

	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/proxy"
)

// ForwardDevinCLI serves the devin-cli lane: the local Devin CLI in ACP mode
// (Agent Client Protocol, JSON-RPC 2.0 over the child's stdio).
//
// Nothing about this lane is HTTP. One request spawns one `devin acp` child,
// walks initialize → session/new → session/prompt (devin-cli.js:515-600),
// folds the session/update notifications it streams back into OpenAI chunks,
// and kills the child when the turn ends or the caller goes away. Credentials
// are the CLI's own (`devin auth login`), so the provider is noAuth and no
// header is built here.
//
// The child runs in the client's workspace and can run shell commands and
// modify the filesystem there — that is the feature, and it is also why the
// provider ships hidden.
func ForwardDevinCLI(w http.ResponseWriter, req *Request) error {
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if req.Config == nil {
		return errors.New("devin-cli: no provider config")
	}

	var body devinCLIRequest
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return &proxy.UpstreamError{StatusCode: http.StatusBadRequest,
			Body: (&devinCLIError{message: "devin-cli: invalid request body: " + err.Error()}).body()}
	}
	messages := body.Messages
	if len(messages) == 0 {
		messages = devinCLIInputMessages(body.Input)
	}
	model := req.ModelName
	if model == "" {
		model = body.Model
	}
	cwd := devinCLIWorkspaceCwd(&body, messages)
	prompt := devinCLIPromptText(messages)
	emitter := newDevinCLIEmitter(req, w, model, prompt)

	servers := devinCLIMCPServers(body.Tools, devinCLIToolResults(messages))
	configDir := ""
	if len(servers) > 0 {
		dir, err := devinCLIMCPConfigDir(servers)
		if err != nil {
			// A missing config only costs the agent its MCP servers; it does
			// not stop the turn, exactly as upstream treats the write failure.
			log.Warn("devin-cli", "MCP config write failed", "error", err)
		} else {
			configDir = dir
			defer os.RemoveAll(dir)
		}
	}

	log.Info("devin-cli", "acp session",
		"model", model, "cwd", cwd, "client_tools", len(body.Tools), "mcp_servers", len(servers))

	err := devinCLIRun(devinCLIRunParams{
		ctx:         ctx,
		emitter:     emitter,
		spec:        devinCLIProcessSpec(cwd, configDir),
		prompt:      prompt,
		model:       model,
		cwd:         cwd,
		clientTools: servers[DevinClientToolsMCPName] != nil,
	})
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// The caller hung up. Reporting the child's death instead would blame
		// the upstream for a disconnect the gateway did not cause.
		return ctxErr
	}
	return emitter.abort(devinCLIAsUpstreamError(err))
}

// devinCLIRunParams bundles one session's inputs.
type devinCLIRunParams struct {
	ctx         context.Context
	emitter     *devinCLIEmitter
	spec        devinCLISpec
	prompt      string
	model       string
	cwd         string
	clientTools bool
}

// errDevinCLIStreamClosed marks the child closing its stdout, which is how a
// turn ends when the CLI exits instead of reporting a stop itself.
var errDevinCLIStreamClosed = errors.New("devin-cli: child stream closed")

// devinCLIRun owns the child for exactly one request: it spawns, drives the
// session, and leaves nothing running behind on any path.
func devinCLIRun(params devinCLIRunParams) error {
	proc, err := devinCLISpawn(params.spec)
	if err != nil {
		return devinCLISpawnFailure(params.spec.Bin, err)
	}
	stderr := newDevinCLIStderr()
	go stderr.drain(proc.Stderr())

	// The child outlives the goroutine that reads it, so cancellation is what
	// kills it. This watcher terminates on either signal — it is the whole
	// reason the read below can block without leaking a process.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-params.ctx.Done():
			_ = proc.Kill()
		case <-watchDone:
		}
	}()

	session := newDevinCLISession(proc, params.emitter, params.model, params.cwd, params.clientTools)
	session.prompt = params.prompt

	runErr := session.start()
	if runErr == nil {
		runErr = session.drain(proc.Stdout(), func() error { return errDevinCLIStreamClosed })
	}
	exitErr := devinCLIReap(proc, stderr)

	switch {
	case runErr == nil || session.done():
		// The turn reached its own terminal frame; the child ending afterwards
		// is the expected outcome and has nothing to add.
		return nil
	case errors.Is(runErr, errDevinCLIStreamClosed), devinCLIBrokenWriter(runErr):
		// The child is gone — it closed its stdout or its stdin refused the
		// write. Upstream ignores a failed write and reports the exit status
		// from the close handler, which is the same answer.
		if exitErr != nil {
			if params.emitter.answered() {
				// The answer already arrived, so only the exit is left to
				// report, and a turn that answered is not a failure.
				return session.finish("stop")
			}
			return devinCLIChildFailure(exitErr, stderr)
		}
		return session.finish("stop")
	default:
		return runErr
	}
}

// devinCLIBrokenWriter reports a write the dead child's stdin refused.
func devinCLIBrokenWriter(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) || errors.Is(err, fs.ErrClosed) ||
		errors.Is(err, syscall.EPIPE)
}

// devinCLIReap closes the child's stdin, gives it a grace period to exit, kills
// it if it did not, and reaps it. Every path ends with a waited-for process:
// the request owns the child, so a request that returns without waiting leaves a
// zombie behind.
func devinCLIReap(proc devinCLIProcess, stderr *devinCLIStderr) error {
	_ = proc.Stdin().Close()

	waited := make(chan error, 1)
	go func() { waited <- proc.Wait() }()

	var err error
	select {
	case err = <-waited:
	case <-time.After(devinCLIProcessGrace):
		_ = proc.Kill()
		err = <-waited
	}
	stderr.wait()
	return err
}

// devinCLIProcessSpec builds the argv and environment for one child.
func devinCLIProcessSpec(cwd, configDir string) devinCLISpec {
	// The default agent carries the CLI's own tools (fs, shell, search), which
	// is what makes it useful at all; `--agent-type summarizer` is the
	// tool-less alternative, chosen by the operator.
	args := []string{"acp"}
	if agentType := strings.TrimSpace(os.Getenv("CLI_DEVIN_AGENT_TYPE")); agentType != "" {
		args = append(args, "--agent-type", agentType)
	}
	env := os.Environ()
	// The child inherits the parent's environment so the CLI finds the
	// credentials `devin auth login` stored. Nothing else is injected: a bogus
	// provider key overrides those credentials and makes the CLI answer
	// -32000 "invalid api key".
	if os.Getenv("DEVIN_PERMISSION_MODE") == "" {
		env = append(env, "DEVIN_PERMISSION_MODE="+devinCLIPermissionModeBypass)
	}
	if configDir != "" {
		env = append(env, "XDG_CONFIG_HOME="+configDir)
	}
	return devinCLISpec{Bin: devinCLIBinary(), Args: args, Dir: cwd, Env: env}
}

// devinCLIError is the lane's own failure: a status, a message and the code the
// error payload carries.
type devinCLIError struct {
	status  int
	code    string
	message string
}

func (e *devinCLIError) Error() string { return e.message }

// body renders the error payload the client sees.
func (e *devinCLIError) body() []byte {
	detail := map[string]any{"message": e.message, "type": "devin_cli_error"}
	if e.code != "" {
		detail["code"] = e.code
	}
	payload, err := json.Marshal(map[string]any{"error": detail})
	if err != nil {
		return []byte(`{"error":{"message":"devin-cli: internal error","type":"devin_cli_error"}}`)
	}
	return payload
}

// devinCLIAsUpstreamError converts the lane's failure into the payload the
// handler writes when the stream was never opened.
func devinCLIAsUpstreamError(err error) error {
	var lane *devinCLIError
	if !errors.As(err, &lane) {
		return err
	}
	status := lane.status
	if status == 0 {
		status = http.StatusBadGateway
	}
	return &proxy.UpstreamError{StatusCode: status, Body: lane.body()}
}

// devinCLISpawnFailure names the binary when the child could not be started.
//
// A missing CLI is the expected failure of this lane: the binary is installed
// by the user, not shipped, so the message has to say which binary was looked
// for and how to get it — a bare 502 leaves an operator with nothing to act on
// (upstream's spawn-error frame carries the same sentence).
func devinCLISpawnFailure(bin string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return &devinCLIError{status: http.StatusBadGateway, code: "spawn_failed",
			message: "Devin CLI not found: " + bin + ". Install via https://cli.devin.ai or set CLI_DEVIN_BIN env var."}
	}
	return &devinCLIError{status: http.StatusBadGateway, code: "spawn_failed",
		message: "Devin CLI spawn error: " + err.Error()}
}

// devinCLIChildFailure reports a child that died before answering, with the tail
// of what it said on stderr.
func devinCLIChildFailure(exitErr error, stderr *devinCLIStderr) error {
	code := devinCLIExitCode(exitErr)
	message := fmt.Sprintf("Devin CLI exited with code %d", code)
	if code < 0 {
		message = "Devin CLI was terminated: " + exitErr.Error()
	}
	if tail := devinCLIOneLine(stderr.tail()); tail != "" {
		message += ": " + tail
	}
	return &devinCLIError{status: http.StatusBadGateway, message: message}
}

// devinCLIOneLine collapses a stderr tail into one bounded line for an error.
func devinCLIOneLine(text string) string {
	const limit = 400
	collapsed := strings.Join(strings.Fields(text), " ")
	if len(collapsed) <= limit {
		return collapsed
	}
	return collapsed[:limit] + "..."
}
