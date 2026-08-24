package chathub

import "strings"

const implicitAnswerTrackID = "__implicit_visible_answer__"

// answerTrack keeps the authoritative snapshot and the append-only bytes that
// have already been exposed to a streaming caller for one ChatHub message.
// ChatHub can interleave MemoryUpdate/progress/plugin messages with the actual
// answer, so answer text must never be assembled in one process-global buffer.
type answerTrack struct {
	id            string
	authoritative string
	delivered     string
	final         bool
}

// answerStream tracks the currently selected ChatHub cursor and all visible
// answer messages seen in one invocation. A cursor may target an internal card;
// blockedCursor prevents its writeAtCursor bytes from falling back to the
// implicit answer track.
type answerStream struct {
	tracks        map[string]*answerTrack
	currentID     string
	lastVisibleID string
	finalID       string
	blockedCursor bool
}

func newAnswerStream() *answerStream {
	return &answerStream{tracks: make(map[string]*answerTrack)}
}

func (a *answerStream) track(id string) *answerTrack {
	if strings.TrimSpace(id) == "" {
		id = implicitAnswerTrackID
	}
	t := a.tracks[id]
	if t == nil {
		t = &answerTrack{id: id}
		a.tracks[id] = t
	}
	return t
}

// observeSnapshot records a bot answer snapshot and returns only the unseen
// append-only suffix that is safe to emit. A non-prefix snapshot is an upstream
// rewrite: it becomes authoritative for Result.Text/FullText, but cannot be
// spliced into bytes already delivered to an SSE client.
func (a *answerStream) observeSnapshot(id, text string, final bool) string {
	if text == "" {
		return ""
	}
	t := a.track(id)
	t.authoritative = text
	if final {
		t.final = true
		a.finalID = t.id
	}
	a.currentID = t.id
	a.lastVisibleID = t.id
	a.blockedCursor = false

	if t.delivered == "" {
		t.delivered = text
		return text
	}
	if strings.HasPrefix(text, t.delivered) {
		tail := text[len(t.delivered):]
		t.delivered = text
		return tail
	}
	return ""
}

// selectCursor binds subsequent writeAtCursor updates to the message selected
// by ChatHub's JSON-path cursor. Unknown cursor targets are internal/non-visible
// cards and are intentionally blocked rather than merged into an answer.
func (a *answerStream) selectCursor(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	if _, ok := a.tracks[id]; ok {
		a.currentID = id
		a.blockedCursor = false
		return
	}
	a.currentID = ""
	a.blockedCursor = true
}

// appendAtCursor applies a pure cursor delta to the selected visible answer.
// Some upstream variants omit an explicit cursor for ordinary single-answer
// streams; those use one implicit track. Once an explicit cursor selected an
// internal card, fallback is disabled until a visible answer is selected.
func (a *answerStream) appendAtCursor(delta string) (string, bool) {
	if delta == "" || a.blockedCursor {
		return "", false
	}
	id := a.currentID
	if id == "" {
		id = implicitAnswerTrackID
	}
	t := a.track(id)
	t.delivered += delta
	// Cursor deltas advance the best available snapshot until a later full
	// snapshot or the type-2 result supplies a stronger authority.
	if t.authoritative == "" || strings.HasPrefix(t.delivered, t.authoritative) {
		t.authoritative = t.delivered
	}
	a.currentID = t.id
	a.lastVisibleID = t.id
	return delta, true
}

func (a *answerStream) bestText() string {
	for _, id := range []string{a.finalID, a.lastVisibleID, a.currentID} {
		if t := a.tracks[id]; t != nil {
			if strings.TrimSpace(t.authoritative) != "" {
				return t.authoritative
			}
			if strings.TrimSpace(t.delivered) != "" {
				return t.delivered
			}
		}
	}
	return ""
}

// cursorMessageID extracts the message id from paths used by the live client,
// for example $['9ab0...'].adaptiveCards[0].body[0].text.
func cursorMessageID(arg map[string]any) string {
	cursor, _ := arg["cursor"].(map[string]any)
	path, _ := cursor["j"].(string)
	for _, quote := range []byte{'\'', '"'} {
		prefix := "$[" + string(quote)
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := path[len(prefix):]
		if end := strings.Index(rest, string(quote)+"]"); end >= 0 {
			return rest[:end]
		}
	}
	return ""
}

func visibleAnswerMessage(m map[string]any) bool {
	author, _ := m["author"].(string)
	if !strings.EqualFold(strings.TrimSpace(author), "bot") {
		return false
	}
	messageType := strings.ToLower(strings.TrimSpace(stringValue(m["messageType"])))
	contentType := strings.ToLower(strings.TrimSpace(stringValue(m["contentType"])))
	if messageType != "" && messageType != "chat" && messageType != "answer" {
		return false
	}
	switch contentType {
	case "searchresults", "code", "toolcall", "progress":
		return false
	}
	// Plugin lifecycle/invocation records are protocol metadata even when the
	// server copies a human-readable description into text.
	if m["pluginInfo"] != nil || strings.TrimSpace(stringValue(m["invocation"])) != "" {
		return false
	}
	return strings.TrimSpace(stringValue(m["text"])) != ""
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}
