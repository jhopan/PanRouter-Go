package executor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// devinCLIProcess is one `devin acp` child, seen through the three pipes the
// ACP transport needs and the two lifecycle calls the request owns.
//
// The interface exists so the spawner can be replaced in tests: the lane's
// transport is stdio to a binary that is not installed on every machine, and a
// test that needs a real `devin` is a test that never runs (upstream injects
// node:child_process the same way).
type devinCLIProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	// Wait reaps the child. It must be called exactly once; the request owns
	// the child, so a request that never waits leaves a zombie behind.
	Wait() error
	Kill() error
}

// devinCLISpec is one spawn: the binary, its argv, the workspace and the
// environment the CLI inherits.
type devinCLISpec struct {
	Bin  string
	Args []string
	Dir  string
	Env  []string
}

// devinCLISpawner starts a child. Errors are returned as the spawn itself
// reported them (a missing binary arrives here as *exec.Error wrapping
// fs.ErrNotExist), so the caller can tell "not installed" from "would not run".
type devinCLISpawner func(spec devinCLISpec) (devinCLIProcess, error)

// devinCLISpawn is the process factory. It is a package variable so a test can
// substitute a fake ACP peer; production always uses spawnDevinCLIProcess.
var devinCLISpawn devinCLISpawner = spawnDevinCLIProcess

// devinCLIProcessGrace is how long the child is given to exit on its own after
// its stdin is closed, before it is killed. Upstream arms the same 2s timer
// (devin-cli.js:496) after the last frame.
const devinCLIProcessGrace = 2 * time.Second

// devinCLIStderrLimit bounds the stderr capture. The child narrates its whole
// session on stderr, and the tail is what explains a failed turn; an unbounded
// buffer would pin the memory of a long-running agent for a message at most a
// few hundred bytes of which is ever read.
const devinCLIStderrLimit = 8 << 10

// devinCLIStderr is a bounded tail buffer for the child's stderr.
type devinCLIStderr struct {
	mu   sync.Mutex
	buf  []byte
	done chan struct{}
}

func newDevinCLIStderr() *devinCLIStderr {
	return &devinCLIStderr{done: make(chan struct{})}
}

// drain copies the child's stderr into the bounded tail until EOF. It is the
// terminator for the read goroutine: the stream ends when the child's fd
// closes, and the request kills the child, so this always returns.
func (s *devinCLIStderr) drain(r io.Reader) {
	defer close(s.done)
	chunk := make([]byte, 4<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			s.mu.Lock()
			s.buf = append(s.buf, chunk[:n]...)
			if len(s.buf) > devinCLIStderrLimit {
				s.buf = s.buf[len(s.buf)-devinCLIStderrLimit:]
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// tail returns the captured stderr, trimmed for a one-line error.
func (s *devinCLIStderr) tail() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(string(s.buf))
}

// wait blocks until the drain goroutine has finished.
//
// The wait is bounded: a grandchild that inherited the child's stderr keeps the
// pipe open after the child is gone, and a request must not be pinned to a file
// descriptor it does not control. Whatever arrived by then is the tail the
// failure message carries.
func (s *devinCLIStderr) wait() {
	if s == nil {
		return
	}
	select {
	case <-s.done:
	case <-time.After(time.Second):
	}
}

// devinCLIBinary resolves the `devin` executable the way upstream does
// (devin-cli.js:23-62):
//
//  1. CLI_DEVIN_BIN, verbatim — the operator's explicit override.
//  2. the installer's own locations, probed explicitly because a server
//     started detached (tray, daemon, launchd) inherits a PATH that has
//     neither ~/.local/bin nor Homebrew's prefix on it.
//  3. the PATH lookup, when nothing was found on disk.
//
// When none of them exists the bare name is returned: the spawn then fails with
// ENOENT naming the binary, which is the error the lane reports.
func devinCLIBinary() string {
	if envBin := strings.TrimSpace(os.Getenv("CLI_DEVIN_BIN")); envBin != "" {
		return envBin
	}
	for _, candidate := range devinCLIBinaryCandidates() {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	name := "devin"
	if runtime.GOOS == "windows" {
		name = "devin.exe"
	}
	if resolved, err := exec.LookPath(name); err == nil {
		return resolved
	}
	return name
}

// devinCLIBinaryCandidates lists the installer and package-manager locations
// upstream probes, in upstream's order. The Windows list includes the official
// installer's %LOCALAPPDATA%\devin\cli\bin\devin.exe arm, which a Scoop or
// Programs install does not cover.
func devinCLIBinaryCandidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" && home != "" {
		localAppData = filepath.Join(home, "AppData", "Local")
	}
	if runtime.GOOS == "windows" {
		return []string{
			filepath.Join(localAppData, "devin", "cli", "bin", "devin.exe"),
			filepath.Join(home, ".local", "bin", "devin.exe"),
			filepath.Join(home, "scoop", "shims", "devin.exe"),
			filepath.Join(localAppData, "Programs", "devin", "devin.exe"),
		}
	}
	return []string{
		filepath.Join(home, ".local", "share", "devin", "bin", "devin"),
		filepath.Join(home, ".devin", "bin", "devin"),
		filepath.Join(home, ".local", "bin", "devin"),
		"/opt/homebrew/bin/devin",
		"/usr/local/bin/devin",
		"/usr/bin/devin",
	}
}

// devinCLIExecProcess is the production devinCLIProcess over an *exec.Cmd.
type devinCLIExecProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
}

func (p *devinCLIExecProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *devinCLIExecProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *devinCLIExecProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *devinCLIExecProcess) Wait() error           { return p.cmd.Wait() }

// Kill terminates the child. It is the cancellation path: the ACP session is
// abandoned, so there is nothing to negotiate with a graceful signal for.
func (p *devinCLIExecProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

// spawnDevinCLIProcess starts the child with its own pipes.
//
// The pipes are created here rather than through exec's StdoutPipe/StderrPipe so
// the read ends outlive Wait: those helpers close the parent's end inside Wait,
// which would cut off the stderr tail the failure message is built from.
func spawnDevinCLIProcess(spec devinCLISpec) (devinCLIProcess, error) {
	cmd := exec.Command(spec.Bin, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("devin-cli: stdin pipe: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, fmt.Errorf("devin-cli: stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("devin-cli: stderr pipe: %w", err)
	}

	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW} {
			f.Close()
		}
		return nil, err
	}
	// The child holds its own copies now; the parent's ends of the write side
	// and of the child's stdin are closed so EOF reaches the reader.
	stdinR.Close()
	stdoutW.Close()
	stderrW.Close()

	return &devinCLIExecProcess{cmd: cmd, stdin: stdinW, stdout: stdoutR, stderr: stderrR}, nil
}

// devinCLIExitCode reads the exit status out of a Wait error.
func devinCLIExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	var own *devinCLIProcessExit
	if errors.As(err, &own) {
		return own.code
	}
	return -1
}

// devinCLIProcessExit is an exit status reported without an *exec.ExitError —
// the shape both a spawned child and a substituted one can produce, so the
// failure message reads the same either way.
type devinCLIProcessExit struct {
	code int
	err  error
}

func (e *devinCLIProcessExit) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("Devin CLI exited with code %d", e.code)
}

// ExitCode reports the status the child ended with.
func (e *devinCLIProcessExit) ExitCode() int { return e.code }
