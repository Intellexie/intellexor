package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Intellexie/intellexor/internal/provider"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// mockLLMUsage is an OpenAI-compatible SSE mock that records every request
// body and reports prompt_tokens proportional to the request's content size,
// so overflow thresholds can be tested against real request growth.
func mockLLMUsage(t *testing.T) (base string, bodies *[]string, mu *sync.Mutex) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mu = &sync.Mutex{}
	recorded := &[]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*recorded = append(*recorded, string(b))
		mu.Unlock()
		var req struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(b, &req)
		n := 0
		for _, m := range req.Messages {
			n += len(m.Content)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		chunks := []string{
			`{"choices":[{"delta":{"role":"assistant","content":"Hello!"}}]}`,
			fmt.Sprintf(`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":10}}`, n/4),
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
	return fmt.Sprintf("http://%s", ln.Addr()), recorded, mu
}

func compactionConfig(t *testing.T, base, extra string) string {
	return compactionConfigLimits(t, base, extra, 100000, 8192)
}

func compactionConfigLimits(t *testing.T, base, extra string, context, output float64) string {
	work := t.TempDir()
	cfg := fmt.Sprintf(`{
	  "model": "mock/test-model",
	  "provider": {
	    "mock": {
	      "npm": "@ai-sdk/openai-compatible",
	      "options": {"baseURL": "%s/v1", "apiKey": "test"},
	      "models": {
	        "test-model": {"name": "Test Model", "tool_call": true, "limit": {"context": %v, "output": %v}}
	      }
	    }
	  }%s
	}`, base, context, output, extra)
	if err := os.WriteFile(filepath.Join(work, "intellexor.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return work
}

func TestIsOverflow(t *testing.T) {
	m := provider.Model{Limit: provider.Limit{Context: 200000, Output: 32000}}
	// count = input + cache.read + output; usable = window - min(output, 32000)
	if isOverflow(session.Tokens{Input: 100000, Cache: session.CacheTokens{Read: 30000}, Output: 5000}, m, nil) {
		t.Fatal("135500 count vs 168000 usable: no overflow expected")
	}
	if !isOverflow(session.Tokens{Input: 180000, Output: 5000}, m, nil) {
		t.Fatal("185000 count vs 168000 usable: overflow expected")
	}
	// unknown window never triggers (opencode: context === 0)
	if isOverflow(session.Tokens{Input: 999999}, provider.Model{}, nil) {
		t.Fatal("unknown window must never trigger")
	}
	// output reserve capped at outputTokenMax
	big := provider.Model{Limit: provider.Limit{Context: 100000, Output: 200000}}
	if !isOverflow(session.Tokens{Input: 69000}, big, nil) {
		t.Fatal("reserve must cap at 32000: 69000 > 100000-32000")
	}
	// config reserved overrides the output-based reserve: reserving less
	// delays compaction (usable = window - reserved)
	small := provider.Model{Limit: provider.Limit{Context: 10000, Output: 8000}}
	if !isOverflow(session.Tokens{Input: 2500}, small, nil) {
		t.Fatal("default reserve = output limit 8000 -> usable 2000: overflow expected")
	}
	r := float64(1000)
	if isOverflow(session.Tokens{Input: 2500}, small, &r) {
		t.Fatal("reserved 1000 -> usable 9000: no overflow expected")
	}
}

func TestFilterCompacted(t *testing.T) {
	u1 := session.UserMessage{ID: "u1", Role: "user"}
	a1 := session.AssistantMessage{ID: "a1", Role: "assistant"}
	sum := session.AssistantMessage{ID: "sum", Role: "assistant", Summary: true}
	u2 := session.UserMessage{ID: "u2", Role: "user"}
	msgs := []session.WithParts{{Info: u1}, {Info: a1}, {Info: sum}, {Info: u2}}
	got := filterCompacted(msgs)
	if len(got) != 2 {
		t.Fatalf("expected history to replay from the summary on, got %d messages", len(got))
	}
	if id := messageIDOf(got[0].Info); id != "sum" {
		t.Fatalf("expected history to start at the summary message, got %s", id)
	}
	if same := filterCompacted([]session.WithParts{{Info: u1}, {Info: a1}}); len(same) != 2 {
		t.Fatalf("no summary: history must be untouched, got %d", len(same))
	}
}

func TestCompactManual(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	base := mockLLM(t)
	work := compactionConfig(t, base, "")
	i := New(work)

	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "compact test"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"type": "text", "text": "hello world"})
	if _, err := i.Prompt(s, PromptInput{Model: &ModelSel{ProviderID: "mock", ModelID: "test-model"}, Parts: []json.RawMessage{raw}}); err != nil {
		t.Fatal(err)
	}
	before, err := i.Mgr.Find(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Context <= 0 {
		t.Fatalf("expected a context estimate after the prompt, got %v", before.Context)
	}

	wp, err := i.Compact(s, "mock", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := wp.Info.(session.AssistantMessage)
	if !ok || !a.Summary {
		t.Fatalf("expected a summary:true assistant message, got %+v", wp.Info)
	}
	if a.Tokens.Input == 0 && a.Tokens.Output == 0 {
		t.Fatalf("expected the summary call's usage on the message, got %+v", a.Tokens)
	}
	// the summary replays the whole conversation: parent is the last user message
	msgs, _ := i.Mgr.LoadMessages(s)
	var lastUser string
	for _, m := range msgs {
		if u, ok := m.Info.(session.UserMessage); ok {
			lastUser = u.ID
		}
	}
	if a.ParentID != lastUser {
		t.Fatalf("expected summary parentID=%s, got %s", lastUser, a.ParentID)
	}
	// compacting flag cleared
	fresh, err := i.Mgr.Find(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Time.Compacting != nil {
		t.Fatal("time.compacting must be cleared after compaction")
	}
	// the context estimate must reset to the summary's own footprint so the
	// UI circle drops immediately instead of keeping the pre-compaction size
	if fresh.Context != a.Tokens.Output+a.Tokens.Reasoning {
		t.Fatalf("expected context reset to the summary footprint %v, got %v", a.Tokens.Output+a.Tokens.Reasoning, fresh.Context)
	}
	if fresh.Context >= before.Context {
		t.Fatalf("expected compaction to shrink the context estimate: %v not < %v", fresh.Context, before.Context)
	}
	// history now replays from the summary alone
	filtered := filterCompacted(msgs)
	if len(filtered) != 1 || messageIDOf(filtered[0].Info) != a.ID {
		t.Fatalf("expected history to start at the summary, got %d messages", len(filtered))
	}
	// the summary text is a plain text part on the message
	if len(wp.Parts) != 1 || wp.Parts[0].Type != "text" || wp.Parts[0].Text != "Hello!" {
		t.Fatalf("expected the streamed summary as a text part, got %+v", wp.Parts)
	}
}

func TestAutoCompactOnOverflow(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	base, bodies, mu := mockLLMUsage(t)
	// usable window = 2000 - 1000 = 1000 tokens: an 8000-char prompt
	// (~2000 tokens) must trip auto compaction on the next prompt
	work := compactionConfigLimits(t, base, "", 2000, 1000)
	i := New(work)

	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "auto compact"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	sel := &ModelSel{ProviderID: "mock", ModelID: "test-model"}

	big, _ := json.Marshal(map[string]any{"type": "text", "text": strings.Repeat("x", 8000)})
	if _, err := i.Prompt(s, PromptInput{Model: sel, Parts: []json.RawMessage{big}}); err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(map[string]any{"type": "text", "text": "again"})
	if _, err := i.Prompt(s, PromptInput{Model: sel, Parts: []json.RawMessage{again}}); err != nil {
		t.Fatal(err)
	}

	msgs, _ := i.Mgr.LoadMessages(s)
	summaries, resumes := 0, 0
	var summaryID string
	for _, m := range msgs {
		if a, ok := m.Info.(session.AssistantMessage); ok && a.Summary {
			summaries++
			summaryID = a.ID
		}
		if _, ok := m.Info.(session.UserMessage); ok {
			for _, p := range m.Parts {
				if p.Synthetic && strings.Contains(p.Text, "resume from where you left off") {
					resumes++
				}
			}
		}
	}
	if summaries != 1 {
		t.Fatalf("expected exactly one summary message after the overflow, got %d", summaries)
	}
	if resumes != 1 {
		t.Fatalf("expected exactly one synthetic resume message, got %d", resumes)
	}

	// the post-compaction request must replay [system, summary assistant, resume user]
	mu.Lock()
	snapshot := append([]string(nil), *bodies...)
	mu.Unlock()
	found := false
	for _, b := range snapshot {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal([]byte(b), &req) != nil || len(req.Messages) < 3 {
			continue
		}
		for j := 0; j+1 < len(req.Messages); j++ {
			if req.Messages[j].Role == "assistant" && req.Messages[j+1].Role == "user" &&
				strings.Contains(fmt.Sprint(req.Messages[j+1].Content), "resume from where you left off") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no request replayed [summary, resume] after auto compaction")
	}

	// the summary call itself carried the whole conversation + the ask
	sawSummaryAsk := false
	for _, b := range snapshot {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal([]byte(b), &req) != nil || len(req.Messages) == 0 {
			continue
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" && strings.Contains(fmt.Sprint(last.Content), "Provide a detailed but concise summary") {
			sawSummaryAsk = true
		}
	}
	if !sawSummaryAsk {
		t.Fatal("no summarize request with the summary ask was sent")
	}

	// the fresh context is small again: a third prompt must not re-compact
	third, _ := json.Marshal(map[string]any{"type": "text", "text": "third"})
	if _, err := i.Prompt(s, PromptInput{Model: sel, Parts: []json.RawMessage{third}}); err != nil {
		t.Fatal(err)
	}
	msgs, _ = i.Mgr.LoadMessages(s)
	n := 0
	for _, m := range msgs {
		if a, ok := m.Info.(session.AssistantMessage); ok && a.Summary && a.ID != summaryID {
			n++
		}
	}
	if n != 0 {
		t.Fatal("third prompt re-compacted although the context fits again")
	}
}

func TestAutoCompactDisabled(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	base, _, _ := mockLLMUsage(t)
	work := compactionConfig(t, base, `,"compaction": {"auto": false}`)
	i := New(work)

	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "no auto"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	sel := &ModelSel{ProviderID: "mock", ModelID: "test-model"}
	big, _ := json.Marshal(map[string]any{"type": "text", "text": strings.Repeat("x", 8000)})
	if _, err := i.Prompt(s, PromptInput{Model: sel, Parts: []json.RawMessage{big}}); err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(map[string]any{"type": "text", "text": "again"})
	if _, err := i.Prompt(s, PromptInput{Model: sel, Parts: []json.RawMessage{again}}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := i.Mgr.LoadMessages(s)
	for _, m := range msgs {
		if a, ok := m.Info.(session.AssistantMessage); ok && a.Summary {
			t.Fatal("compaction.auto=false must disable the automatic summary")
		}
	}
}

func bigToolOutputPart(sessionID, messageID, callID, output string) session.Part {
	return session.Part{
		ID: util.ID("prt"), SessionID: sessionID, MessageID: messageID, Type: "tool",
		CallID: callID, Tool: "bash",
		State: &session.ToolState{
			Status: "completed",
			Input:  map[string]any{"command": "ls"},
			Output: output,
			Time:   &session.ToolTime{Start: util.Now()},
		},
	}
}

func TestPruneMarksOldToolOutputs(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	i := New(work)
	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "prune"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}

	// three turns; the first turn's tool output is ~50k tokens, the second
	// turn's is small and must stay protected
	save := func(role, id, text string) {
		if role == "user" {
			u := session.UserMessage{ID: id, SessionID: s.ID, Role: "user", Time: session.UserMsgTime{Created: util.Now()}}
			_ = i.Mgr.SaveMessage(s, u)
			p := session.Part{ID: util.ID("prt"), SessionID: s.ID, MessageID: id, Type: "text", Text: text}
			_ = i.Mgr.SavePart(s, p)
			return
		}
		a := session.AssistantMessage{ID: id, SessionID: s.ID, Role: "assistant", Time: session.MsgTime{Created: util.Now()}}
		_ = i.Mgr.SaveMessage(s, a)
	}
	save("user", "msg_u1", "turn one")
	save("assistant", "msg_a1", "")
	big := bigToolOutputPart(s.ID, "msg_a1", "call_big", strings.Repeat("x", 200000))
	_ = i.Mgr.SavePart(s, big)
	save("user", "msg_u2", "turn two")
	save("assistant", "msg_a2", "")
	small := bigToolOutputPart(s.ID, "msg_a2", "call_small", "tiny output")
	_ = i.Mgr.SavePart(s, small)
	save("user", "msg_u3", "turn three")

	i.Prune(s)

	msgs, _ := i.Mgr.LoadMessages(s)
	var bigPart, smallPart *session.Part
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.CallID == "call_big" {
				cp := p
				bigPart = &cp
			}
			if p.CallID == "call_small" {
				cp := p
				smallPart = &cp
			}
		}
	}
	if bigPart == nil || bigPart.State == nil || bigPart.State.Time == nil || bigPart.State.Time.Compacted == nil {
		t.Fatal("expected the >40k-token old tool output to be marked compacted")
	}
	if smallPart == nil || smallPart.State == nil || smallPart.State.Time == nil || smallPart.State.Time.Compacted != nil {
		t.Fatal("the newest turns' tool output must stay untouched")
	}

	// history renders pruned outputs as cleared
	hist := historyFrom(msgs)
	saw := false
	for _, m := range hist {
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.Output == prunedToolOutput {
				saw = true
			}
		}
	}
	if !saw {
		t.Fatal("expected the pruned tool result to replay as cleared output")
	}
}

func TestPruneUnderMinimum(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	i := New(work)
	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "prune min"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	save := func(role, id, text string) {
		if role == "user" {
			u := session.UserMessage{ID: id, SessionID: s.ID, Role: "user", Time: session.UserMsgTime{Created: util.Now()}}
			_ = i.Mgr.SaveMessage(s, u)
			p := session.Part{ID: util.ID("prt"), SessionID: s.ID, MessageID: id, Type: "text", Text: text}
			_ = i.Mgr.SavePart(s, p)
			return
		}
		a := session.AssistantMessage{ID: id, SessionID: s.ID, Role: "assistant", Time: session.MsgTime{Created: util.Now()}}
		_ = i.Mgr.SaveMessage(s, a)
	}
	save("user", "msg_u1", "turn one")
	save("assistant", "msg_a1", "")
	// 30k tokens: beyond the 40k protect? no — under it, so nothing is pruned
	_ = i.Mgr.SavePart(s, bigToolOutputPart(s.ID, "msg_a1", "call_mid", strings.Repeat("x", 120000)))
	save("user", "msg_u2", "turn two")
	save("assistant", "msg_a2", "")
	save("user", "msg_u3", "turn three")

	i.Prune(s)
	msgs, _ := i.Mgr.LoadMessages(s)
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.State != nil && p.State.Time != nil && p.State.Time.Compacted != nil {
				t.Fatal("outputs within the protect budget must not be pruned")
			}
		}
	}
}

// TestCompactRequestShape pins the two compaction request shapes: the
// fallback path (conversation's own model) mirrors the step request so
// vLLM's prefix cache covers the replayed history; an explicitly requested
// compaction model uses the dedicated summarize prompt instead.
func TestCompactRequestShape(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	base, bodies, mu := mockLLMUsage(t)
	work := compactionConfig(t, base, "")
	i := New(work)

	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "request shape"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"type": "text", "text": "hello world"})
	if _, err := i.Prompt(s, PromptInput{Model: &ModelSel{ProviderID: "mock", ModelID: "test-model"}, Parts: []json.RawMessage{raw}}); err != nil {
		t.Fatal(err)
	}

	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), *bodies...)
	}

	// the mock also sees the async title request, so requests are classified
	// by content rather than position: the step request ends with the bare
	// user prompt, a compaction request ends with the summarize ask
	type wireMsg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	var probe struct {
		Messages []wireMsg `json:"messages"`
	}
	isCompact := func(b string) bool { return strings.Contains(b, "summary of our conversation above") }
	isStep := func(b string) bool {
		if isCompact(b) || json.Unmarshal([]byte(b), &probe) != nil || len(probe.Messages) == 0 {
			return false
		}
		last := probe.Messages[len(probe.Messages)-1]
		return last.Role == "user" && string(last.Content) == `"hello world"`
	}
	var body struct {
		Messages []wireMsg `json:"messages"`
		Tools    []any     `json:"tools"`
	}

	// fallback path (no explicit model): the summarize request must mirror
	// the step request — same system prompt, tools present, and the whole
	// step conversation as a token-identical prefix (the prefix-cache hit)
	if _, err := i.Compact(s, "", ""); err != nil {
		t.Fatal(err)
	}
	var stepBody, summaryBody string
	for _, b := range snapshot() {
		if isStep(b) {
			stepBody = b
		}
		if isCompact(b) {
			summaryBody = b
		}
	}
	if stepBody == "" || summaryBody == "" {
		t.Fatalf("expected a step and a summary request, got %d bodies", len(snapshot()))
	}
	if err := json.Unmarshal([]byte(stepBody), &body); err != nil {
		t.Fatal(err)
	}
	stepMsgs := append([]wireMsg(nil), body.Messages...)
	body = struct {
		Messages []wireMsg `json:"messages"`
		Tools    []any     `json:"tools"`
	}{}
	if err := json.Unmarshal([]byte(summaryBody), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) == 0 || body.Messages[0].Role != "system" {
		t.Fatalf("expected a system message in the mirrored summary request, got %+v", body.Messages)
	}
	if string(body.Messages[0].Content) != string(stepMsgs[0].Content) {
		t.Fatal("mirrored summary request must reuse the step request's system prompt")
	}
	if len(body.Tools) == 0 {
		t.Fatal("mirrored summary request must carry the step request's tools")
	}
	if len(body.Messages) <= len(stepMsgs) {
		t.Fatalf("expected the summarize ask appended after the step history, got %d messages", len(body.Messages))
	}
	for j := range stepMsgs {
		if stepMsgs[j].Role != body.Messages[j].Role || string(stepMsgs[j].Content) != string(body.Messages[j].Content) {
			t.Fatalf("summary request diverges from the step request at message %d", j)
		}
	}
	if last := body.Messages[len(body.Messages)-1]; !strings.Contains(string(last.Content), "do not call any tools") {
		t.Fatalf("mirrored path must pin the model to a text answer, got %s", last.Content)
	}

	// explicit compaction model: dedicated summarize prompt, no tools.
	// A fresh turn first — after the first compaction the history starts
	// at the summary, and compacting needs a user message to summarize.
	raw2, _ := json.Marshal(map[string]any{"type": "text", "text": "second turn"})
	if _, err := i.Prompt(s, PromptInput{Model: &ModelSel{ProviderID: "mock", ModelID: "test-model"}, Parts: []json.RawMessage{raw2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Compact(s, "mock", "test-model"); err != nil {
		t.Fatal(err)
	}
	var explicit string
	for _, b := range snapshot() {
		if isCompact(b) && strings.Contains(b, "summarizing conversations") {
			explicit = b
		}
	}
	if explicit == "" {
		t.Fatal("expected a dedicated-prompt summary request for the explicit model")
	}
	body = struct {
		Messages []wireMsg `json:"messages"`
		Tools    []any     `json:"tools"`
	}{}
	if err := json.Unmarshal([]byte(explicit), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) == 0 || body.Messages[0].Role != "system" {
		t.Fatalf("expected a system message in the explicit summary request, got %+v", body.Messages)
	}
	var sys string
	if err := json.Unmarshal(body.Messages[0].Content, &sys); err != nil || sys != summarizeSystemPrompt {
		t.Fatalf("explicit-model summary must use the dedicated summarize prompt, got %s", body.Messages[0].Content)
	}
	if len(body.Tools) != 0 {
		t.Fatal("explicit-model summary must not carry tools")
	}
	if last := body.Messages[len(body.Messages)-1]; strings.Contains(string(last.Content), "do not call any tools") {
		t.Fatal("dedicated summarize prompt needs no no-tools pin")
	}
}
