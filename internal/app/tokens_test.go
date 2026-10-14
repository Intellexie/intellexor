package app

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"intellexor/internal/bus"
	"intellexor/internal/session"
	"intellexor/internal/store"
	"sync"
)

// mock OpenAI-compatible SSE endpoint that reports usage on the final chunk
func mockLLM(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		chunks := []string{
			`{"choices":[{"delta":{"role":"assistant","content":"Hello!"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":200},"completion_tokens_details":{"reasoning_tokens":10}}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			f.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return fmt.Sprintf("http://%s", ln.Addr())
}

func TestTokensAccumulateAndSessionEvent(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	base := mockLLM(t)

	work := t.TempDir()
	cfg := fmt.Sprintf(`{
	  "model": "mock/test-model",
	  "provider": {
	    "mock": {
	      "npm": "@ai-sdk/openai-compatible",
	      "options": {"baseURL": "%s/v1", "apiKey": "test"},
	      "models": {
	        "test-model": {"name": "Test Model", "tool_call": true, "limit": {"context": 100000, "output": 8192}}
	      }
	    }
	  }
	}`, base)
	if err := os.WriteFile(filepath.Join(work, "intellexor.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	i := New(work)

	// capture session.updated payloads (slice, not a channel: the mirror
	// keeps firing for the second prompt below)
	var mu sync.Mutex
	var payloads []float64
	i.Hub.AddMirror(func(ev bus.Event) {
		if ev.Type != "session.updated" {
			return
		}
		info, _ := ev.Properties["info"].(session.Session)
		total := 0.0
		if info.Tokens.Total != nil {
			total = *info.Tokens.Total
		}
		mu.Lock()
		payloads = append(payloads, total)
		mu.Unlock()
	})

	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "token test"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(map[string]any{"type": "text", "text": "hi"})
	if _, err := i.Prompt(s, PromptInput{Model: &ModelSel{ProviderID: "mock", ModelID: "test-model"}, Parts: []json.RawMessage{raw}}); err != nil {
		t.Fatal(err)
	}

	// 1. session tokens accumulated on disk?
	got, err := i.Mgr.Find(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("session tokens: input=%v output=%v reasoning=%v cache=%+v",
		got.Tokens.Input, got.Tokens.Output, got.Tokens.Reasoning, got.Tokens.Cache)
	if got.Tokens.Total != nil {
		t.Logf("session tokens total=%v", *got.Tokens.Total)
	} else {
		t.Logf("session tokens total=nil")
	}
	if got.Tokens.Input == 0 && got.Tokens.Output == 0 {
		t.Fatalf("expected accumulated tokens on session, got %+v", got.Tokens)
	}

	// 2. does any session.updated event carry the accumulated tokens?
	//    (the web client rebuilds the context circle from this event's payload)
	if len(payloads) == 0 || payloads[len(payloads)-1] == 0 {
		t.Fatalf("no session.updated event carried tokens (%v); context circle would stay at 0%%", payloads)
	}
	t.Logf("session.updated events: tokens.total=%v", payloads)

	// 3. context tracks the LAST step's usage, not the cumulative sum: a
	//    second prompt must leave Context at one step's worth of tokens while
	//    Tokens.Total keeps accumulating. The context circle divides Context
	//    by the model's context window; using the cumulative sum made it show
	//    ~100% on tiny conversations.
	raw2, _ := json.Marshal(map[string]any{"type": "text", "text": "again"})
	if _, err := i.Prompt(s, PromptInput{Model: &ModelSel{ProviderID: "mock", ModelID: "test-model"}, Parts: []json.RawMessage{raw2}}); err != nil {
		t.Fatal(err)
	}
	got, err = i.Mgr.Find(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after 2 prompts: tokens.total=%v context=%v", *got.Tokens.Total, got.Context)
	if got.Context != 1060 {
		t.Fatalf("expected context=1060 (last step only), got %v", got.Context)
	}
	if *got.Tokens.Total != 2120 {
		t.Fatalf("expected cumulative tokens.total=2120, got %v", *got.Tokens.Total)
	}
}
