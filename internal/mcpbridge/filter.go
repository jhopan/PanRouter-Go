package mcpbridge

import (
	json "encoding/json/v2"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Frame filtering: the bridge compacts MCP tool results before they reach the
// model.
//
// A Browser MCP snapshot is an accessibility tree that runs to megabytes:
// thousands of "generic" wrapper nodes, the same repeated sibling structure
// hundreds of times, and text blocks far past what is useful to a model. The
// child sends the whole thing; upstream's bridge rewrites only the text blocks
// of a JSON-RPC result, and every other frame is passed through byte-for-byte.
//
// The numbers are upstream's (src/lib/mcp/stdioSseBridge.js): filter only text
// past 2000 characters, keep the first 10 and last 5 of a repeated group of 30
// or more siblings, and hard-truncate at 50000.

const (
	maxTextChars      = 50000
	collapseThreshold = 30
	collapseKeepHead  = 10
	collapseKeepTail  = 5
	// minFilterRunes is the size below which a text block is left alone.
	minFilterRunes = 2000
	// truncationReserve is the room kept for the truncation notice itself.
	truncationReserve = 300
)

var (
	// A wrapper node with no content of its own. (?m) is upstream's /gm: ^ and $
	// must match per line, not once per text block.
	genericNoiseLine = regexp.MustCompile(`(?m)^[\t\n\f\r ]*-[\t\n\f\r ]*generic:?[\t\n\f\r ]*$`)
	// A text node whose value is empty.
	emptyTextLine = regexp.MustCompile(`(?m)^[\t\n\f\r ]*-[\t\n\f\r ]*text:[\t\n\f\r ]*""[\t\n\f\r ]*$`)
	// A tree line: indent, dash, role word. The indent and role identify a
	// sibling group; the rest of the line is its detail.
	siblingLine = regexp.MustCompile(`^([\t\n\f\r ]*)-[\t\n\f\r ]*([a-zA-Z]+)\b`)
)

// deterministicJSON keeps a rewritten frame's key order stable across
// broadcasts; json/v2 otherwise marshals maps in a random order.
var deterministicJSON = json.Deterministic(true)

// sibling is the parsed head of a tree line.
type sibling struct {
	indent string
	role   string
}

func matchSibling(line string) (sibling, bool) {
	m := siblingLine.FindStringSubmatch(line)
	if m == nil {
		return sibling{}, false
	}
	return sibling{indent: m[1], role: m[2]}, true
}

// smartFilterText drops noise lines, collapses repeated sibling groups and
// hard-truncates. Text below the threshold is returned unchanged.
func smartFilterText(text string) string {
	if utf8.RuneCountInString(text) < minFilterRunes {
		return text
	}
	out := genericNoiseLine.ReplaceAllString(text, "")
	out = emptyTextLine.ReplaceAllString(out, "")
	out = collapseRepeated(out)

	runes := []rune(out)
	if len(runes) <= maxTextChars {
		return out
	}
	headRunes := maxTextChars - truncationReserve
	head := string(runes[:headRunes])
	omitted := utf8.RuneCountInString(out) - utf8.RuneCountInString(head)
	return fmt.Sprintf("%s\n\n... [truncated %d chars by panrouter bridge. "+
		"Page is large; ask user to scroll/navigate to a specific section, "+
		"or click an element with the refs shown above]", head, omitted)
}

// collapseRepeated groups consecutive lines sharing a leading indent and role
// word, together with their deeper continuation lines, and folds a group of
// collapseThreshold or more down to its head and tail.
func collapseRepeated(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))

	for i := 0; i < len(lines); {
		head, ok := matchSibling(lines[i])
		if !ok {
			out = append(out, lines[i])
			i++
			continue
		}

		j := i
		for j < len(lines) {
			line := lines[j]
			if s, isSibling := matchSibling(line); isSibling && s == head {
				j++
				continue
			}
			// A deeper line belongs to the preceding sibling: its content.
			if strings.HasPrefix(line, head.indent+" ") || strings.HasPrefix(line, head.indent+"\t") {
				j++
				continue
			}
			break
		}

		groupLen := j - i
		if groupLen < collapseThreshold {
			out = append(out, lines[i:j]...)
			i = j
			continue
		}

		headEnd := findNthSiblingEnd(lines, i, head, collapseKeepHead)
		tailStart := findLastNSiblingStart(lines, j, head, collapseKeepTail)
		out = append(out, lines[i:headEnd]...)
		out = append(out, fmt.Sprintf(`%s... [%d similar "%s" items omitted by panrouter bridge]`,
			head.indent, groupLen-collapseKeepHead-collapseKeepTail, head.role))
		out = append(out, lines[tailStart:j]...)
		i = j
	}
	return strings.Join(out, "\n")
}

// findNthSiblingEnd returns the index just past the n-th sibling of the group
// that starts at start, which is where the kept head ends.
func findNthSiblingEnd(lines []string, start int, head sibling, n int) int {
	count := 0
	for k := start; k < len(lines); k++ {
		if s, ok := matchSibling(lines[k]); ok && s == head {
			count++
			if count > n {
				return k
			}
		}
	}
	return len(lines)
}

// findLastNSiblingStart returns the index of the n-th sibling counted from the
// end of the group that ends at end, which is where the kept tail begins.
func findLastNSiblingStart(lines []string, end int, head sibling, n int) int {
	positions := make([]int, 0, collapseThreshold)
	for k := range end {
		if s, ok := matchSibling(lines[k]); ok && s == head {
			positions = append(positions, k)
		}
	}
	if len(positions) > n {
		return positions[len(positions)-n]
	}
	return end
}

// filterFrame rewrites the text blocks of a JSON-RPC result, and returns any
// other frame untouched. A frame that does not parse as JSON is passed through
// as well: the child may emit something this bridge does not understand, and
// dropping it would silently lose a protocol message.
func filterFrame(line string) string {
	var msg map[string]any
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return line
	}
	result, ok := msg["result"].(map[string]any)
	if !ok {
		return line
	}
	content, ok := result["content"].([]any)
	if !ok {
		return line
	}

	mutated := false
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok || block["type"] != "text" {
			continue
		}
		text, ok := block["text"].(string)
		if !ok {
			continue
		}
		if filtered := smartFilterText(text); filtered != text {
			block["text"] = filtered
			mutated = true
		}
	}
	if !mutated {
		return line
	}
	out, err := json.Marshal(msg, deterministicJSON)
	if err != nil {
		return line
	}
	return string(out)
}
