package clientconfig

import (
	"bytes"
	"encoding/json"
	"errors"
)

// JSON rendering for the adapters whose upstream route builds the document in
// the route rather than splicing keys (copilot's root array, cowork's derived
// config set, openclaw's rebuilt agents/models trees).
//
// The upstream routes write JSON.stringify(value, null, 2) and nothing else, so
// the document form here keeps that shape: two-space indent, no trailing
// newline. HTML escaping is off because JSON.stringify never escapes <, > or &,
// and turning a URL like ".../mcp?x=1&y=2" into \u0026 is a byte the user would
// see and upstream would not write.

// jsonDocument renders v as JSON.stringify(v, null, 2).
func jsonDocument(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// jsonInline renders v as compact JSON for a ValueJSON replacement, so the
// value a caller splices into a file keeps that file's own layout instead of
// carrying the encoder's indentation.
func jsonInline(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return string(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// readClientFile returns an adapter target's current bytes. A missing directory,
// missing file or a guard that cannot be resolved because the client never ran
// here is nil bytes and no error: for these adapters "no file yet" is the
// ordinary first-run case, and their upstream routes treat it the same way
// (readFile in a try/catch, then start from the default).
func readClientFile(file TargetFile) ([]byte, error) {
	g, err := resolveGuard(file.Dir, relFromDir(file.Dir, file.Path))
	if err != nil {
		if errors.Is(err, ErrBaseMissing) {
			return nil, nil
		}
		return nil, err
	}
	return readThroughGuard(g)
}

// parseClientJSON decodes an adapter-owned JSON document. Empty bytes are the
// zero value, not an error, for the same first-run reason.
//
// Trailing commas are tolerated before decoding because the upstream routes
// strip exactly those before JSON.parse; every other malformation is reported
// rather than swallowed, so a file a person broke is not silently replaced.
func parseClientJSON(b []byte, out any) error {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	return json.Unmarshal(stripTrailingCommas(b), out)
}

// stripTrailingCommas drops a comma that is followed only by whitespace and a
// closing brace or bracket. String literals are skipped, so a comma inside a
// value is never touched.
func stripTrailingCommas(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inString := false
	escaped := false
	for i := range len(src) {
		c := src[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\r' || src[j] == '\n') {
				j++
			}
			if j < len(src) && (src[j] == '}' || src[j] == ']') {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}
