package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"panrouter/proxy/internal/providers"
)

func TestResolveDefaultStallTimeout(t *testing.T) {
	// 1. Unset env -> DefaultStallTimeout (6m)
	os.Unsetenv("STREAM_STALL_TIMEOUT_MS")
	os.Unsetenv("PANROUTER_STREAM_STALL_TIMEOUT_MS")
	if d := ResolveDefaultStallTimeout(); d != DefaultStallTimeout {
		t.Errorf("ResolveDefaultStallTimeout() with unset env = %v, want %v", d, DefaultStallTimeout)
	}

	// 2. STREAM_STALL_TIMEOUT_MS = 15000 -> 15s
	t.Setenv("STREAM_STALL_TIMEOUT_MS", "15000")
	if d := ResolveDefaultStallTimeout(); d != 15*time.Second {
		t.Errorf("ResolveDefaultStallTimeout() = %v, want 15s", d)
	}

	// 3. PANROUTER_STREAM_STALL_TIMEOUT_MS = 20000 -> 20s
	os.Unsetenv("STREAM_STALL_TIMEOUT_MS")
	t.Setenv("PANROUTER_STREAM_STALL_TIMEOUT_MS", "20000")
	if d := ResolveDefaultStallTimeout(); d != 20*time.Second {
		t.Errorf("ResolveDefaultStallTimeout() = %v, want 20s", d)
	}

	// 4. Invalid string fallback -> DefaultStallTimeout
	t.Setenv("STREAM_STALL_TIMEOUT_MS", "invalid")
	t.Setenv("PANROUTER_STREAM_STALL_TIMEOUT_MS", "0")
	if d := ResolveDefaultStallTimeout(); d != DefaultStallTimeout {
		t.Errorf("ResolveDefaultStallTimeout() with invalid env = %v, want %v", d, DefaultStallTimeout)
	}
}

func TestResolveDefaultFirstChunkTimeout(t *testing.T) {
	os.Unsetenv("STREAM_FIRST_CHUNK_TIMEOUT_MS")
	os.Unsetenv("PANROUTER_STREAM_FIRST_CHUNK_TIMEOUT_MS")
	if d := ResolveDefaultFirstChunkTimeout(); d != DefaultFirstChunkTimeout {
		t.Errorf("unset env = %v, want %v", d, DefaultFirstChunkTimeout)
	}
	if DefaultFirstChunkTimeout >= DefaultStallTimeout {
		t.Errorf("the first-chunk budget (%v) must be its own value, not the inter-chunk one (%v)",
			DefaultFirstChunkTimeout, DefaultStallTimeout)
	}

	t.Setenv("STREAM_FIRST_CHUNK_TIMEOUT_MS", "1234")
	if d := ResolveDefaultFirstChunkTimeout(); d != 1234*time.Millisecond {
		t.Errorf("STREAM_FIRST_CHUNK_TIMEOUT_MS = %v, want 1234ms", d)
	}

	os.Unsetenv("STREAM_FIRST_CHUNK_TIMEOUT_MS")
	t.Setenv("PANROUTER_STREAM_FIRST_CHUNK_TIMEOUT_MS", "4321")
	if d := ResolveDefaultFirstChunkTimeout(); d != 4321*time.Millisecond {
		t.Errorf("legacy env name = %v, want 4321ms", d)
	}
}

// TestResolveStallBudgets pins the two-budget contract: a provider that declares
// only the inter-chunk timeout keeps the default for prefill, and one that
// declares both gets both. Collapsing them onto one value is what aborted
// healthy MiniMax turns.
func TestResolveStallBudgets(t *testing.T) {
	os.Unsetenv("STREAM_STALL_TIMEOUT_MS")
	os.Unsetenv("PANROUTER_STREAM_STALL_TIMEOUT_MS")
	os.Unsetenv("STREAM_FIRST_CHUNK_TIMEOUT_MS")
	os.Unsetenv("PANROUTER_STREAM_FIRST_CHUNK_TIMEOUT_MS")

	t.Run("nil config keeps both defaults", func(t *testing.T) {
		got := ResolveStallBudgets(nil)
		if got.InterChunk != DefaultStallTimeout || got.FirstChunk != DefaultFirstChunkTimeout {
			t.Errorf("budgets = %+v, want the defaults", got)
		}
	})

	t.Run("inter-chunk only", func(t *testing.T) {
		got := ResolveStallBudgets(&providers.ProviderConfig{StallTimeoutMs: 120000})
		if got.InterChunk != 2*time.Minute {
			t.Errorf("InterChunk = %v, want 2m", got.InterChunk)
		}
		if got.FirstChunk != DefaultFirstChunkTimeout {
			t.Errorf("FirstChunk = %v, want the default %v", got.FirstChunk, DefaultFirstChunkTimeout)
		}
	})

	t.Run("both declared", func(t *testing.T) {
		got := ResolveStallBudgets(&providers.ProviderConfig{
			StallTimeoutMs:      15 * 60 * 1000,
			FirstChunkTimeoutMs: 10 * 60 * 1000,
		})
		if got.InterChunk != 15*time.Minute || got.FirstChunk != 10*time.Minute {
			t.Errorf("budgets = %+v, want 15m/10m", got)
		}
	})

	t.Run("MiniMax Code declares the pair upstream declares", func(t *testing.T) {
		for _, id := range []string{"minimax-code", "minimax-code-global"} {
			cfg, ok := providers.KnownProviders[id]
			if !ok {
				t.Fatalf("%s missing from KnownProviders", id)
			}
			if cfg.StallTimeoutMs <= int(DefaultStallTimeout/time.Millisecond) {
				t.Errorf("%s StallTimeoutMs = %dms, want well above the %v default",
					id, cfg.StallTimeoutMs, DefaultStallTimeout)
			}
			if cfg.FirstChunkTimeoutMs != 10*60*1000 {
				t.Errorf("%s FirstChunkTimeoutMs = %d, want 600000", id, cfg.FirstChunkTimeoutMs)
			}
			// What the stream actually runs on: the resolved pair, not the raw
			// fields. Upstream sets 15m between chunks and 10m before the first
			// one on both MiniMax Code registries.
			got := ResolveStallBudgets(&cfg)
			if got.InterChunk != 15*time.Minute || got.FirstChunk != 10*time.Minute {
				t.Errorf("%s budgets = {%v, %v}, want 15m/10m", id, got.InterChunk, got.FirstChunk)
			}
		}
	})
}

// TestStallReaderUsesTheBudgetForItsPhase is the behavioural half: the
// first-chunk budget governs the wait before any byte, and the inter-chunk
// budget takes over the moment data arrives. The error names the phase and the
// budget, so a log can tell "slow prefill" from "stream is gone".
func TestStallReaderUsesTheBudgetForItsPhase(t *testing.T) {
	t.Run("no first chunk", func(t *testing.T) {
		pr, pw := io.Pipe()
		defer pw.Close()
		budgets := StallBudgets{FirstChunk: 30 * time.Millisecond, InterChunk: time.Hour}
		r := NewStallReaderWithContext(context.Background(), pr, budgets, "phase-first")
		defer r.Close()

		_, err := r.Read(make([]byte, 8))
		if !errors.Is(err, ErrStreamStall) {
			t.Fatalf("err = %v, want a stall", err)
		}
		var stallErr *StreamStallError
		if !errors.As(err, &stallErr) {
			t.Fatalf("err = %v, want *StreamStallError", err)
		}
		if stallErr.Phase != stallPhaseFirstChunk {
			t.Errorf("phase = %q, want %q", stallErr.Phase, stallPhaseFirstChunk)
		}
		if stallErr.Budget != budgets.FirstChunk {
			t.Errorf("budget = %v, want the first-chunk budget %v", stallErr.Budget, budgets.FirstChunk)
		}
		if stallErr.Code() != StreamStallCode {
			t.Errorf("code = %q, want %q", stallErr.Code(), StreamStallCode)
		}
		if msg := stallErr.Error(); !strings.Contains(msg, stallPhaseFirstChunk) || !strings.Contains(msg, "30ms") {
			t.Errorf("message %q must name the phase and the budget", msg)
		}
	})

	t.Run("stalled after the first chunk", func(t *testing.T) {
		pr, pw := io.Pipe()
		defer pw.Close()
		// A long first-chunk budget and a short inter-chunk one: only a state
		// that keeps its own phase can produce the second error.
		budgets := StallBudgets{FirstChunk: time.Hour, InterChunk: 30 * time.Millisecond}
		r := NewStallReaderWithContext(context.Background(), pr, budgets, "phase-inter")
		defer r.Close()

		go func() {
			_, _ = pw.Write([]byte("first"))
		}()
		buf := make([]byte, 8)
		n, err := r.Read(buf)
		if err != nil || n != len("first") {
			t.Fatalf("first read = (%d, %v), want the first chunk", n, err)
		}

		_, err = r.Read(buf)
		if !errors.Is(err, ErrStreamStall) {
			t.Fatalf("second read err = %v, want a stall", err)
		}
		var stallErr *StreamStallError
		if !errors.As(err, &stallErr) {
			t.Fatalf("err = %v, want *StreamStallError", err)
		}
		if stallErr.Phase != stallPhaseAfterFirstChunk {
			t.Errorf("phase = %q, want %q", stallErr.Phase, stallPhaseAfterFirstChunk)
		}
		if stallErr.Budget != budgets.InterChunk {
			t.Errorf("budget = %v, want the inter-chunk budget %v", stallErr.Budget, budgets.InterChunk)
		}
		if stallErr.Chunks != 1 {
			t.Errorf("chunks = %d, want the one chunk that arrived", stallErr.Chunks)
		}
	})
}

// TestStallTimeoutEnvEndToEnd pins that the environment still wins over the
// compiled defaults, and that what reaches the client is the watchdog's own
// message: the frame carries `code: STREAM_STALL`, names the phase that
// expired, and repeats the budget it ran on.
func TestStallTimeoutEnvEndToEnd(t *testing.T) {
	t.Setenv("STREAM_FIRST_CHUNK_TIMEOUT_MS", "600000") // prefill cannot be the trigger
	t.Setenv("STREAM_STALL_TIMEOUT_MS", "20")           // 20ms silence after the first chunk

	pr, pw := io.Pipe()
	defer pw.Close()

	r := NewStallReaderWithContext(context.Background(), pr, StallBudgets{}, "env-stall-e2e")
	defer r.Close()

	go func() {
		_, _ = pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	}()

	rec := &mockResponseWriter{}
	err := SSECopy(rec, r, rec, nil)
	if err == nil {
		t.Fatal("expected stalled stream to error")
	}
	if !errors.Is(err, ErrStreamStall) {
		t.Errorf("error = %v, want it to wrap ErrStreamStall", err)
	}

	frame, ok := errorFrame(t, rec.String())
	if !ok {
		t.Fatalf("client got no error frame: %q", rec.String())
	}
	if frame.Error.Code != StreamStallCode {
		t.Errorf("code = %q, want %q", frame.Error.Code, StreamStallCode)
	}
	if !strings.Contains(frame.Error.Message, stallPhaseAfterFirstChunk) {
		t.Errorf("message = %q, want it to name the phase %q", frame.Error.Message, stallPhaseAfterFirstChunk)
	}
	if !strings.Contains(frame.Error.Message, "budget 20ms") {
		t.Errorf("message = %q, want the env budget it fired on", frame.Error.Message)
	}
}

// TestStreamStallPayloadNamesPhase is the payload half of the contract: both
// phases reach the client as `code: STREAM_STALL` with the phase and the numbers
// in the message, so a client log distinguishes a slow prefill from a stream
// that died mid-turn. Built through the same two calls SSECopy's abort path uses.
func TestStreamStallPayloadNamesPhase(t *testing.T) {
	tests := []struct {
		name       string
		stall      *StreamStallError
		wantBudget string
	}{
		{
			name: "no first chunk",
			stall: &StreamStallError{
				Phase: stallPhaseFirstChunk, Budget: 30 * time.Millisecond,
				Waited: 31 * time.Millisecond, Label: "payload-first",
			},
			wantBudget: "budget 30ms",
		},
		{
			name: "stalled after first chunk",
			stall: &StreamStallError{
				Phase: stallPhaseAfterFirstChunk, Budget: 40 * time.Millisecond,
				SinceLast: 41 * time.Millisecond, Chunks: 3, Bytes: 512, Label: "payload-inter",
			},
			wantBudget: "budget 40ms",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Wrapped the way StallReader.Read surfaces it.
			cause := fmt.Errorf("read upstream after stall: %w", tt.stall)
			status, message := ClassifyStreamAbort(cause)
			if status != http.StatusGatewayTimeout {
				t.Errorf("status = %d, want 504", status)
			}
			if !strings.Contains(message, tt.stall.Phase) {
				t.Errorf("message = %q, want it to name the phase %q", message, tt.stall.Phase)
			}
			if !strings.Contains(message, tt.wantBudget) {
				t.Errorf("message = %q, want it to carry %q", message, tt.wantBudget)
			}

			rec := &mockResponseWriter{}
			if _, err := rec.Write(BuildStreamErrorBytes(status, message, SSEFormatOpenAI)); err != nil {
				t.Fatalf("write frame: %v", err)
			}
			frame, ok := errorFrame(t, rec.String())
			if !ok {
				t.Fatalf("no error frame in %q", rec.String())
			}
			if frame.Error.Code != StreamStallCode {
				t.Errorf("code = %q, want %q", frame.Error.Code, StreamStallCode)
			}
			if !strings.Contains(frame.Error.Message, tt.stall.Phase) {
				t.Errorf("payload message = %q, want the phase %q", frame.Error.Message, tt.stall.Phase)
			}
		})
	}
}
