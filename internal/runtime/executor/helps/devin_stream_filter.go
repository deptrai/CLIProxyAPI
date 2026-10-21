package helps

import (
	"strings"
)

// DevinContentFilter filters leaked raw tool markup and suppresses degenerate
// token repetition loops (such as infinite repeating </invoke> and </parameter> tags)
// emitted by upstream reasoning models during long-context tool invocation.
type DevinContentFilter struct {
	consecutiveTagOnlyCount int
	halted                  bool
}

var leakedToolTags = []string{
	"</invoke>",
	"</parameter>",
	"<invoke/>",
	"<parameter/>",
	"</tool_call>",
	"</tool_calls>",
	"<tool_call/>",
	"<tool_calls/>",
	"<｜DSML｜tool_calls>",
	"</｜DSML｜>",
	"<｜tool call end｜>",
	"<｜tool calls end｜>",
}

// FilterChunk inspects a streaming text chunk from upstream Devin response.
// It removes leaked internal tool tags. If consecutive chunks consist purely of
// leaked tags and whitespace (a degenerate repetition loop), it flags shouldHalt = true
// to terminate the stream before consuming excessive tokens or hanging client sessions.
func (f *DevinContentFilter) FilterChunk(chunk string) (clean string, shouldHalt bool) {
	if f.halted {
		return "", true
	}
	if chunk == "" {
		return "", false
	}

	hasTag := false
	cleaned := chunk
	for _, tag := range leakedToolTags {
		if strings.Contains(cleaned, tag) {
			hasTag = true
			cleaned = strings.ReplaceAll(cleaned, tag, "")
		}
	}

	trimmed := strings.TrimSpace(cleaned)

	if hasTag && trimmed == "" {
		// Chunk contained only leaked tool tags and whitespace.
		f.consecutiveTagOnlyCount++
		if f.consecutiveTagOnlyCount >= 4 {
			f.halted = true
			return "", true
		}
		// Suppress emitting empty whitespace from leaked tags.
		return "", false
	}

	if trimmed != "" {
		// Meaningful content received, reset consecutive tag counter.
		f.consecutiveTagOnlyCount = 0
	}

	return cleaned, false
}
