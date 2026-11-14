package mcpbridge

import (
	json "encoding/json/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSmartFilterTextLeavesShortTextAlone(t *testing.T) {
	text := "  - generic\n  - text: \"\"\n" + strings.Repeat("a", 100)

	if got := smartFilterText(text); got != text {
		t.Fatalf("text under the threshold was rewritten:\n%q", got)
	}
}

func TestSmartFilterTextDropsNoiseLines(t *testing.T) {
	text := strings.Repeat("  - button \"Go\"\n", 150) +
		"  - generic\n" +
		"  - text: \"\"\n" +
		"  - heading \"Title\"\n"

	out := smartFilterText(text)
	if strings.Contains(out, "- generic\n") {
		t.Errorf("a bare generic node survived:\n%s", out)
	}
	if strings.Contains(out, `- text: ""`) {
		t.Errorf("an empty text node survived:\n%s", out)
	}
	if !strings.Contains(out, `- heading "Title"`) {
		t.Errorf("a content node was dropped:\n%s", out)
	}
}

func TestSmartFilterTextCollapsesRepeatedSiblings(t *testing.T) {
	var b strings.Builder
	for range 40 {
		b.WriteString("  - button \"Go\"\n")
	}
	b.WriteString(strings.Repeat("x", 2100))
	text := b.String()

	out := smartFilterText(text)

	wantNotice := `  ... [25 similar "button" items omitted by panrouter bridge]`
	if !strings.Contains(out, wantNotice) {
		t.Fatalf("missing collapse notice %q in:\n%s", wantNotice, out)
	}
	if got := strings.Count(out, "- button"); got != collapseKeepHead+collapseKeepTail {
		t.Errorf("kept %d sibling lines, want %d (head %d + tail %d)",
			got, collapseKeepHead+collapseKeepTail, collapseKeepHead, collapseKeepTail)
	}
	if !strings.Contains(out, strings.Repeat("x", 2100)) {
		t.Error("text after the collapsed group was lost")
	}
}

func TestSmartFilterTextKeepsGroupsUnderThreshold(t *testing.T) {
	var b strings.Builder
	for range collapseThreshold - 1 {
		b.WriteString("  - button \"Go\"\n")
	}
	b.WriteString(strings.Repeat("x", 2100))

	out := smartFilterText(b.String())
	if strings.Contains(out, "items omitted") {
		t.Errorf("a group below the threshold was collapsed:\n%s", out)
	}
	if got := strings.Count(out, "- button"); got != collapseThreshold-1 {
		t.Errorf("kept %d sibling lines, want %d", got, collapseThreshold-1)
	}
}

func TestSmartFilterTextTruncates(t *testing.T) {
	text := strings.Repeat("a", 60000)

	out := smartFilterText(text)

	if !strings.Contains(out, "... [truncated ") {
		t.Fatalf("no truncation notice:\n%.200s", out)
	}
	if runes := utf8.RuneCountInString(out); runes > maxTextChars+200 {
		t.Errorf("truncated text is %d runes, want it bounded by %d", runes, maxTextChars)
	}
	if !strings.Contains(out, "10300 chars") {
		t.Errorf("truncation did not count the omitted characters:\n%.300s", out)
	}
}

func TestFilterFramePassesThroughUnparsable(t *testing.T) {
	line := "not json at all"

	if got := filterFrame(line); got != line {
		t.Errorf("filterFrame rewrote an unparsable frame:\n%q", got)
	}
}

func TestFilterFramePassesThroughWithoutResultContent(t *testing.T) {
	tests := map[string]string{
		"no result":        `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"no such tool"}}`,
		"result is scalar": `{"jsonrpc":"2.0","id":1,"result":7}`,
		"content missing":  `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`,
	}
	for name, line := range tests {
		t.Run(name, func(t *testing.T) {
			if got := filterFrame(line); got != line {
				t.Errorf("filterFrame rewrote a frame with nothing to filter:\n%q", got)
			}
		})
	}
}

func TestFilterFrameRewritesOnlyTextBlocks(t *testing.T) {
	big := strings.Repeat("x", 60000)
	line := `{"jsonrpc":"2.0","id":1,"result":{"content":[` +
		`{"type":"text","text":"` + big + `"},` +
		`{"type":"image","data":"` + big + `"},` +
		`{"type":"text","text":"short answer"}]}}`

	out := filterFrame(line)
	if out == line {
		t.Fatal("filterFrame left an oversized text block alone")
	}

	var msg struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
				Data string `json:"data"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		t.Fatalf("rewritten frame is not JSON: %v\n%s", err, out)
	}
	if len(msg.Result.Content) != 3 {
		t.Fatalf("rewritten frame has %d content blocks, want 3", len(msg.Result.Content))
	}
	if !strings.Contains(msg.Result.Content[0].Text, "... [truncated ") {
		t.Error("the oversized text block was not truncated")
	}
	if len(msg.Result.Content[1].Data) != len(big) {
		t.Error("the image block was rewritten; only text blocks may change")
	}
	if msg.Result.Content[2].Text != "short answer" {
		t.Errorf("short text block = %q, want it untouched", msg.Result.Content[2].Text)
	}
}

func TestMatchSiblingReadsIndentAndRole(t *testing.T) {
	tests := []struct {
		line string
		want sibling
		ok   bool
	}{
		{`  - button "Go"`, sibling{indent: "  ", role: "button"}, true},
		{"\t- text: \"hi\"", sibling{indent: "\t", role: "text"}, true},
		{"- heading Title", sibling{indent: "", role: "heading"}, true},
		{"plain line", sibling{}, false},
		{"  - 1button", sibling{}, false},
	}
	for _, tt := range tests {
		got, ok := matchSibling(tt.line)
		if ok != tt.ok || got != tt.want {
			t.Errorf("matchSibling(%q) = %+v,%v want %+v,%v", tt.line, got, ok, tt.want, tt.ok)
		}
	}
}
