package log

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// TestTagForSession pins the dot each seed must receive.
//
// The expectations are the reference implementation's own output
// (src/sse/utils/logger.js tagForSession, run under node), not a restatement of
// the Go loop: two seeds here carry a non-BMP code point, and a Go port that
// hashed runes instead of the UTF-16 code units JavaScript walks would land on
// a different dot for both. Which dot an operator learned to associate with a
// conversation is the whole point of the field.
func TestTagForSession(t *testing.T) {
	tests := []struct {
		name string
		seed string
		want string
	}{
		{"short ascii", "s", "🟡"},
		{"single ascii", "x", "🟢"},
		{"three chars", "abc", "🟣"},
		{"session id", "session-1", "🟣"},
		{"claude session header", "claude-code-session-123", "🟣"},
		{"request id", "req-1", "🟣"},
		{"six chars", "abcdef", "🔴"},
		{"repeated letters", "zzzzz", "🟣"},
		{"model id", "th/deepseek-v4.1-flash:free", "🟠"},
		{"non-BMP seed", "🦖", "🟢"},
		{"non-BMP inside the seed", "cli-🦖-session", "⚪"},
		{"astral plane in the middle", "session-😀-2", "🟤"},
		{"long seed", strings.Repeat("a", 64), "🟢"},
		{"digit", "0", "🟢"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TagForSession(tt.seed)
			if got != tt.want {
				t.Errorf("TagForSession(%q) = %q, want %q", tt.seed, got, tt.want)
			}
			// The same seed must keep its dot: the tag correlates a whole
			// conversation, and a second call is the same request path as the
			// first attempt of it.
			if again := TagForSession(tt.seed); again != got {
				t.Errorf("TagForSession(%q) is unstable: %q then %q", tt.seed, got, again)
			}
			if !inTagSet(got) {
				t.Errorf("TagForSession(%q) = %q, which is not one of the eight tags", tt.seed, got)
			}
		})
	}
}

// An empty seed has no session to correlate, so it must take a rotating tag
// rather than always hashing to the first slot.
func TestTagForSession_EmptySeedRotates(t *testing.T) {
	first := TagForSession("")
	second := TagForSession("")
	if first == second {
		t.Errorf("two seedless calls both got %q, want the rotating fallback", first)
	}
	if !inTagSet(first) || !inTagSet(second) {
		t.Errorf("seedless tags %q / %q are not in the tag set", first, second)
	}
}

func TestNextTag_RotatesThroughTheSetAndWraps(t *testing.T) {
	seen := make(map[string]bool, TagCount)
	first := ""
	for i := range int(TagCount) {
		tag := NextTag()
		if !inTagSet(tag) {
			t.Fatalf("call %d returned %q, which is not one of the eight tags", i, tag)
		}
		if seen[tag] {
			t.Fatalf("call %d repeated %q before the set was exhausted", i, tag)
		}
		seen[tag] = true
		if i == 0 {
			first = tag
		}
	}
	if got := NextTag(); got != first {
		t.Errorf("after one full rotation NextTag() = %q, want %q", got, first)
	}
}

func TestRequestID_IsSixHexChars(t *testing.T) {
	tests := []struct {
		name   string
		reader *strings.Reader
	}{
		{"all zero bytes", strings.NewReader("\x00\x00\x00")},
		{"low bytes", strings.NewReader("\x01\x02\x03")},
		{"high bytes", strings.NewReader("\xfe\xff\x00")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer swapRequestIDReader(tt.reader)()
			got := RequestID()
			if len(got) != 6 {
				t.Fatalf("RequestID() = %q, want six characters", got)
			}
			for _, c := range got {
				if !isLowerHex(byte(c)) {
					t.Fatalf("RequestID() = %q, want lowercase hex only", got)
				}
			}
		})
	}
}

// A broken entropy source must still yield an id: the id only correlates two
// lines of a log, and losing it must never cost the request its line.
func TestRequestID_SurvivesABrokenReader(t *testing.T) {
	defer swapRequestIDReader(errorReader{})()
	got := RequestID()
	if len(got) != 6 {
		t.Fatalf("RequestID() = %q, want six characters even when the reader fails", got)
	}
	for _, c := range got {
		if !isLowerHex(byte(c)) {
			t.Fatalf("RequestID() = %q, want lowercase hex only", got)
		}
	}
}

func TestFmtThink(t *testing.T) {
	tests := []struct {
		name string
		in   ThinkIntent
		want string
	}{
		{"no intent", ThinkIntent{}, ""},
		{"disabled", ThinkIntent{Mode: "none"}, "off"},
		{"auto", ThinkIntent{Mode: "auto"}, "auto"},
		{"level", ThinkIntent{Mode: "level", Level: "high"}, "high"},
		{"level with budget ignored", ThinkIntent{Mode: "level", Level: "low", Budget: 1024}, "low"},
		{"budget in thousands", ThinkIntent{Mode: "budget", Budget: 10000}, "10k"},
		{"budget rounds up a half", ThinkIntent{Mode: "budget", Budget: 1500}, "2k"},
		{"budget rounds down below a half", ThinkIntent{Mode: "budget", Budget: 1400}, "1k"},
		{"budget exactly at the threshold", ThinkIntent{Mode: "budget", Budget: 1000}, "1k"},
		{"budget below the threshold is tokens", ThinkIntent{Mode: "budget", Budget: 999}, "999"},
		{"zero budget", ThinkIntent{Mode: "budget", Budget: 0}, "0"},
		{"unknown mode", ThinkIntent{Mode: "wat"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FmtThink(tt.in); got != tt.want {
				t.Errorf("FmtThink(%+v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestFormatRequestLine pins field order, separators and every optional field.
// The first case is the operator's own reference output, which is what the
// port has to reproduce line for line.
func TestFormatRequestLine(t *testing.T) {
	tests := []struct {
		name string
		in   RequestLine
		want string
	}{
		{
			name: "reference line",
			in: RequestLine{
				ClientModel: "th/deepseek-v4.1-flash:free",
				Provider:    "tokenharbor",
				Model:       "deepseek-v4.1-flash:free",
				SourceFmt:   "openai",
				TargetFmt:   "openai",
				Stream:      true,
				Messages:    245,
				Tools:       13,
				AccName:     "Key 84",
				AccID:       "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0",
				RequestID:   "9c6d61",
			},
			want: "POST th/deepseek-v4.1-flash:free → tokenharbor/deepseek-v4.1-flash:free · " +
				"FMT: openai→openai · STREAM · 245 MSG · 13 TOOL · ACC:Key 84 · REQ:9c6d61",
		},
		{
			name: "non-streaming with thinking and no tools",
			in: RequestLine{
				ClientModel: "gpt-5.4",
				Provider:    "tokenrouter",
				Model:       "gpt-5.4",
				SourceFmt:   "openai",
				TargetFmt:   "openai-responses",
				Messages:    2,
				Think:       "high",
				AccName:     "work",
				RequestID:   "abcdef",
			},
			want: "POST gpt-5.4 → tokenrouter/gpt-5.4 · FMT: openai→openai-responses · JSON · " +
				"2 MSG · THINK:high · ACC:work · REQ:abcdef",
		},
		{
			name: "tool count of zero is omitted",
			in: RequestLine{
				ClientModel: "m", Provider: "p", Model: "m",
				SourceFmt: "claude", TargetFmt: "claude",
				Messages: 1, Tools: 0, AccName: "n", RequestID: "000001",
			},
			want: "POST m → p/m · FMT: claude→claude · JSON · 1 MSG · ACC:n · REQ:000001",
		},
		{
			name: "absent thinking intent is omitted",
			in: RequestLine{
				ClientModel: "m", Provider: "p", Model: "m",
				SourceFmt: "openai", TargetFmt: "openai",
				Stream: true, Messages: 3, Tools: 2, Think: "",
				AccName: "n", RequestID: "000002",
			},
			want: "POST m → p/m · FMT: openai→openai · STREAM · 3 MSG · 2 TOOL · ACC:n · REQ:000002",
		},
		{
			name: "thinking mode off is a value, not an absence",
			in: RequestLine{
				ClientModel: "m", Provider: "p", Model: "m",
				SourceFmt: "openai", TargetFmt: "gemini",
				Messages: 1, Think: "off", AccName: "n",
			},
			want: "POST m → p/m · FMT: openai→gemini · JSON · 1 MSG · THINK:off · ACC:n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatRequestLine(tt.in); got != tt.want {
				t.Errorf("FormatRequestLine() =\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestFormatAccount(t *testing.T) {
	tests := []struct {
		name string
		acc  string
		id   string
		want string
	}{
		{"display name wins", "Key 84", "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0", "Key 84"},
		{"id is cut to eight characters", "", "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0", "0b1c2d3e"},
		{"short id is kept whole", "", "conn-7", "conn-7"},
		{"exactly eight characters", "", "12345678", "12345678"},
		{"id is cut on a rune boundary", "", "koneksi-🦖-panjang", "koneksi-"},
		{"nothing to report", "", "", "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatAccount(tt.acc, tt.id); got != tt.want {
				t.Errorf("FormatAccount(%q, %q) = %q, want %q", tt.acc, tt.id, got, tt.want)
			}
		})
	}
}

// TestFormatDoneLine pins the completion line, including the cache block's
// three states and the operator's own reference line.
func TestFormatDoneLine(t *testing.T) {
	tests := []struct {
		name string
		in   DoneLine
		want string
	}{
		{
			name: "reference line",
			in: DoneLine{
				TotalMs: 7622, TTFTMs: 6903,
				InTokens: 133730, OutTokens: 118, CacheRead: 132352,
			},
			want: "DONE 7622ms · TTFT 6903ms · IN 133730 (CACHE ↻132352) · OUT 118",
		},
		{
			name: "no cache figures",
			in:   DoneLine{TotalMs: 900, TTFTMs: 120, InTokens: 40, OutTokens: 7},
			want: "DONE 900ms · TTFT 120ms · IN 40 · OUT 7",
		},
		{
			name: "cache write only",
			in:   DoneLine{TotalMs: 1500, TTFTMs: 300, InTokens: 2000, OutTokens: 12, CacheWrite: 1900},
			want: "DONE 1500ms · TTFT 300ms · IN 2000 (CACHE +1900) · OUT 12",
		},
		{
			name: "cache read and write",
			in:   DoneLine{TotalMs: 1500, TTFTMs: 300, InTokens: 2000, OutTokens: 12, CacheRead: 900, CacheWrite: 1000},
			want: "DONE 1500ms · TTFT 300ms · IN 2000 (CACHE ↻900 +1000) · OUT 12",
		},
		{
			name: "ttft not measured is omitted",
			in:   DoneLine{TotalMs: 4200, InTokens: 10, OutTokens: 3, CacheRead: 5},
			want: "DONE 4200ms · IN 10 (CACHE ↻5) · OUT 3",
		},
		{
			name: "nothing measured",
			in:   DoneLine{},
			want: "DONE 0ms · IN 0 · OUT 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatDoneLine(tt.in); got != tt.want {
				t.Errorf("FormatDoneLine() =\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestFormatRTKLine(t *testing.T) {
	tests := []struct {
		name string
		in   RTKStats
		want string
	}{
		{
			name: "reference line",
			in: RTKStats{
				BytesBefore: 170413, BytesAfter: 165817, Hits: 12,
				Filters: []string{"dedup-log", "git-status", "build-output", "ls"},
			},
			want: "[RTK] saved 4596B / 170413B (2.7%) via [dedup-log,git-status,build-output,ls] hits=12",
		},
		{
			name: "nothing saved, nothing printed",
			in:   RTKStats{BytesBefore: 900, BytesAfter: 900, Filters: []string{"git-diff"}},
			want: "",
		},
		{
			name: "single filter, whole input",
			in:   RTKStats{BytesBefore: 1000, BytesAfter: 400, Hits: 1, Filters: []string{"git-diff"}},
			want: "[RTK] saved 600B / 1000B (60.0%) via [git-diff] hits=1",
		},
		{
			name: "no byte was weighed keeps the reference's zero",
			in:   RTKStats{BytesBefore: 0, BytesAfter: 0, Hits: 1, Filters: []string{"git-diff"}},
			want: "[RTK] saved 0B / 0B (0%) via [git-diff] hits=1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatRTKLine(tt.in); got != tt.want {
				t.Errorf("FormatRTKLine() =\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestFormatCavemanLine(t *testing.T) {
	for _, level := range []string{"lite", "full", "ultra"} {
		if got, want := FormatCavemanLine(level), "CAVEMAN:"+level; got != want {
			t.Errorf("FormatCavemanLine(%q) = %q, want %q", level, got, want)
		}
	}
}

// TestLine_ReachesTheConsoleBuffer is the contract that matters to the
// dashboard: the operator watches the console panel, so a line printed only to
// the process log is a line they never see.
func TestLine_ReachesTheConsoleBuffer(t *testing.T) {
	ClearConsoleLogs()
	defer SetLevel(LevelInfo)
	SetLevel(LevelInfo)
	defer swapLineClock(func() time.Time { return time.Date(2026, 11, 15, 21, 11, 36, 0, time.UTC) })()

	Line("🟡", "▶", "POST th/deepseek-v4.1-flash:free → tokenharbor/deepseek-v4.1-flash:free")
	Line("🟡", "📊", "DONE 7622ms · IN 133730 · OUT 118")

	entries := ConsoleEntries()
	if len(entries) != 2 {
		t.Fatalf("expected 2 buffered entries, got %d: %+v", len(entries), entries)
	}
	first := entries[0]
	if want := "[21:11:36] 🟡 ▶ POST th/deepseek-v4.1-flash:free → tokenharbor/deepseek-v4.1-flash:free"; first.Line != want {
		t.Errorf("buffered line = %q, want %q", first.Line, want)
	}
	if first.Level != LevelInfo {
		t.Errorf("buffered level = %v, want %v", first.Level, LevelInfo)
	}
	if !strings.HasPrefix(entries[1].Line, "[21:11:36] 🟡 📊 DONE 7622ms") {
		t.Errorf("second buffered line = %q", entries[1].Line)
	}
}

// A raw line is one the reference prints with a bare console.log, so it must
// reach the buffer without a tag or a timestamp being added to it.
func TestRaw_ReachesTheConsoleBufferUnframed(t *testing.T) {
	ClearConsoleLogs()
	defer SetLevel(LevelInfo)
	SetLevel(LevelInfo)

	const line = "[RTK] saved 4596B / 170413B (2.7%) via [dedup-log,git-status,build-output,ls] hits=12"
	Raw(line)

	entries := ConsoleEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 buffered entry, got %d", len(entries))
	}
	if entries[0].Line != line {
		t.Errorf("buffered line = %q, want %q", entries[0].Line, line)
	}

	// An empty line is not a line.
	Raw("")
	if got := len(ConsoleEntries()); got != 1 {
		t.Errorf("an empty raw line was buffered: %d entries", got)
	}
}

// The terminal half of the contract: the line carries the reference's single
// [HH:MM:SS], not that stamp plus the standard logger's own date-and-time
// prefix — two timestamps on one line is not what the operator pasted, and the
// two would disagree about when the line happened.
func TestLine_PrintedWithASingleTimestamp(t *testing.T) {
	ClearConsoleLogs()
	defer SetLevel(LevelInfo)
	SetLevel(LevelInfo)
	defer swapLineClock(func() time.Time { return time.Date(2026, 11, 15, 21, 11, 36, 0, time.UTC) })()

	var printed bytes.Buffer
	log.SetOutput(&printed)
	defer log.SetOutput(os.Stderr)

	Line("🟡", "▶", "POST m → p/m")

	want := "[21:11:36] 🟡 ▶ POST m → p/m\n"
	if got := printed.String(); got != want {
		t.Errorf("printed line = %q, want %q", got, want)
	}
}

// Above INFO these lines are off, exactly as upstream's `LEVEL > INFO` guard
// turns them off — including the level check the caller uses to decide whether
// to build one.
func TestLine_SuppressedAboveInfo(t *testing.T) {
	ClearConsoleLogs()
	defer SetLevel(LevelInfo)
	SetLevel(LevelWarn)

	if LineEnabled() {
		t.Error("LineEnabled() is true at WARN, so callers would build lines that are never printed")
	}
	Line("🟡", "▶", "POST m")
	Raw("[RTK] saved 1B / 2B (50.0%) via [git-diff] hits=1")
	if got := len(ConsoleEntries()); got != 0 {
		t.Fatalf("expected no buffered lines above INFO, got %d", got)
	}
}

func inTagSet(tag string) bool {
	for _, candidate := range reqTags {
		if candidate == tag {
			return true
		}
	}
	return false
}

func isLowerHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

func swapRequestIDReader(r io.Reader) func() {
	prev := requestIDReader
	requestIDReader = r
	return func() { requestIDReader = prev }
}

func swapLineClock(now func() time.Time) func() {
	prev := lineClock
	lineClock = now
	return func() { lineClock = prev }
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }
