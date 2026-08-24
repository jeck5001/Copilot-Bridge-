package web

import "strings"

var upstreamThrottlePlaceholders = []string{
	"we're temporarily unable to respond to this volume of requests. please try again later.",
	"we are temporarily unable to respond to this volume of requests. please try again later.",
}

func normalizedPlaceholderText(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(text)), " "))
}

// isUpstreamThrottlePlaceholderText recognizes the short natural-language
// overload response Microsoft sometimes emits inside an otherwise successful
// completion frame.  Keep this deliberately narrow: arbitrary model prose
// mentioning throttling must not rotate an account.
func isUpstreamThrottlePlaceholderText(text string) bool {
	normalized := normalizedPlaceholderText(text)
	if normalized == "" || len(normalized) > 512 {
		return false
	}
	for _, placeholder := range upstreamThrottlePlaceholders {
		if normalized == placeholder {
			return true
		}
	}
	return false
}

type streamPlaceholderGuard struct {
	pending  strings.Builder
	released bool
}

// Feed delays only a prefix that could still become a known overload message.
// Normal content is released as soon as it diverges, preserving true streaming
// latency while preventing an overload sentence from becoming visible output.
func (g *streamPlaceholderGuard) Feed(chunk string) (string, bool) {
	if chunk == "" {
		return "", false
	}
	if g.released {
		return chunk, false
	}
	g.pending.WriteString(chunk)
	normalized := normalizedPlaceholderText(g.pending.String())
	for _, placeholder := range upstreamThrottlePlaceholders {
		if strings.HasPrefix(placeholder, normalized) || normalized == placeholder {
			return "", true
		}
	}
	g.released = true
	text := g.pending.String()
	g.pending.Reset()
	return text, false
}

func (g *streamPlaceholderGuard) Finish(placeholder bool) string {
	if g.released {
		return ""
	}
	text := g.pending.String()
	g.pending.Reset()
	if placeholder {
		return ""
	}
	g.released = true
	return text
}

func (g *streamPlaceholderGuard) Reset() {
	g.pending.Reset()
	g.released = false
}
