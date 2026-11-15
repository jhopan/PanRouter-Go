package chat

import (
	"bytes"
	"context"
	"encoding/json/jsontext"

	"panrouter/proxy/internal/log"
	"panrouter/proxy/internal/providers"
	"panrouter/proxy/internal/thinking"
	"panrouter/proxy/internal/tokensaver"
	"panrouter/proxy/internal/translator"
)

// tokenSaverResult is what applyTokenSavers did to one request body: the bytes
// that go upstream, the token estimate the usage ledger already records, and the
// two figures the correlated console lines print.
type tokenSaverResult struct {
	Body         []byte
	OrigTokens   int
	SavedTokens  int
	SavedPct     int
	RTK          tokensaver.MessageCompressionStats
	CavemanLevel string // the level whose prompt was injected; "" when none was
}

// emitAttemptLines prints one attempt's correlated lines: the request line, then
// the token-saver lines this path produced. Port of the ▶ / ⚙ / [RTK] output in
// open-sse/handlers/chatCore.js:258-279, :348 and open-sse/rtk/index.js:151-157.
//
// Order is the operator's: the saver lines explain the request line above them,
// so a reader sees which account served the turn and what shaping it cost before
// the filters are named. The caller has already checked log.LineEnabled.
func emitAttemptLines(tag string, req log.RequestLine, saver tokenSaverResult) {
	log.Line(tag, "▶", log.FormatRequestLine(req))
	if line := log.FormatRTKLine(log.RTKStats{
		BytesBefore: saver.RTK.BytesBefore,
		BytesAfter:  saver.RTK.BytesAfter,
		Hits:        saver.RTK.Hits,
		Filters:     saver.RTK.Filters,
	}); line != "" {
		// Upstream prints this one with a bare console.log, so it carries no
		// tag and no timestamp of its own.
		log.Raw(line)
	}
	if saver.CavemanLevel != "" {
		log.Line(tag, "⚙", log.FormatCavemanLine(saver.CavemanLevel))
	}
}

// thinkForLine renders the reasoning intent the request line reports, in the
// vocabulary log.FmtThink prints. A request that carried none has no field.
func thinkForLine(in *thinking.Intent) string {
	if in == nil {
		return ""
	}
	return log.FmtThink(log.ThinkIntent{Mode: string(in.Mode), Level: in.Level, Budget: in.Budget})
}

// clientWireFormat names the format the client sent — the left half of the FMT:
// field (upstream detectFormat on the client body, chatCore.js:87). A Messages
// request is Claude whatever the upstream speaks; everything else is the wire
// the client-format context records, which is exactly the providers.Format*
// vocabulary the line prints.
func clientWireFormat(ctx context.Context, endpoint string) string {
	if endpoint == "/v1/messages" || endpoint == "/v1/v1/messages" {
		return providers.FormatClaude
	}
	return string(translator.ClientFormatFrom(ctx))
}

// upstreamWireFormat names the format the upstream speaks — the right half of
// FMT:. Same precedence the translator playground resolves with
// (translator_playground.go:530 translatorTargetFormat): the model's declared
// lane, then the provider's registry wire, then Chat Completions.
func upstreamWireFormat(provider, model string, cfg *providers.ProviderConfig) string {
	if formats, ok := providers.GetModelFormats(provider, model); ok && formats.TargetFormat != "" {
		return formats.TargetFormat
	}
	if cfg != nil && cfg.Format != "" {
		return cfg.Format
	}
	return providers.FormatOpenAI
}

// bodyShapeCounts returns the message and tool counts the request line prints
// (upstream reads them off the body it dispatches, chatCore.js:262-263).
//
// The walk streams the body instead of decoding it: only two lengths are wanted,
// and materialising a conversation's worth of messages to count them would cost
// a second copy of the whole request on the box that serves it.
func bodyShapeCounts(body []byte) (msgs, tools int) {
	if len(body) == 0 {
		return 0, 0
	}
	dec := jsontext.NewDecoder(bytes.NewBuffer(body))
	if dec.PeekKind() != '{' {
		return 0, 0
	}
	if _, err := dec.ReadToken(); err != nil {
		return 0, 0
	}
	for {
		if dec.PeekKind() != '"' {
			return msgs, tools
		}
		tok, err := dec.ReadToken()
		if err != nil {
			return msgs, tools
		}
		key := tok.String()
		if dec.PeekKind() == '[' {
			if _, err := dec.ReadToken(); err != nil {
				return msgs, tools
			}
			n := countArrayElements(dec)
			// Upstream takes the first of these that has entries, in this
			// order: the same turn is `messages` in Chat and Claude format,
			// `input` on the Responses wire and `contents` in Gemini.
			switch key {
			case "messages", "input", "contents":
				if msgs == 0 {
					msgs = n
				}
			case "tools":
				tools = n
			}
			continue
		}
		if err := dec.SkipValue(); err != nil {
			return msgs, tools
		}
	}
}

// countArrayElements counts the values of the array the decoder has just opened,
// skipping each one's contents.
func countArrayElements(dec *jsontext.Decoder) int {
	n := 0
	for {
		kind := dec.PeekKind()
		if kind == ']' {
			_, _ = dec.ReadToken()
			return n
		}
		if kind == 0 {
			return n // truncated body: report what was counted
		}
		if err := dec.SkipValue(); err != nil {
			return n
		}
		n++
	}
}