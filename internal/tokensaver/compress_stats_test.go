package tokensaver

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"strings"
	"testing"
)

// statsTool is one tool result as the request carries it: the command the tool
// ran (which is what picks a filter) and the output it produced.
type statsTool struct {
	command string
	content string
}

// statsGitDiff is tool output the git-diff filter claims: a diff header, more
// than GitDiffMaxLines hunk lines, and the index/---/+++ metadata it drops.
func statsGitDiff() statsTool {
	var b strings.Builder
	b.WriteString("diff --git a/alpha.go b/alpha.go\n")
	b.WriteString("index 1a2b3c4..5d6e7f8 100644\n")
	b.WriteString("--- a/alpha.go\n")
	b.WriteString("+++ b/alpha.go\n")
	for i := range GitDiffMaxLines + 20 {
		fmt.Fprintf(&b, "@@ -%d,1 +%d,1 @@ context line %d\n", i, i, i)
	}
	return statsTool{command: "git diff", content: b.String()}
}

// statsGitStatus is tool output the git-status filter claims: the branch line,
// a heading, and the "(use ...)" tips it drops.
func statsGitStatus() statsTool {
	var b strings.Builder
	b.WriteString("On branch main\n")
	b.WriteString("Changes not staged for commit:\n")
	for i := range 40 {
		b.WriteString("  (use \"git add <file>...\" to update what will be committed)\n")
		fmt.Fprintf(&b, "\tmodified: internal/pkg/file%02d.go\n", i)
	}
	return statsTool{command: "git status", content: b.String()}
}

// statsUnclaimed is tool output no command and no filter claims: unique lines,
// no diff header, no git vocabulary, no severity marker. It is the body that has
// to be weighed on both sides of the percentage.
func statsUnclaimed() statsTool {
	var b strings.Builder
	for i := range 12 {
		fmt.Fprintf(&b, "note %03d: lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor\n", i)
	}
	return statsTool{content: b.String()}
}

// statsBody builds an OpenAI Chat Completions body whose tool results are the
// given tools, in order.
func statsBody(t *testing.T, tools ...statsTool) []byte {
	t.Helper()
	msgs := make([]any, 0, len(tools)*2)
	for i, tool := range tools {
		msgs = append(msgs,
			map[string]any{
				"role": "assistant",
				"tool_calls": []any{
					map[string]any{
						"id":   fmt.Sprintf("call_%d", i),
						"type": "function",
						"function": map[string]any{
							"name":      "bash",
							"arguments": fmt.Sprintf(`{"command":%q}`, tool.command),
						},
					},
				},
			},
			map[string]any{
				"role":         "tool",
				"tool_call_id": fmt.Sprintf("call_%d", i),
				"content":      tool.content,
			})
	}
	body, err := json.Marshal(map[string]any{"model": "m", "messages": msgs})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return body
}

func TestCompressMessagesWithConfigStats_BooksEveryConsideredBody(t *testing.T) {
	diff, status, unclaimed := statsGitDiff(), statsGitStatus(), statsUnclaimed()
	body := statsBody(t, diff, status, unclaimed)

	out, did, stats := CompressMessagesWithConfigStats(body, DefaultRTKConfig())
	if !did {
		t.Fatal("expected the diff and status bodies to be compressed")
	}
	wantBefore := len(diff.content) + len(status.content) + len(unclaimed.content)
	if stats.BytesBefore != wantBefore {
		t.Errorf("BytesBefore = %d, want %d (every considered body, claimed or not)", stats.BytesBefore, wantBefore)
	}
	if stats.BytesAfter >= stats.BytesBefore {
		t.Errorf("BytesAfter = %d, want less than %d", stats.BytesAfter, stats.BytesBefore)
	}
	if stats.Hits != 2 {
		t.Errorf("Hits = %d, want 2 (the diff and the status, not the unclaimed body)", stats.Hits)
	}
	if got := strings.Join(stats.Filters, ","); got != "git-diff,git-status" {
		t.Errorf("Filters = [%s], want [git-diff,git-status] in first-seen order", got)
	}
	if len(out) >= len(body) {
		t.Errorf("compressed body is not smaller: %d >= %d", len(out), len(body))
	}
}

// The stats variant must not change what the pass does: it is the same
// compression with an accountant attached.
func TestCompressMessagesWithConfigStats_MatchesThePlainPass(t *testing.T) {
	body := statsBody(t, statsGitDiff(), statsUnclaimed())

	plain, plainDid := CompressMessagesWithConfig(body, DefaultRTKConfig())
	withStats, statsDid, _ := CompressMessagesWithConfigStats(body, DefaultRTKConfig())

	if plainDid != statsDid {
		t.Errorf("changed flag differs: plain=%v stats=%v", plainDid, statsDid)
	}
	if !bytes.Equal(plain, withStats) {
		t.Error("the stats variant returned a different body than CompressMessagesWithConfig")
	}
}

// A repeated body from the same filter is one filter and two hits.
func TestCompressMessagesWithConfigStats_CountsHitsPerBodyNotPerFilter(t *testing.T) {
	body := statsBody(t, statsGitDiff(), statsGitDiff())

	_, did, stats := CompressMessagesWithConfigStats(body, DefaultRTKConfig())
	if !did {
		t.Fatal("expected both diff bodies to be compressed")
	}
	if stats.Hits != 2 {
		t.Errorf("Hits = %d, want 2", stats.Hits)
	}
	if got := strings.Join(stats.Filters, ","); got != "git-diff" {
		t.Errorf("Filters = [%s], want [git-diff] once", got)
	}
}

// A body no filter claimed is still weighed on both sides, and reports no hit —
// which is what makes the console line omit itself rather than print a zero.
func TestCompressMessagesWithConfigStats_UnclaimedBody(t *testing.T) {
	unclaimed := statsUnclaimed()
	body := statsBody(t, unclaimed)

	_, _, stats := CompressMessagesWithConfigStats(body, DefaultRTKConfig())
	if stats.Hits != 0 || len(stats.Filters) != 0 {
		t.Errorf("stats = %+v, want no hits and no filters", stats)
	}
	if stats.BytesBefore != len(unclaimed.content) {
		t.Errorf("BytesBefore = %d, want %d (the body that was weighed)", stats.BytesBefore, len(unclaimed.content))
	}
	if stats.BytesAfter != stats.BytesBefore {
		t.Errorf("BytesAfter = %d, want %d (nothing was saved)", stats.BytesAfter, stats.BytesBefore)
	}
}

// A body that is not an object, or carries no tool results at all, must report
// nothing rather than a bogus total.
func TestCompressMessagesWithConfigStats_BodiesItCannotRead(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"not json", []byte("not json at all")},
		{"json array", []byte(`[]`)},
		{"no items", []byte(`{"model":"m"}`)},
		{"messages not an array", []byte(`{"messages":{"role":"user"}}`)},
		{"small tool result", []byte(`{"messages":[{"role":"tool","tool_call_id":"c","content":"tiny"}]}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, did, stats := CompressMessagesWithConfigStats(tt.body, DefaultRTKConfig())
			if did {
				t.Error("did = true, want false")
			}
			if stats.Hits != 0 || len(stats.Filters) != 0 {
				t.Errorf("stats = %+v, want no hits and no filters", stats)
			}
			if !bytes.Equal(out, tt.body) {
				t.Error("body was changed")
			}
		})
	}
}