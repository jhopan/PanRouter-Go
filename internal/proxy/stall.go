package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/shutdown"
)

// ErrStreamStall reports that the stall watchdog fired, so a pending Read
// fails with this instead of the bare "file already closed" the Close would
// otherwise produce. Callers that must tell a timeout apart from a lost
// socket (an in-band SSE error frame, for instance) match on it.
var ErrStreamStall = errors.New("stream stall timeout")

// StreamStallCode is the machine-readable tag upstream puts on a stall error
// (`code: "STREAM_STALL"`), carried into the in-band error frame so a client
// can branch on the cause without matching prose.
const StreamStallCode = "STREAM_STALL"

// StreamStallError says which phase expired, on which budget, after how long —
// the difference between "the provider is slow" and "the provider is dead" is
// exactly those three numbers, and upstream's bare "stream stall timeout" left
// the log unable to tell them apart.
type StreamStallError struct {
	Phase     string
	Budget    time.Duration
	Waited    time.Duration
	SinceLast time.Duration
	Chunks    int64
	Bytes     int64
	Label     string
}

func (e *StreamStallError) Error() string {
	if e.Phase == stallPhaseFirstChunk {
		return fmt.Sprintf("stream %s (waited %s, budget %s)", e.Phase, roundMS(e.Waited), roundMS(e.Budget))
	}
	return fmt.Sprintf("stream %s (no data for %s, budget %s)", e.Phase, roundMS(e.SinceLast), roundMS(e.Budget))
}

// Unwrap makes errors.Is(err, ErrStreamStall) true for every consumer that
// already classifies stalls.
func (e *StreamStallError) Unwrap() error { return ErrStreamStall }

// Code is the tag the in-band error frame publishes.
func (e *StreamStallError) Code() string { return StreamStallCode }

func roundMS(d time.Duration) time.Duration { return d.Round(time.Millisecond) }

func (e *StreamStallError) keyvals() []any {
	return []any{
		"label", e.Label, "phase", e.Phase, "budget_ms", e.Budget.Milliseconds(),
		"waited_ms", e.Waited.Milliseconds(), "since_last_ms", e.SinceLast.Milliseconds(),
		"chunks", e.Chunks, "bytes", e.Bytes,
	}
}

// StallReader wraps an io.ReadCloser with a watchdog that fires when the stream
// stays silent for longer than the budget for its current phase. When it fires,
// the underlying reader is closed, which unblocks any pending Read call.
// Matches Next.js stall detection in pipeWithDisconnect.
type StallReader struct {
	reader  io.ReadCloser
	budgets StallBudgets
	label   string

	mu        sync.Mutex
	timer     *time.Timer
	startedAt time.Time
	lastAt    time.Time
	sawChunk  bool
	chunks    int64
	bytes     int64
	fired     *StreamStallError

	once     sync.Once
	doneOnce sync.Once
	done     chan struct{} // closed on Close; stops the shutdown watcher
	stalled  atomic.Bool   // watchdog fired; Read reports ErrStreamStall
}

// DefaultFirstEventTimeout is upstream's first-event deadline for the Cloud
// Code Assist surface (OMP DEFAULT_FIRST_EVENT_TIMEOUT_MS = 300000).
const DefaultFirstEventTimeout = 5 * time.Minute

// FlashFirstEventTimeout is the tighter first-event budget the Gemini Flash
// families get (OMP classes/gemini.kdl stream-first-event-timeout-ms 60000):
// they answer quickly or not at all.
const FlashFirstEventTimeout = 60 * time.Second

// NewStallReaderWithFirstEvent keeps upstream's call shape — an inter-chunk
// timeout plus a separate first-event deadline — while the watchdog underneath
// runs on this package's two-budget StallBudgets. A zero timeout or
// firstEventTimeout leaves that phase at its compiled default, which is the
// same fallback upstream applies.
func NewStallReaderWithFirstEvent(ctx context.Context, rc io.ReadCloser, timeout, firstEventTimeout time.Duration, label string) io.ReadCloser {
	budgets := DefaultStallBudgets()
	if timeout > 0 {
		budgets.InterChunk = timeout
	}
	if firstEventTimeout > 0 {
		budgets.FirstChunk = firstEventTimeout
	}
	return NewStallReaderWithContext(ctx, rc, budgets, label)
}

// NewStallReader wraps rc with stall detection on the given budgets.
func NewStallReader(rc io.ReadCloser, budgets StallBudgets, label string) io.ReadCloser {
	return NewStallReaderWithContext(context.Background(), rc, budgets, label)
}

// NewStallReaderWithContext wraps rc with stall detection and context cancellation.
//
// The watchdog runs on two budgets, because the two phases measure different
// things: before the first byte the provider is prefilling the prompt (slow is
// normal on a large context), and after it the stream is silent (slow is a
// symptom). Whichever budget expires produces a *StreamStallError naming the
// phase and carrying both numbers.
//
// In addition to stall detection and process shutdown, if ctx is canceled (e.g.
// client disconnects / aborts), the reader is immediately closed to unblock
// pending reads and prevent socket leaks on Windows (FIN_WAIT_1 / CLOSE_WAIT).
func NewStallReaderWithContext(ctx context.Context, rc io.ReadCloser, budgets StallBudgets, label string) io.ReadCloser {
	if budgets.FirstChunk <= 0 {
		budgets.FirstChunk = ResolveDefaultFirstChunkTimeout()
	}
	if budgets.InterChunk <= 0 {
		budgets.InterChunk = ResolveDefaultStallTimeout()
	}
	now := time.Now()
	s := &StallReader{
		reader:    rc,
		budgets:   budgets,
		label:     label,
		startedAt: now,
		lastAt:    now,
		done:      make(chan struct{}),
	}
	s.arm()
	go func() {
		if ctx != nil && ctx.Done() != nil {
			select {
			case <-shutdown.Done():
				log.Info("stream", "shutdown, closing stream", "label", label)
				s.Close()
			case <-ctx.Done():
				log.Info("stream", "client context canceled, closing stream", "label", label)
				s.Close()
			case <-s.done:
			}
		} else {
			select {
			case <-shutdown.Done():
				log.Info("stream", "shutdown, closing stream", "label", label)
				s.Close()
			case <-s.done:
			}
		}
	}()
	return s
}

// arm installs the timer for the phase the stream is in.
func (s *StallReader) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armLocked()
}

func (s *StallReader) armLocked() {
	budget := s.budgets.FirstChunk
	if s.sawChunk {
		budget = s.budgets.InterChunk
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(budget, func() { s.fire(budget) })
}

func (s *StallReader) fire(budget time.Duration) {
	s.mu.Lock()
	phase := stallPhaseFirstChunk
	if s.sawChunk {
		phase = stallPhaseAfterFirstChunk
	}
	now := time.Now()
	stallErr := &StreamStallError{
		Phase:     phase,
		Budget:    budget,
		Waited:    now.Sub(s.startedAt),
		SinceLast: now.Sub(s.lastAt),
		Chunks:    s.chunks,
		Bytes:     s.bytes,
		Label:     s.label,
	}
	s.fired = stallErr
	s.mu.Unlock()

	log.Warn("stream", "stall detected", stallErr.keyvals()...)
	s.stalled.Store(true)
	s.Close()
}

// Read implements io.Reader. Each successful call re-arms the watchdog on the
// budget for the phase the stream is now in.
func (s *StallReader) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	if n > 0 {
		s.mu.Lock()
		s.sawChunk = true
		s.chunks++
		s.bytes += int64(n)
		s.lastAt = time.Now()
		s.mu.Unlock()
	}
	if err == nil {
		s.arm()
	}
	if err != nil {
		s.mu.Lock()
		if s.timer != nil {
			s.timer.Stop()
		}
		s.mu.Unlock()
	}
	// The watchdog closes the reader out from under this Read, so the raw
	// error is an opaque "file already closed". Translate it back into the
	// timeout it actually was, so callers can tell a stall from a lost socket.
	if err != nil && s.stalled.Load() {
		if stallErr := s.StallError(); stallErr != nil {
			return n, fmt.Errorf("read upstream after stall: %w", stallErr)
		}
		return n, fmt.Errorf("read upstream after stall: %w", ErrStreamStall)
	}
	return n, err
}

// StallError returns the stall the watchdog recorded, or nil when it never
// fired.
func (s *StallReader) StallError() *StreamStallError {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fired
}

// Close implements io.Closer. Stops the stall timer, stops the shutdown
// watcher, and closes the reader. Idempotent and synchronized with the timer
// and shutdown goroutines: whichever of Close, the stall-fire, or the shutdown
// watcher runs first closes the reader exactly once via s.once.
func (s *StallReader) Close() error {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Unlock()
	s.doneOnce.Do(func() { close(s.done) })
	var err error
	s.once.Do(func() { err = s.reader.Close() })
	return err
}
