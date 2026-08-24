package chathub

import (
	"errors"
	"strings"
	"testing"
)

func TestScrubCitationsPreservesSourceWhitespace(t *testing.T) {
	input := "function render() {\n    const html = `<div>  A & B</div>`;[^1^]\n\n\n    return html;\n}\n"
	want := "function render() {\n    const html = `<div>  A & B</div>`;\n\n\n    return html;\n}\n"
	if got := scrubCitations(input); got != want {
		t.Fatalf("source whitespace changed\nwant: %q\n got: %q", want, got)
	}
}

func TestCompactStreamErrorIsSingleLineAndBounded(t *testing.T) {
	got := compactStreamError(errors.New("read tcp\r\n" + strings.Repeat("timeout ", 80)))
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("error was not compacted to one line: %q", got)
	}
	if len(got) > 323 || !strings.Contains(got, "read tcp") {
		t.Fatalf("unexpected compact error length/content: %d %q", len(got), got)
	}
}
