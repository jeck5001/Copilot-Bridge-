package web

import (
	"errors"
	"strings"
	"testing"
)

func TestTerminalOnlyUpstreamTextNeedsStreamingBackfill(t *testing.T) {
	var delivered strings.Builder
	emitted := ""
	if err := backfillTerminalStreamText("TERMINAL_ONLY_TEXT", &delivered, false, func(text string) error {
		emitted += text
		delivered.WriteString(text)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if delivered.String() != "TERMINAL_ONLY_TEXT" {
		t.Fatalf("terminal-only completion text was lost: %q", delivered.String())
	}
	if emitted != "TERMINAL_ONLY_TEXT" {
		t.Fatalf("terminal-only text was not emitted: %q", emitted)
	}
	if err := backfillTerminalStreamText("DUPLICATE", &delivered, false, func(string) error {
		return errors.New("must not emit twice")
	}); err != nil {
		t.Fatalf("existing delta text triggered duplicate backfill: %v", err)
	}

	var guarded strings.Builder
	if err := backfillTerminalStreamText("GUARDED_TEXT", &guarded, true, func(string) error {
		return errors.New("guarded text must remain buffered")
	}); err != nil {
		t.Fatal(err)
	}
	if guarded.String() != "GUARDED_TEXT" {
		t.Fatalf("guarded terminal text was not buffered: %q", guarded.String())
	}
}
