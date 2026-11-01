package app

import (
	"testing"

	"github.com/Intellexie/intellexor/internal/session"
)

// A prompt whose provider cannot be resolved (model points at a provider that
// does not exist) must not fail silently: the user turn and an assistant
// message carrying the error are persisted and published, so every open
// timeline — the board's orchestrator chat included — renders an error card
// instead of nothing (prompt_async swallows the returned error).
func TestUnresolvableProviderSurfacesError(t *testing.T) {
	cfg := `{
		"model": "nonexistent/m",
		"provider": {}
	}`
	i, s := setupTitleTest(t, cfg)

	_, err := i.Prompt(s, textInput("hello there"))
	if err == nil {
		t.Fatal("expected an error from the prompt, got nil")
	}

	msgs, lerr := i.Mgr.LoadMessages(s)
	if lerr != nil {
		t.Fatal(lerr)
	}
	var userText, asstErr string
	for _, m := range msgs {
		switch info := m.Info.(type) {
		case session.UserMessage:
			for _, p := range m.Parts {
				if p.Type == "text" {
					userText = p.Text
				}
			}
		case session.AssistantMessage:
			if info.Error != nil {
				asstErr, _ = info.Error.Data["message"].(string)
			}
		}
	}
	if userText != "hello there" {
		t.Fatalf("user message not persisted, got %q", userText)
	}
	if asstErr == "" {
		t.Fatal("assistant message carrying the error not persisted")
	}
}

// The failure path must also publish the events the web UI listens to:
// message.updated renders the error card in every open timeline and
// session.error dots the sidebar row.
func TestUnresolvableProviderPublishesEvents(t *testing.T) {
	cfg := `{
		"model": "nonexistent/m",
		"provider": {}
	}`
	i, s := setupTitleTest(t, cfg)

	ch := i.Hub.Subscribe()
	defer i.Hub.Unsubscribe(ch)

	_, _ = i.Prompt(s, textInput("hello there"))

	seen := map[string]bool{}
	for {
		select {
		case ev := <-ch:
			seen[ev.Type] = true
			continue
		default:
		}
		break
	}
	if !seen["message.updated"] {
		t.Fatal("message.updated not published for the error card")
	}
	if !seen["session.error"] {
		t.Fatal("session.error not published")
	}
}
