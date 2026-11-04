package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/tool"
)

// The orchestrator is a primary agent restricted to coordination tools: the
// model must never be offered a build tool (bash/edit/write/...), and it is
// the only agent with the cron tool.
func TestOrchestratorToolRestriction(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	i := New(t.TempDir())
	orch, err := i.Agent("orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	if orch.Name != "orchestrator" || orch.Mode != "primary" {
		t.Fatalf("unexpected agent: %+v", orch)
	}

	got := activeToolNames(i.Directory, orch)
	want := []string{"invalid", "board", "webfetch", "todowrite", "task", "cron", "schedule_wakeup"}
	if !slices.Equal(got, want) {
		t.Fatalf("orchestrator tools = %v, want %v", got, want)
	}

	// agents without a Tools list keep the full default set — including the
	// build tools, but not the orchestrator-only cron tool
	buildDef, err := i.Agent("build")
	if err != nil {
		t.Fatal(err)
	}
	build := activeToolNames(i.Directory, buildDef)
	for _, n := range []string{"bash", "read", "edit", "write", "task", "webfetch", "todowrite", "question", "board"} {
		if !slices.Contains(build, n) {
			t.Fatalf("default tool set missing %q: %v", n, build)
		}
	}
	if slices.Contains(build, "cron") {
		t.Fatalf("cron must not be in the default tool set: %v", build)
	}
	if slices.Contains(build, "apply_patch") {
		t.Fatalf("apply_patch is gpt-model-only: %v", build)
	}
}

// Explore agents are read-only researchers: they must never spawn further
// subagents. The task tool is hard-removed from their tool set (the
// Permission rules are inert), and runSubagent refuses the spawn even if the
// model emits a task call that was never offered to it.
func TestExploreCannotSpawnSubagents(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	i := New(t.TempDir())
	explore, err := i.Agent("explore")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(explore.DenyTools, "task") {
		t.Fatalf("explore must deny the task tool: %+v", explore)
	}
	if explore.allowsTool("task") {
		t.Fatal("explore.allowsTool(task) must be false")
	}

	got := activeToolNames(i.Directory, explore)
	if slices.Contains(got, "task") {
		t.Fatalf("task tool offered to the explore agent: %v", got)
	}
	// the rest of the default set stays intact
	for _, n := range []string{"bash", "read", "glob", "grep", "webfetch"} {
		if !slices.Contains(got, n) {
			t.Fatalf("explore lost %q from its tool set: %v", n, got)
		}
	}

	// backstop: runSubagent refuses an explore parent outright, creating no
	// child session
	parent := &session.Session{ProjectID: store.ProjectID(i.Directory), Directory: i.Directory, Agent: "explore"}
	_, childID, err := i.runSubagent(context.Background(), parent, "explore", tool.SubagentInput{
		Description:  "nested",
		Prompt:       "spawn something",
		SubagentType: "explore",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot spawn subagents") {
		t.Fatalf("explore parent must not spawn subagents, got err=%v childID=%q", err, childID)
	}
	if childID != "" {
		t.Fatalf("no child session must be created, got %q", childID)
	}

	// agents with the default tool set are unaffected
	build, err := i.Agent("build")
	if err != nil {
		t.Fatal(err)
	}
	if !build.allowsTool("task") {
		t.Fatal("build must keep the task tool")
	}
}

// The gpt-model apply_patch swap must respect the agent's tool restriction.
func TestToolNamesForModelRespectsRestriction(t *testing.T) {
	restricted := []string{"invalid", "board", "webfetch", "todowrite", "task", "cron"}
	got := toolNamesForModel("gpt-5", restricted, false)
	if slices.Contains(got, "apply_patch") {
		t.Fatalf("restricted agent got apply_patch on a gpt model: %v", got)
	}

	got = toolNamesForModel("gpt-5", []string{"invalid", "bash", "edit", "write"}, true)
	if !slices.Contains(got, "apply_patch") {
		t.Fatalf("gpt model lost apply_patch: %v", got)
	}
	if slices.Contains(got, "edit") || slices.Contains(got, "write") {
		t.Fatalf("gpt model kept edit/write: %v", got)
	}

	// non-gpt models pass through untouched
	got = toolNamesForModel("claude-sonnet-4", restricted, false)
	if !slices.Equal(got, restricted) {
		t.Fatalf("non-gpt model changed the tool list: %v", got)
	}
}

// A session started from a card gets coordination guidance when it runs as
// the orchestrator, and implementation guidance otherwise.
func TestBoardContextByAgent(t *testing.T) {
	i, s := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	card := linkCard(t, i, s, "Coordinate the thing", "inprogress")

	bc := boardContext(i, s, "orchestrator")
	if !strings.Contains(bc, card.ID) {
		t.Fatalf("board context missing card id:\n%s", bc)
	}
	if !strings.Contains(bc, "coordination task") {
		t.Fatalf("orchestrator board context missing coordination guidance:\n%s", bc)
	}
	bc = boardContext(i, s, "build")
	if !strings.Contains(bc, "implement it end to end") {
		t.Fatalf("build board context missing implementation guidance:\n%s", bc)
	}
}

// A config-defined agent is converted into a runtime AgentDef: it resolves
// by name, and a run uses its prompt and model override (not the config
// default model). Unknown agent names error instead of falling back to
// another agent.
func TestConfigAgentWiring(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	var mu sync.Mutex
	var lastBody []byte
	h := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("title generator")) {
			respondEmptyStop(w)
			return
		}
		mu.Lock()
		lastBody = body
		mu.Unlock()
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "ok"}, "finish_reason": "stop"}}}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}
	srv := httptest.NewServer(http.HandlerFunc(h))
	t.Cleanup(srv.Close)

	work := t.TempDir()
	cfg := fmt.Sprintf(`{
		"model": "test/a",
		"provider": {"test": {"name": "test",
			"options": {"baseURL": %q, "apiKey": "k"},
			"models": {"a": {"name": "a", "tool_call": true, "limit": {"context": 100000, "output": 4096}},
				"b": {"name": "b", "tool_call": true, "limit": {"context": 100000, "output": 4096}}}}},
		"agent": {"reviewer": {"description": "Reviews code", "mode": "primary",
			"model": "test/b", "prompt": "You are a reviewer agent.",
			"temperature": 0.3, "tools": {"bash": false}}}
	}`, srv.URL+"/v1")
	if err := os.WriteFile(filepath.Join(work, "intellexor.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	i := New(work)

	rev, err := i.Agent("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if rev.Native {
		t.Fatal("config agent must not be marked native")
	}
	if rev.Model != "test/b" || rev.Prompt != "You are a reviewer agent." || rev.Mode != "primary" {
		t.Fatalf("unexpected agent def: %+v", rev)
	}
	if rev.Temperature == nil || *rev.Temperature != 0.3 {
		t.Fatalf("temperature not converted: %+v", rev)
	}
	if !slices.Contains(rev.DenyTools, "bash") {
		t.Fatalf("tools deny list not converted: %+v", rev)
	}

	// unknown agents error instead of silently becoming build
	if _, err := i.Agent("nope"); err == nil {
		t.Fatal("unknown agent must error")
	}

	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "cfg agent"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"type": "text", "text": "hi"})
	if _, err := i.Prompt(s, PromptInput{Agent: "reviewer", Parts: []json.RawMessage{raw}}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(lastBody, &req); err != nil {
		t.Fatalf("request body is not JSON: %v: %s", err, lastBody)
	}
	if req.Model != "b" {
		t.Fatalf("run did not use the agent's model override: got %q", req.Model)
	}
	if len(req.Messages) == 0 || req.Messages[0].Role != "system" {
		t.Fatalf("no system message in request: %s", lastBody)
	}
	if sys, ok := req.Messages[0].Content.(string); !ok || !strings.Contains(sys, "You are a reviewer agent.") {
		t.Fatalf("run did not use the agent's prompt: %v", req.Messages[0].Content)
	}
	for _, tool := range req.Tools {
		if tool.Function.Name == "bash" {
			t.Fatalf("denied tool offered to the model: %s", lastBody)
		}
	}
}
