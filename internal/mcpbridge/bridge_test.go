package mcpbridge

import (
	"bufio"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// helperEnv marks the re-executed test binary as the stub MCP child.
const helperEnv = "PANROUTER_MCPBRIDGE_STUB"

// TestStubProcess is not a test: it is the child the bridge talks to. The test
// binary re-executes itself with helperEnv set, then echoes every stdin line
// back as a JSON-RPC result so both directions of the bridge are observable
// without npx, without a network and without sleeps.
func TestStubProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("stub child for the bridge tests")
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		echo, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "echo: " + scanner.Text()}},
			},
		})
		if err != nil {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "%s\n", echo)
	}
	os.Exit(0)
}

// stubBridge builds a bridge whose children are the stub above.
func stubBridge(t *testing.T) *Bridge {
	t.Helper()
	b := NewWithSpawn(func(Plugin) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=TestStubProcess")
		cmd.Env = append(os.Environ(), helperEnv+"=1")
		return cmd, nil
	})
	b.logf = func(string, ...any) {}
	t.Cleanup(b.KillAll)
	return b
}

func TestRegisterSessionRefusesUnknownPlugin(t *testing.T) {
	b := stubBridge(t)

	if _, err := b.RegisterSession("nope"); !errors.Is(err, ErrUnknownPlugin) {
		t.Fatalf("RegisterSession(nope) error = %v, want ErrUnknownPlugin", err)
	}
}

func TestRegisterSessionReturnsUUID(t *testing.T) {
	b := stubBridge(t)

	session, err := b.RegisterSession("browsermcp")
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	defer b.UnregisterSession("browsermcp", session.ID())

	if len(session.ID()) != 36 {
		t.Errorf("session id = %q, want a 36-character UUID", session.ID())
	}
}

func TestSendReachesChildAndFramesComeBack(t *testing.T) {
	b := stubBridge(t)
	session, err := b.RegisterSession("browsermcp")
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	defer b.UnregisterSession("browsermcp", session.ID())

	message := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if err := b.Send("browsermcp", []byte(message)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case frame := <-session.Frames():
		if !strings.HasPrefix(frame, "event: message\ndata: ") {
			t.Fatalf("frame = %q, want an event: message frame", frame)
		}
		if !strings.HasSuffix(frame, "\n\n") {
			t.Errorf("frame does not end with a blank line: %q", frame)
		}
		if !strings.Contains(frame, "echo: ") || !strings.Contains(frame, "tools/list") {
			t.Errorf("frame does not carry the child's answer to the sent line: %q", frame)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no frame arrived from the child")
	}
}

func TestSendWithoutChildReportsNotRunning(t *testing.T) {
	b := stubBridge(t)

	if err := b.Send("browsermcp", []byte(`{"jsonrpc":"2.0","id":1}`)); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Send error = %v, want ErrNotRunning", err)
	}
}

func TestUnregisterLastSessionStopsTheChild(t *testing.T) {
	b := stubBridge(t)
	session, err := b.RegisterSession("browsermcp")
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	b.UnregisterSession("browsermcp", session.ID())

	if err := b.Send("browsermcp", []byte(`{"jsonrpc":"2.0","id":1}`)); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("after the last session left, Send error = %v, want ErrNotRunning", err)
	}
}

func TestSecondSessionSharesTheChild(t *testing.T) {
	b := stubBridge(t)
	first, err := b.RegisterSession("browsermcp")
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	second, err := b.RegisterSession("browsermcp")
	if err != nil {
		t.Fatalf("second RegisterSession: %v", err)
	}
	defer b.UnregisterSession("browsermcp", first.ID())
	defer b.UnregisterSession("browsermcp", second.ID())

	if first.ID() == second.ID() {
		t.Fatal("two sessions were handed the same id")
	}
	if err := b.Send("browsermcp", []byte(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Both subscribers see the same broadcast.
	for name, session := range map[string]*Session{"first": first, "second": second} {
		select {
		case frame := <-session.Frames():
			if !strings.Contains(frame, "echo: ") || !strings.Contains(frame, "ping") {
				t.Errorf("%s session frame = %q, want the ping answered", name, frame)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("%s session received no frame", name)
		}
	}
}

func TestUnregisterOneOfTwoSessionsKeepsChildAlive(t *testing.T) {
	b := stubBridge(t)
	first, err := b.RegisterSession("browsermcp")
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	second, err := b.RegisterSession("browsermcp")
	if err != nil {
		t.Fatalf("second RegisterSession: %v", err)
	}
	defer b.UnregisterSession("browsermcp", second.ID())

	b.UnregisterSession("browsermcp", first.ID())
	if err := b.Send("browsermcp", []byte(`{"jsonrpc":"2.0","id":2}`)); err != nil {
		t.Fatalf("Send after one of two sessions left: %v", err)
	}
}

func TestKillAllStopsChildren(t *testing.T) {
	b := stubBridge(t)
	if _, err := b.RegisterSession("browsermcp"); err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	b.KillAll()

	if err := b.Send("browsermcp", []byte(`{"jsonrpc":"2.0","id":1}`)); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("after KillAll, Send error = %v, want ErrNotRunning", err)
	}
}

func TestFindPluginOnlyAcceptsPresets(t *testing.T) {
	if _, ok := FindPlugin("browsermcp"); !ok {
		t.Fatal("browsermcp is not a preset")
	}
	// A user-supplied command line must never be spawnable by name.
	for _, name := range []string{"", "browsermcp/../../bin/sh", "sh", "npx"} {
		if _, ok := FindPlugin(name); ok {
			t.Errorf("FindPlugin(%q) accepted a non-preset", name)
		}
	}
}
