package proxy

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestClampResponseNil(t *testing.T) {
	if got := clampResponse(nil, 10); got != nil {
		t.Errorf("clampResponse(nil, 10) = %v, want nil", got)
	}
}

func TestClampResponseUnderBudgetUnchanged(t *testing.T) {
	result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hello"}}}

	got := clampResponse(result, 100)
	if len(got.Content) != 1 {
		t.Fatalf("Content = %v, want it unchanged (1 block)", got.Content)
	}
	if got.Content[0].(*mcp.TextContent).Text != "hello" {
		t.Errorf("Text = %q, want %q", got.Content[0].(*mcp.TextContent).Text, "hello")
	}
}

func TestClampResponseZeroOrNegativeBudgetIsNoop(t *testing.T) {
	result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", 1000)}}}

	for _, budget := range []int64{0, -1} {
		got := clampResponse(result, budget)
		if got != result {
			t.Errorf("clampResponse(result, %d) modified the result, want it returned unchanged (no budget configured)", budget)
		}
	}
}

// TestClampResponseTruncatesOversizedText proves the FEATURE-018 acceptance
// criterion: text over max_response_bytes is truncated, the
// "[TRUNCATED: ...]" notice is appended, and isError stays false.
func TestClampResponseTruncatesOversizedText(t *testing.T) {
	result := &mcp.CallToolResult{
		IsError: false,
		Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("a", 100)}},
	}

	got := clampResponse(result, 10)

	if got.IsError {
		t.Error("IsError = true, want false: a truncated response is still a successful call")
	}
	if len(got.Content) != 2 {
		t.Fatalf("Content = %v, want 2 blocks (truncated text + notice)", got.Content)
	}

	truncated := got.Content[0].(*mcp.TextContent).Text
	if len(truncated) != 10 {
		t.Errorf("truncated text length = %d, want 10", len(truncated))
	}
	if truncated != strings.Repeat("a", 10) {
		t.Errorf("truncated text = %q, want the first 10 bytes of the original", truncated)
	}

	notice := got.Content[1].(*mcp.TextContent).Text
	if notice != truncationNotice {
		t.Errorf("notice = %q, want %q", notice, truncationNotice)
	}
}

// TestClampResponseBudgetSpansMultipleBlocks proves the budget is a total
// across every text block, not applied per block: the first block fits
// whole, and the second is dropped entirely once the budget is exhausted.
func TestClampResponseBudgetSpansMultipleBlocks(t *testing.T) {
	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: "12345"},
			&mcp.TextContent{Text: "abcdefghij"},
		},
	}

	got := clampResponse(result, 5)

	if len(got.Content) != 2 {
		t.Fatalf("Content = %v, want 2 blocks (first block kept whole + notice)", got.Content)
	}
	if text := got.Content[0].(*mcp.TextContent).Text; text != "12345" {
		t.Errorf("first block = %q, want %q (kept whole, it fit the budget)", text, "12345")
	}
	if notice := got.Content[1].(*mcp.TextContent).Text; notice != truncationNotice {
		t.Errorf("second block = %q, want the truncation notice (the second text block was dropped entirely)", notice)
	}
}

// TestClampResponseLeavesNonTextContentUntouched proves only text content
// counts toward the budget and gets truncated; other content types (here,
// an image) pass through unmodified and don't themselves trigger clamping.
func TestClampResponseLeavesNonTextContentUntouched(t *testing.T) {
	image := &mcp.ImageContent{Data: []byte{0xFF, 0xD8, 0xFF}, MIMEType: "image/jpeg"}
	result := &mcp.CallToolResult{Content: []mcp.Content{image}}

	got := clampResponse(result, 1)

	if len(got.Content) != 1 {
		t.Fatalf("Content = %v, want the image left alone (no text content to clamp)", got.Content)
	}
	if got.Content[0] != mcp.Content(image) {
		t.Error("the image content block was replaced, want it untouched")
	}
}

// TestClampResponseTruncationIsValidUTF8 proves the cut point never splits
// a multi-byte rune, even when the byte budget lands mid-rune.
func TestClampResponseTruncationIsValidUTF8(t *testing.T) {
	text := strings.Repeat("é", 10) // 'é' is 2 bytes in UTF-8
	result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}

	// Budget lands in the middle of a 2-byte rune.
	got := clampResponse(result, 5)

	truncated := got.Content[0].(*mcp.TextContent).Text
	if !utf8.ValidString(truncated) {
		t.Fatalf("truncated text %q is not valid UTF-8", truncated)
	}
	if len(truncated) > 5 {
		t.Errorf("truncated text is %d bytes, want at most 5", len(truncated))
	}
}
