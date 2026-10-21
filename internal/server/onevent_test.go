package server

import (
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/bus"
	"github.com/Intellexie/intellexor/internal/store"
)

// OnEvent must forward every instance event (with its directory) to the
// subscriber — the desktop bridge's replacement for the SSE endpoints.
func TestOnEventForwarding(t *testing.T) {
	t.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()
	s := New(t.TempDir(), "test")

	type received struct {
		dir string
		ev  bus.Event
	}
	ch := make(chan received, 8)
	cancel := s.OnEvent(func(dir string, ev any) {
		be, ok := ev.(bus.Event)
		if !ok {
			t.Errorf("OnEvent payload is %T, want bus.Event", ev)
			return
		}
		ch <- received{dir, be}
	})
	defer cancel()

	in := s.Instance("") // creates the default instance + its mirror
	in.Hub.Publish("test.event", map[string]any{"k": "v"})

	select {
	case r := <-ch:
		if r.ev.Type != "test.event" {
			t.Fatalf("event type = %q, want test.event", r.ev.Type)
		}
		if r.dir != s.DefaultDir {
			t.Fatalf("directory = %q, want %q", r.dir, s.DefaultDir)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for forwarded event")
	}
}
