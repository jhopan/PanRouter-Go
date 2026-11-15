package log

// Correlated console lines: the port of PanRouter's line() family
// (src/sse/utils/logger.js).
//
// These lines are a second, human-facing view of a request. The structured
// logger says what happened; this one says which account served the request and
// what it cost, in the one format an operator compares against the Next
// reference. They are additional output: nothing about what reaches the
// database, the usage ledger or the structured logs changes.
//
// Every line is also captured into the console buffer the dashboard streams, so
// the console panel and the terminal show the same text.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

// reqTags are the eight coloured dots that correlate the lines of one session.
// Port of REQ_TAGS (src/sse/utils/logger.js:33).
var reqTags = [...]string{"🟢", "🔵", "🟣", "🟡", "🟠", "🔴", "⚪", "🟤"}

// TagCount is the size of the tag set, for callers that reason about the dots
// (tests, tooling) rather than printing one.
const TagCount = uint64(len(reqTags))

// tagCursor is the rotating fallback cursor. Atomic because two requests that
// share no session seed allocate a tag concurrently.
var tagCursor atomic.Uint64

// NextTag allocates the next rotating tag, the fallback for a request that has
// no session seed. Port of nextTag() (logger.js:38-42).
//
// The counter advances before the modulo, so two concurrent callers never
// receive the same dot unless the set has genuinely wrapped around.
func NextTag() string {
	return reqTags[(tagCursor.Add(1)-1)%TagCount]
}

// TagForSession derives the stable tag of a session seed: the same seed always
// maps to the same dot, which is what makes one CLI conversation's lines
// readable as a unit. Port of tagForSession (logger.js:45-50).
//
// The hash wraps as an int32 and walks UTF-16 code units, so a seed whose
// colour an operator learned from the Next build keeps its colour here.
func TagForSession(seed string) string {
	if seed == "" {
		return NextTag()
	}
	var h int32
	for _, unit := range utf16.Encode([]rune(seed)) {
		h = h*31 + int32(unit)
	}
	n := int64(h)
	if n < 0 {
		// Not bit-negation: a seed hashing to math.MinInt32 must land on slot 0
		// exactly as Math.abs does upstream, not on the negative remainder Go
		// would produce.
		n = -n
	}
	return reqTags[n%int64(len(reqTags))]
}

// requestIDReader is the entropy source behind RequestID. A variable so a test
// can pin the id instead of matching a random one.
var requestIDReader io.Reader = rand.Reader

// RequestID returns the six lowercase hex characters that identify one request
// attempt. Port of requestId() (logger.js:60-62).
//
// The tag identifies the session, which is what makes a conversation readable,
// but two attempts of that conversation (a stall, then the retry) carry the same
// tag; this id is per attempt, so a request line and the error line of the
// attempt that produced it can be matched.
func RequestID() string {
	var b [3]byte
	if _, err := io.ReadFull(requestIDReader, b[:]); err != nil {
		// The id is diagnostic: a broken entropy source must not cost a request
		// its line, and the clock is unique enough for the correlation it does.
		return fmt.Sprintf("%06x", uint32(time.Now().UnixNano())&0xffffff)
	}
	return hex.EncodeToString(b[:])
}

// lineClock stamps Line and mirrors formatTime() (logger.js:28-30), which
// prints a 24-hour wall clock with no date. A variable so a test can pin the
// second a line carries.
var lineClock = time.Now

// LineEnabled reports whether a Line would be printed. A caller that has to
// build a line's fields checks it first: the request line walks a request body,
// and the level has to be able to refuse before that cost is paid.
//
// Port of the level guard in line() (logger.js:70): these lines are INFO output,
// so any level above INFO silences them.
func LineEnabled() bool {
	return shouldLog(LevelInfo)
}

// Line prints one correlated console line: [HH:MM:SS] <tag> <symbol> <message>.
// Port of line() (logger.js:69-73).
func Line(tag, symbol, message string) {
	if !LineEnabled() {
		return
	}
	emitLine(renderLine(lineClock(), tag, symbol, message))
}

// Raw prints a line that already carries its own framing, capturing it the same
// way. The token-saver lines are printed this way upstream — a bare
// console.log rather than line() — so they must reach the console panel without
// a tag the reference does not put there.
func Raw(line string) {
	if !LineEnabled() || line == "" {
		return
	}
	emitLine(line)
}

func renderLine(now time.Time, tag, symbol, message string) string {
	var b strings.Builder
	b.Grow(len(tag) + len(symbol) + len(message) + 16)
	b.WriteByte('[')
	b.WriteString(now.Format("15:04:05"))
	b.WriteString("] ")
	b.WriteString(tag)
	b.WriteByte(' ')
	b.WriteString(symbol)
	b.WriteByte(' ')
	b.WriteString(message)
	return b.String()
}

// emitLine captures the line for the dashboard's console panel and writes it to
// the process log, the same destination every other line from this package goes
// to, so LOG_FILE redirection cannot lose half the output.
//
// It writes through the logger's writer rather than through Println: these lines
// carry their own [HH:MM:SS], and the standard logger's prefix would stamp a
// second timestamp onto a line the reference prints with one. The buffer still
// receives the rendered text unchanged.
func emitLine(line string) {
	captureConsole(LevelInfo, line)
	// A destination that refuses the write is the destination's problem: the
	// console panel already has the line.
	_, _ = io.WriteString(log.Default().Writer(), line+"\n")
}

// ThinkIntent is the reasoning request a line reports, in the shape
// internal/thinking normalises a request into.
type ThinkIntent struct {
	Mode   string // "none" | "auto" | "level" | "budget"
	Level  string // set when Mode is "level"
	Budget int    // thinking tokens, set when Mode is "budget"
}

// FmtThink renders a thinking intent for the request line. Port of fmtThink
// (logger.js:88-97): "off", "auto", a level name, or a budget in thousands.
// "" means the request carried no intent, and the field is then omitted.
func FmtThink(in ThinkIntent) string {
	switch in.Mode {
	case "none":
		return "off"
	case "auto":
		return "auto"
	case "budget":
		if in.Budget >= 1000 {
			// Math.round on a non-negative budget: halves round up.
			return strconv.Itoa((in.Budget+500)/1000) + "k"
		}
		return strconv.Itoa(in.Budget)
	case "level":
		return in.Level
	}
	return ""
}

// RequestLine is the input of the correlated request line, one per attempt.
type RequestLine struct {
	ClientModel string // the model the client asked for
	Provider    string
	Model       string
	SourceFmt   string // the format the client sent
	TargetFmt   string // the format the upstream speaks
	Stream      bool
	Messages    int
	Tools       int
	Think       string // FmtThink output; "" omits THINK
	AccName     string // connection display name, when it has one
	AccID       string // connection id, printed as its first eight characters
	RequestID   string // RequestID(); "" omits REQ
}

// FormatRequestLine renders the request line:
//
//	POST <clientModel> → <provider>/<model> · FMT: <source>→<target> ·
//	STREAM|JSON · <n> MSG · [<n> TOOL] · [THINK:<x>] · ACC:<name|id8|-> ·
//	[REQ:<id6>]
//
// Field order, separators and the optional fields are upstream's
// (chatCore.js:258-279): TOOL only when the body declares tools, THINK only
// when the request carried a reasoning intent, REQ only when an attempt id
// exists.
func FormatRequestLine(r RequestLine) string {
	var b strings.Builder
	b.Grow(len(r.ClientModel) + len(r.Provider) + len(r.Model) + len(r.AccName) + 64)
	b.WriteString("POST ")
	b.WriteString(r.ClientModel)
	b.WriteString(" → ")
	b.WriteString(r.Provider)
	b.WriteByte('/')
	b.WriteString(r.Model)
	b.WriteString(" · FMT: ")
	b.WriteString(r.SourceFmt)
	b.WriteString("→")
	b.WriteString(r.TargetFmt)
	b.WriteString(" · ")
	if r.Stream {
		b.WriteString("STREAM")
	} else {
		b.WriteString("JSON")
	}
	b.WriteString(" · ")
	b.WriteString(strconv.Itoa(r.Messages))
	b.WriteString(" MSG")
	if r.Tools > 0 {
		b.WriteString(" · ")
		b.WriteString(strconv.Itoa(r.Tools))
		b.WriteString(" TOOL")
	}
	if r.Think != "" {
		b.WriteString(" · THINK:")
		b.WriteString(r.Think)
	}
	b.WriteString(" · ACC:")
	b.WriteString(FormatAccount(r.AccName, r.AccID))
	if r.RequestID != "" {
		b.WriteString(" · REQ:")
		b.WriteString(r.RequestID)
	}
	return b.String()
}

// FormatAccount names the account that served an attempt: its display name when
// it has one, else the first eight characters of its id, else "-"
// (chatCore.js:276).
func FormatAccount(name, id string) string {
	if name != "" {
		return name
	}
	if id == "" {
		return "-"
	}
	// Cut on a rune boundary: an id is an ASCII identifier in practice, and a
	// byte cut that landed mid-rune would put invalid UTF-8 in the console.
	seen := 0
	for i := range id {
		if seen == 8 {
			return id[:i]
		}
		seen++
	}
	return id
}

// DoneLine is the usage summary of an attempt that finished.
type DoneLine struct {
	TotalMs    int64
	TTFTMs     int64 // 0 when the first token was never timed
	InTokens   int
	OutTokens  int
	CacheRead  int // prompt tokens served from the provider's cache
	CacheWrite int // prompt tokens written into it
}

// FormatDoneLine renders the completion line:
//
//	DONE <total>ms[ · TTFT <ttft>ms] · IN <in>[ (CACHE ↻<read>[ +<write>])] ·
//	OUT <out>
//
// Port of formatDoneLine (open-sse/handlers/chatCore/requestDetail.js:86-101).
// The CACHE block appears only when one of its two figures is non-zero and TTFT
// only when it was measured: a zero there means "not measured", not "instant",
// and printing it would read as a fast first token the request never had.
func FormatDoneLine(d DoneLine) string {
	var b strings.Builder
	b.Grow(48)
	b.WriteString("DONE ")
	b.WriteString(strconv.FormatInt(d.TotalMs, 10))
	b.WriteString("ms")
	if d.TTFTMs > 0 {
		b.WriteString(" · TTFT ")
		b.WriteString(strconv.FormatInt(d.TTFTMs, 10))
		b.WriteString("ms")
	}
	b.WriteString(" · IN ")
	b.WriteString(strconv.Itoa(d.InTokens))
	if d.CacheRead > 0 || d.CacheWrite > 0 {
		b.WriteString(" (CACHE")
		if d.CacheRead > 0 {
			b.WriteString(" ↻")
			b.WriteString(strconv.Itoa(d.CacheRead))
		}
		if d.CacheWrite > 0 {
			b.WriteString(" +")
			b.WriteString(strconv.Itoa(d.CacheWrite))
		}
		b.WriteByte(')')
	}
	b.WriteString(" · OUT ")
	b.WriteString(strconv.Itoa(d.OutTokens))
	return b.String()
}

// RTKStats is what one RTK pass saved, in the terms the console line reports:
// the tool-output bytes it weighed before and after, the filters that claimed a
// body, and how many bodies shrank.
type RTKStats struct {
	BytesBefore int
	BytesAfter  int
	Hits        int
	Filters     []string // first-seen order, no repeats
}

// FormatRTKLine renders the savings line:
//
//	[RTK] saved <bytes>B / <total>B (<pct>%) via [<filters>] hits=<n>
//
// Port of formatRtkLog (open-sse/rtk/index.js:151-157), including its
// one-decimal percentage, its first-seen filter order and the hits= tail. ""
// means the pass saved nothing, and there is then no line to print.
func FormatRTKLine(s RTKStats) string {
	if s.Hits == 0 {
		return ""
	}
	saved := s.BytesBefore - s.BytesAfter
	pct := "0"
	if s.BytesBefore > 0 {
		pct = strconv.FormatFloat(float64(saved)/float64(s.BytesBefore)*100, 'f', 1, 64)
	}
	var b strings.Builder
	b.Grow(64 + len(s.Filters)*12)
	b.WriteString("[RTK] saved ")
	b.WriteString(strconv.Itoa(saved))
	b.WriteString("B / ")
	b.WriteString(strconv.Itoa(s.BytesBefore))
	b.WriteString("B (")
	b.WriteString(pct)
	b.WriteString("%) via [")
	b.WriteString(strings.Join(s.Filters, ","))
	b.WriteString("] hits=")
	b.WriteString(strconv.Itoa(s.Hits))
	return b.String()
}

// FormatCavemanLine renders the caveman entry of the saver-flag line. Port of
// the CAVEMAN entry that xf carries (chatCore.js:318-322) and that is printed at
// :348: the mode is the level whose prompt was injected.
func FormatCavemanLine(level string) string {
	return "CAVEMAN:" + level
}
