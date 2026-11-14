package mcpbridge

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"github.com/google/uuid"
)

var (
	// ErrUnknownPlugin is returned when the requested plugin is not one of the
	// presets. The caller answers upstream's "Unknown plugin: <name>" 404.
	ErrUnknownPlugin = errors.New("mcpbridge: unknown local plugin")
	// ErrNotRunning is returned by Send when no child is alive for the plugin.
	// It covers both "never spawned" and "exited since": upstream throws the
	// same error for both, because it also reads the store rather than the
	// process.
	ErrNotRunning = errors.New("mcpbridge: bridge not running")
)

const (
	// sessionQueueFrames bounds one subscriber's backlog. Upstream enqueues into
	// a ReadableStream with no bound; a Go channel needs one, and blocking the
	// child's stdout reader on a stalled subscriber would stall every other
	// subscriber of the same plugin. A subscriber that falls this far behind is
	// not draining its SSE socket, so its newest frames are dropped instead —
	// upstream's own client would be reading the same truncated stream.
	sessionQueueFrames = 256
	// maxFrameBytes is the longest child line the reader accepts, matching the
	// scale of an MCP frame: a browser snapshot with inline images. A longer
	// line ends the reader for that child rather than growing without bound.
	maxFrameBytes = 32 << 20
)

// Session is one SSE subscriber. Frames arrive already formatted
// ("event: message\ndata: …\n\n"), exactly as they go on the wire.
type Session struct {
	id     string
	frames chan string
}

// ID is the session id the SSE handshake puts in the message endpoint URL.
func (s *Session) ID() string { return s.id }

// Frames is the stream of frames for this subscriber. It is never closed: a
// child that dies leaves its subscribers connected with no further frames,
// which is what upstream does when the child's entry is dropped from the store.
func (s *Session) Frames() <-chan string { return s.frames }

type entry struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	// sessions is guarded by Bridge.mu, like the entry itself.
	sessions map[string]*Session
	// exited is set by the wait goroutine under Bridge.mu, so ensureLocked can
	// tell a live child from a corpse without racing cmd.Wait.
	exited bool
}

// Bridge owns the child processes and their subscribers.
type Bridge struct {
	mu      sync.Mutex
	entries map[string]*entry
	spawn   func(Plugin) (*exec.Cmd, error)
	logf    func(format string, args ...any)
}

// New returns an empty bridge that spawns the real presets.
func New() *Bridge {
	return &Bridge{
		entries: make(map[string]*entry),
		spawn:   spawnCommand,
		logf:    log.Printf,
	}
}

// NewWithSpawn returns a bridge that starts a child through spawn instead of
// the preset command line, so a test can point a plugin at a stub process.
// Production uses New.
func NewWithSpawn(spawn func(Plugin) (*exec.Cmd, error)) *Bridge {
	b := New()
	b.spawn = spawn
	return b
}

var defaultBridge = New()

// Default is the process-wide bridge. One child per plugin exists for the whole
// process, so two SSE subscribers share it, exactly like upstream's globalThis
// store.
func Default() *Bridge { return defaultBridge }

// RegisterSession subscribes to a plugin, spawning its child if none is alive,
// and returns the session whose ID goes into the SSE handshake.
//
// The plugin is validated before anything is spawned: an unknown name is the
// caller's 404, never a command.
func (b *Bridge) RegisterSession(name string) (*Session, error) {
	if _, ok := FindPlugin(name); !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownPlugin, name)
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	e, err := b.ensureLocked(name)
	if err != nil {
		return nil, err
	}
	s := &Session{id: uuid.NewString(), frames: make(chan string, sessionQueueFrames)}
	e.sessions[s.id] = s
	return s, nil
}

// UnregisterSession drops a subscriber and kills the child when it was the last
// one, so an idle plugin does not keep an npx process alive.
func (b *Bridge) UnregisterSession(name, sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.entries[name]
	if e == nil {
		return
	}
	delete(e.sessions, sessionID)
	if len(e.sessions) > 0 {
		return
	}
	delete(b.entries, name)
	kill(e)
}

// Send writes one JSON-RPC message to the plugin's stdin. The message is used
// verbatim (upstream writes JSON.stringify of the parsed body; a body that was
// already JSON is the same bytes once re-serialized, and echoing the client's
// bytes avoids reformatting numbers the child would then see differently).
func (b *Bridge) Send(name string, message []byte) error {
	b.mu.Lock()
	e := b.entries[name]
	b.mu.Unlock()
	if e == nil {
		return fmt.Errorf("%w: %s", ErrNotRunning, name)
	}

	// One write per message, and one writer at a time: two concurrent POSTs must
	// not interleave halves of two JSON-RPC lines into one line the child would
	// fail to parse.
	line := make([]byte, 0, len(message)+1)
	line = append(line, message...)
	line = append(line, '\n')

	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	if _, err := e.stdin.Write(line); err != nil {
		return fmt.Errorf("%w: %s", ErrNotRunning, name)
	}
	return nil
}

// KillAll stops every child. Called on shutdown so an npx child does not
// outlive the gateway (upstream killAllBridges does the same).
func (b *Bridge) KillAll() {
	b.mu.Lock()
	entries := make([]*entry, 0, len(b.entries))
	for name, e := range b.entries {
		entries = append(entries, e)
		delete(b.entries, name)
	}
	b.mu.Unlock()

	for _, e := range entries {
		kill(e)
	}
}

// ensureLocked returns the live entry for name, spawning it when there is none.
// Callers hold b.mu.
func (b *Bridge) ensureLocked(name string) (*entry, error) {
	if e := b.entries[name]; e != nil && !e.exited {
		return e, nil
	}
	plugin, ok := FindPlugin(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownPlugin, name)
	}

	cmd, err := b.spawn(plugin)
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp %s: stdin pipe: %w", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp %s: stdout pipe: %w", name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp %s: stderr pipe: %w", name, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp %s: start: %w", name, err)
	}

	e := &entry{cmd: cmd, stdin: stdin, sessions: make(map[string]*Session)}
	b.entries[name] = e
	go b.readFrames(name, e, stdout)
	go b.drainStderr(name, stderr)
	go b.waitExit(name, e)
	return e, nil
}

// readFrames turns the child's newline-delimited JSON-RPC into SSE frames and
// broadcasts each one. Blank lines are skipped, as upstream's reader does.
func (b *Bridge) readFrames(name string, e *entry, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)
	for scanner.Scan() {
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		frame := "event: message\ndata: " + filterFrame(raw) + "\n\n"
		b.broadcast(name, e, frame)
	}
	if err := scanner.Err(); err != nil {
		b.logf("[mcp:%s] read frames: %v", name, err)
	}
}

func (b *Bridge) broadcast(name string, e *entry, frame string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries[name] != e {
		return
	}
	for _, s := range e.sessions {
		select {
		case s.frames <- frame:
		default:
		}
	}
}

func (b *Bridge) drainStderr(name string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4<<10), 1<<20)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			b.logf("[mcp:%s] %s", name, line)
		}
	}
}

// waitExit drops the entry once the child is gone, so the next subscriber
// respawns. Subscribers stay connected (upstream drops the store entry and
// leaves their streams open).
func (b *Bridge) waitExit(name string, e *entry) {
	err := e.cmd.Wait()
	b.mu.Lock()
	e.exited = true
	if b.entries[name] == e {
		delete(b.entries, name)
	}
	b.mu.Unlock()
	if err != nil {
		b.logf("[mcp:%s] exited: %v", name, err)
	} else {
		b.logf("[mcp:%s] exited", name)
	}
}

func kill(e *entry) {
	if e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
	}
	_ = e.stdin.Close()
}

// spawnCommand resolves the preset's command and returns the process to start.
//
// The plugin's Command is resolved through PATH rather than trusted as a path,
// and on Windows the npx shim is a .cmd file: CreateProcess cannot execute one
// directly, so it is run through the interpreter it names.
func spawnCommand(p Plugin) (*exec.Cmd, error) {
	path, err := exec.LookPath(p.Command)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %s not found: %w", p.Name, p.Command, err)
	}
	if runtime.GOOS == "windows" && isBatchScript(path) {
		return exec.Command("cmd", append([]string{"/c", path}, p.Args...)...), nil
	}
	return exec.Command(path, p.Args...), nil
}

func isBatchScript(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat")
}
