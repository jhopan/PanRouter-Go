// CodeBuddy conversation + tracing identity — parity with
// open-sse/executors/codebuddy-intl.js (uuidV1 / stableConversationId /
// perTurnId / the B3 block) and tests/unit/codebuddy-intl-cli-identity.test.js.
//
// The ids are not decoration: the official CLI's conversation id is time-based,
// its per-turn id is fresh, and it sends Zipkin B3 + W3C traceparent + baggage
// on every chat call. A caller with none of that is not the SDK the gateway
// expects to see.
package executor

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	uuidShapeRE    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	cbTraceHex32RE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	cbTraceHex16RE = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// codebuddyIntlHeaders builds the intl lane's header set for one body.
func codebuddyIntlHeaders(t *testing.T, body string) map[string]string {
	t.Helper()
	raw := []byte(body)
	return codebuddyHeaders(&Request{
		Provider: "codebuddy-intl",
		APIKey:   codebuddyTestToken(codebuddyTestSub),
		Body:     raw,
	}, nil, raw)
}

func TestCodeBuddyTracingHeaders(t *testing.T) {
	h := codebuddyIntlHeaders(t, `{"messages":[{"role":"user","content":"hi"}]}`)

	if !cbTraceHex32RE.MatchString(h["X-B3-TraceId"]) {
		t.Errorf("X-B3-TraceId = %q, want 32 hex chars", h["X-B3-TraceId"])
	}
	if !cbTraceHex16RE.MatchString(h["X-B3-SpanId"]) {
		t.Errorf("X-B3-SpanId = %q, want 16 hex chars", h["X-B3-SpanId"])
	}
	if !cbTraceHex16RE.MatchString(h["X-B3-ParentSpanId"]) {
		t.Errorf("X-B3-ParentSpanId = %q, want 16 hex chars", h["X-B3-ParentSpanId"])
	}
	if h["X-B3-Sampled"] != "1" {
		t.Errorf("X-B3-Sampled = %q, want 1", h["X-B3-Sampled"])
	}
	if h["X-Trace-ID"] != h["X-B3-TraceId"] {
		t.Errorf("X-Trace-ID = %q, want the B3 trace id %q", h["X-Trace-ID"], h["X-B3-TraceId"])
	}
	if want := "00-" + h["X-B3-TraceId"] + "-" + h["X-B3-SpanId"] + "-01"; h["traceparent"] != want {
		t.Errorf("traceparent = %q, want %q", h["traceparent"], want)
	}
	if want := h["X-B3-TraceId"] + "-" + h["X-B3-SpanId"] + "-1-" + h["X-B3-ParentSpanId"]; h["b3"] != want {
		t.Errorf("b3 = %q, want %q", h["b3"], want)
	}
	// The span id and the parent span id are the two halves of the trace id in
	// the CLI's format, so a reader can reconstruct the chain.
	if h["X-B3-SpanId"]+h["X-B3-ParentSpanId"] != h["X-B3-TraceId"] {
		t.Errorf("span+parent = %q, want the trace id %q", h["X-B3-SpanId"]+h["X-B3-ParentSpanId"], h["X-B3-TraceId"])
	}
}

func TestCodeBuddyBaggageCarriesSessionAndTurn(t *testing.T) {
	h := codebuddyIntlHeaders(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	baggage := h["baggage"]
	if !strings.HasPrefix(baggage, "codebuddy.session_id=") {
		t.Fatalf("baggage = %q, want the session id first", baggage)
	}
	parts := strings.Split(baggage, ",")
	if len(parts) != 2 {
		t.Fatalf("baggage = %q, want exactly two entries", baggage)
	}
	sessionID := strings.TrimPrefix(parts[0], "codebuddy.session_id=")
	if sessionID != h["X-Conversation-ID"] {
		t.Errorf("baggage session id = %q, want the conversation id %q", sessionID, h["X-Conversation-ID"])
	}
	requestID := strings.TrimPrefix(parts[1], "codebuddy.conversation_request_id=")
	if !cbTraceHex32RE.MatchString(requestID) {
		t.Errorf("baggage request id = %q, want 32 hex chars (the uuid without dashes)", requestID)
	}
	if want := strings.ReplaceAll(h["X-Conversation-Request-ID"], "-", ""); requestID != want {
		t.Errorf("baggage request id = %q, want %q", requestID, want)
	}
}

func TestCodeBuddyConversationAndTurnIDs(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"the same turn"}]}`
	first := codebuddyIntlHeaders(t, body)
	second := codebuddyIntlHeaders(t, body)

	if !uuidShapeRE.MatchString(first["X-Conversation-ID"]) {
		t.Errorf("X-Conversation-ID = %q, want a uuid", first["X-Conversation-ID"])
	}
	// The conversation id is the SAME for the same conversation: it names the
	// conversation, so a new value each turn would not be one.
	if first["X-Conversation-ID"] != second["X-Conversation-ID"] {
		t.Errorf("conversation id changed between identical requests: %q vs %q",
			first["X-Conversation-ID"], second["X-Conversation-ID"])
	}
	// The per-turn ids are fresh, and the CLI reuses one value for the message,
	// request and root id on a turn.
	if first["X-Conversation-Request-ID"] == second["X-Conversation-Request-ID"] {
		t.Error("the per-turn request id must rotate")
	}
	for _, key := range []string{"X-Conversation-Request-ID", "X-Conversation-Message-ID", "X-Request-ID", "X-Root-Request-ID"} {
		if first[key] != first["X-Conversation-Request-ID"] {
			t.Errorf("%s = %q, want the turn's request id %q", key, first[key], first["X-Conversation-Request-ID"])
		}
		if !uuidShapeRE.MatchString(first[key]) {
			t.Errorf("%s = %q, want a uuid", key, first[key])
		}
	}
}

func TestCodeBuddyConversationKeyPrefersTheBodySession(t *testing.T) {
	// The CLI names its session in the body; a client that does must keep one
	// conversation id across turns even though its opening message changes.
	first := codebuddyIntlHeaders(t, `{"session_id":"sess-123","messages":[{"role":"user","content":"turn one"}]}`)["X-Conversation-ID"]
	second := codebuddyIntlHeaders(t, `{"session_id":"sess-123","messages":[{"role":"user","content":"turn one"},{"role":"assistant","content":"ok"},{"role":"user","content":"turn two"}]}`)["X-Conversation-ID"]
	if first != second {
		t.Errorf("the body's session_id must keep one conversation id: %q vs %q", first, second)
	}
	other := codebuddyIntlHeaders(t, `{"session_id":"sess-456","messages":[{"role":"user","content":"turn one"}]}`)["X-Conversation-ID"]
	if other == first {
		t.Error("a different session_id must be a different conversation")
	}

	// prompt_cache_key and conversation_id are the other spellings the wrappers use.
	for _, key := range []string{"prompt_cache_key", "conversation_id"} {
		a := codebuddyIntlHeaders(t, `{"`+key+`":"k-1","messages":[{"role":"user","content":"hi"}]}`)["X-Conversation-ID"]
		b := codebuddyIntlHeaders(t, `{"`+key+`":"k-1","messages":[{"role":"user","content":"a different message"}]}`)["X-Conversation-ID"]
		if a != b {
			t.Errorf("%s must key the conversation: %q vs %q", key, a, b)
		}
	}
}

func TestCodeBuddyConversationIDIsTimeBasedV1(t *testing.T) {
	codebuddyResetConversationStarts()
	t.Cleanup(codebuddyResetConversationStarts)

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	before := time.Now().Add(-2 * time.Second)
	first := codebuddyIntlHeaders(t, body)["X-Conversation-ID"]
	after := time.Now().Add(2 * time.Second)

	// Version 1: the third group starts with 1.
	if first[14] != '1' {
		t.Errorf("conversation id = %q, want a version-1 uuid", first)
	}
	// Variant: the fourth group's first hex digit is 8..b.
	if v := first[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Errorf("conversation id = %q, variant nibble = %c, want an RFC-4122 variant", first, v)
	}
	// The node id is the CLI's constant, so a reader can tell which client opened
	// the conversation.
	if !strings.HasSuffix(first, codebuddyUUIDv1Node) {
		t.Errorf("conversation id = %q, want the CLI's node id %s", first, codebuddyUUIDv1Node)
	}
	// The embedded timestamp is the moment the conversation was first seen.
	if got := codebuddyUUIDv1Time(t, first); got.Before(before) || got.After(after) {
		t.Errorf("embedded timestamp = %v, want between %v and %v", got, before, after)
	}

	// A later turn of the same conversation keeps the id — and therefore the
	// timestamp — even though wall-clock time has moved on.
	time.Sleep(5 * time.Millisecond)
	if second := codebuddyIntlHeaders(t, body)["X-Conversation-ID"]; second != first {
		t.Errorf("conversation id changed on a later turn: %q vs %q", first, second)
	}
	// A different conversation gets a different id.
	other := codebuddyIntlHeaders(t, `{"messages":[{"role":"user","content":"something else"}]}`)["X-Conversation-ID"]
	if other == first {
		t.Error("two conversations share one id")
	}
}

// codebuddyUUIDv1Time decodes the 60-bit timestamp a version-1 uuid carries.
func codebuddyUUIDv1Time(t *testing.T, id string) time.Time {
	t.Helper()
	flat := strings.ReplaceAll(id, "-", "")
	if len(flat) < 16 {
		t.Fatalf("uuid %q is too short to carry a timestamp", id)
	}
	parse := func(text string) uint64 {
		value, err := strconv.ParseUint(text, 16, 64)
		if err != nil {
			t.Fatalf("uuid %q has a non-hex group %q: %v", id, text, err)
		}
		return value
	}
	timeLow := parse(flat[0:8])
	timeMid := parse(flat[8:12])
	timeHigh := parse(flat[12:16])
	ticks := (timeHigh&0x0FFF)<<48 | timeMid<<32 | timeLow
	const gregorianOffset = 0x01b21dd213814000
	return time.UnixMilli(int64((ticks - gregorianOffset) / 10000))
}
