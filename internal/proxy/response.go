package proxy

import (
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// truncationNotice is spec.md §6's exact wording for a clamped response.
const truncationNotice = "[TRUNCATED: response exceeded max_response_bytes]"

// clampResponse enforces maxBytes on result's text content (FEATURE-018,
// spec.md §5.3's "Response Clamping & Truncation"). Only *mcp.TextContent
// blocks count toward the budget and are what get truncated — that's what
// actually floods an LLM's context window; StructuredContent and non-text
// content (images, audio, embedded resources) are left exactly as the
// backend returned them. isError is never set by clamping: an
// oversized-but-otherwise-successful response is still a successful call,
// per spec.md §6's "Output Truncated" row.
func clampResponse(result *mcp.CallToolResult, maxBytes int64) *mcp.CallToolResult {
	if result == nil || maxBytes <= 0 {
		return result
	}

	var total int64
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			total += int64(len(tc.Text))
		}
	}
	if total <= maxBytes {
		return result
	}

	clamped := make([]mcp.Content, 0, len(result.Content)+1)
	remaining := maxBytes
	for _, c := range result.Content {
		tc, ok := c.(*mcp.TextContent)
		if !ok {
			clamped = append(clamped, c)
			continue
		}
		switch {
		case remaining <= 0:
			// Budget already exhausted by an earlier block: drop this one
			// entirely rather than appending an empty text block.
		case int64(len(tc.Text)) <= remaining:
			clamped = append(clamped, tc)
			remaining -= int64(len(tc.Text))
		default:
			clamped = append(clamped, &mcp.TextContent{
				Meta:        tc.Meta,
				Annotations: tc.Annotations,
				Text:        truncateUTF8(tc.Text, int(remaining)),
			})
			remaining = 0
		}
	}
	clamped = append(clamped, &mcp.TextContent{Text: truncationNotice})

	out := *result
	out.Content = clamped
	out.IsError = false
	return &out
}

// truncateUTF8 returns the longest prefix of s that is at most maxBytes
// bytes and valid UTF-8, so a multi-byte rune straddling the cut point is
// dropped whole rather than left as an invalid trailing fragment.
func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	b := s[:maxBytes]
	for len(b) > 0 && !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}
	return b
}
