package app

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/Intellexie/intellexor/internal/config"
	"github.com/Intellexie/intellexor/internal/provider"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/util"
)

// Context compaction, mirroring opencode v1
// (packages/opencode/src/session/compaction.ts + the overflow check in
// session/prompt.ts getMessages):
//
//   - the conversation up to now is summarized into an assistant message with
//     `summary: true`; later requests replay history starting from that
//     message (filterCompacted), so everything the summary covers leaves the
//     context window
//   - auto compaction runs before a step when the newest assistant usage
//     shows the context overflowing the model window minus the output
//     reserve; a synthetic user message then asks the model to resume from
//     the summary
//   - prune clears old tool outputs (ToolTime.Compacted) so stale results
//     stop occupying the window; BuildHistory renders them as
//     "[Old tool result content cleared]"

const (
	// outputTokenMax caps the output reserve (opencode SessionPrompt.OUTPUT_TOKEN_MAX).
	outputTokenMax = 32000
	// pruneMinimum: a prune pass only rewrites parts when it frees at least this many tokens.
	pruneMinimum = 20000
	// pruneProtect: tool output within this budget of the newest end stays verbatim.
	pruneProtect = 40000
	// charsPerToken matches opencode's Token.estimate.
	charsPerToken = 4
)

const summarizeSystemPrompt = `You are a helpful AI assistant tasked with summarizing conversations.

When asked to summarize, provide a detailed but concise summary of the conversation.
Focus on information that would be helpful for continuing the conversation, including:
- What was done
- What is currently being worked on
- Which files are being modified
- What needs to be done next

Your summary should be comprehensive enough to provide context but concise enough to be quickly understood.`

const summarizeAsk = "Provide a detailed but concise summary of our conversation above. Focus on information that would be helpful for continuing the conversation, including what we did, what we're doing, which files we're working on, and what we're going to do next."

const resumeText = "Use the above summary generated from your last session to resume from where you left off."

// prunedToolOutput is what the request history shows for tool results whose
// stored output was cleared by a prune pass (opencode message-v2.ts).
const prunedToolOutput = "[Old tool result content cleared]"

func estimateTokens(s string) float64 {
	return math.Round(float64(len(s)) / charsPerToken)
}

// isOverflow reports whether the newest assistant usage shows the context
// over the model's usable window (opencode SessionCompaction.isOverflow):
// count = input + cache.read + output vs window - output reserve. A model
// with an unknown window (0) never triggers.
func isOverflow(tokens session.Tokens, model provider.Model, reserved *float64) bool {
	window := model.Limit.Context
	if window <= 0 {
		return false
	}
	count := tokens.Input + tokens.Cache.Read + tokens.Output
	var usable float64
	if reserved != nil {
		usable = window - *reserved
	} else {
		out := model.Limit.Output
		if out <= 0 || out > outputTokenMax {
			out = outputTokenMax
		}
		usable = window - out
	}
	return count > usable
}

func reservedOf(cfg *config.CompactionCfg) *float64 {
	if cfg == nil {
		return nil
	}
	return cfg.Reserved
}

func compactionCfg(dir string) *config.CompactionCfg {
	return config.Merge(dir).Compaction
}

// filterCompacted drops everything before the newest summary assistant
// message (opencode MessageV2.filterCompacted): after a compaction, history
// replays from the summary on.
func filterCompacted(msgs []session.WithParts) []session.WithParts {
	for j := len(msgs) - 1; j >= 0; j-- {
		if a, ok := msgs[j].Info.(session.AssistantMessage); ok && a.Summary {
			return msgs[j:]
		}
	}
	return msgs
}

// Compact summarizes the session now — the manual /compact path and the
// opencode-shaped POST /session/:id/summarize. providerID/modelID are
// optional; empty falls back to the session's model, then config, then the
// default pick. Returns the summary message; a failed summary comes back
// with Error set on it (and Summary left false).
func (i *Instance) Compact(s *session.Session, providerID, modelID string) (session.WithParts, error) {
	l := i.lock(s.ID)
	l.Lock()
	defer l.Unlock()

	pid, mid := providerID, modelID
	if pid == "" && s.Model != nil {
		pid, mid = s.Model.ProviderID, s.Model.ID
	}
	if pid == "" {
		if m := config.Merge(i.Directory).Model; m != "" {
			pid, mid, _ = strings.Cut(m, "/")
		}
	}
	if pid == "" {
		all, _ := provider.List(i.Directory)
		pid = pickDefault(all)
		if pid == "" {
			return session.WithParts{}, errors.New("no providers available")
		}
	}
	resolved, model, err := provider.Resolve(i.Directory, pid, mid)
	if err != nil {
		return session.WithParts{}, err
	}
	resolved.Client()

	agentName := s.Agent
	if agentName == "" {
		agentName = "build"
	}
	return i.compactLocked(context.Background(), s, resolved, model, agentName)
}

// compactLocked summarizes the conversation into a `summary: true` assistant
// message (opencode SessionCompaction.run). Callers must hold the session
// lock; the auto path in runLoop does, the HTTP route goes through Compact.
func (i *Instance) compactLocked(ctx context.Context, s *session.Session, resolved *provider.Resolved, model provider.Model, agentName string) (session.WithParts, error) {
	// flag the session as compacting so clients can show it
	// (opencode sets session.time.compacting)
	now := util.Now()
	if fresh, err := i.Mgr.Update(s.ID, func(u *session.Session) { u.Time.Compacting = &now }); err == nil {
		*s = *fresh
		i.Hub.Publish("session.updated", map[string]any{"sessionID": s.ID, "info": *s})
	}
	defer func() {
		if fresh, err := i.Mgr.Update(s.ID, func(u *session.Session) { u.Time.Compacting = nil }); err == nil {
			*s = *fresh
			i.Hub.Publish("session.updated", map[string]any{"sessionID": s.ID, "info": *s})
		}
	}()

	msgs, err := i.Mgr.LoadMessages(s)
	if err != nil {
		return session.WithParts{}, err
	}
	toSummarize := filterCompacted(msgs)
	lastUser := ""
	for j := len(toSummarize) - 1; j >= 0; j-- {
		if u, ok := toSummarize[j].Info.(session.UserMessage); ok {
			lastUser = u.ID
			break
		}
	}
	if lastUser == "" {
		return session.WithParts{}, errors.New("nothing to compact yet")
	}

	asst := session.AssistantMessage{
		ID:         util.ID("msg"),
		SessionID:  s.ID,
		Role:       "assistant",
		ParentID:   lastUser,
		Time:       session.MsgTime{Created: util.Now()},
		ModelID:    model.ID,
		ProviderID: resolved.ID,
		Mode:       "build",
		Agent:      agentName,
		Path:       session.Path{Cwd: i.Directory, Root: i.Worktree},
	}
	_ = i.Mgr.SaveMessage(s, asst)
	i.Hub.Publish("message.updated", map[string]any{"sessionID": s.ID, "info": asst})

	part := session.Part{
		ID: util.ID("prt"), SessionID: s.ID, MessageID: asst.ID, Type: "text",
		Time: &session.PartTime{Start: util.Now()},
	}
	_ = i.Mgr.SavePart(s, part)
	i.Hub.Publish("message.part.updated", map[string]any{"sessionID": s.ID, "part": part, "time": util.Now()})

	history := historyFrom(toSummarize)
	history = append(history, provider.Msg{
		Role:    "user",
		Content: []provider.ContentBlock{{Type: "text", Text: summarizeAsk}},
	})
	maxTokens := int(model.Limit.Output)
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	req := provider.Request{
		Model:     model.ID,
		System:    summarizeSystemPrompt,
		Messages:  history,
		MaxTokens: maxTokens,
	}
	ch, err := resolved.Client().Stream(ctx, req, nil)
	if err != nil {
		return session.WithParts{}, err
	}

	var sb strings.Builder
	var usage *provider.Usage
	var streamErr *provider.StreamError
	for ev := range ch {
		if ctx.Err() != nil {
			break
		}
		switch ev.Type {
		case "text-delta":
			sb.WriteString(ev.Text)
			i.Hub.Publish("message.part.delta", map[string]any{
				"sessionID": s.ID, "messageID": asst.ID, "partID": part.ID, "field": "text", "delta": ev.Text,
			})
		case "finish":
			usage = ev.Usage
		case "error":
			if ev.Err != nil {
				streamErr = ev.Err
			}
		}
	}

	end := util.Now()
	part.Text = sb.String()
	part.Time = &session.PartTime{Start: part.Time.Start, End: &end}
	_ = i.Mgr.SavePart(s, part)
	i.Hub.Publish("message.part.updated", map[string]any{"sessionID": s.ID, "part": part, "time": util.Now()})

	asst.Time.Completed = util.Now()
	if usage != nil {
		tokens := session.Tokens{
			Input:     usage.Input,
			Output:    usage.Output,
			Reasoning: usage.Reasoning,
			Cache:     session.CacheTokens{Read: usage.CacheRead, Write: usage.CacheWrite},
		}
		total := usage.Input + usage.Output + usage.Reasoning + usage.CacheRead + usage.CacheWrite
		tokens.Total = &total
		asst.Tokens = tokens
		asst.Cost = usageCost(model, *usage)
		// the summary call is billed against the session like any other step —
		// read-modify-write so a concurrent title save is not clobbered
		s.Cost += asst.Cost
		s.Tokens = accumulate(s.Tokens, tokens)
		if fresh, err := i.Mgr.Update(s.ID, func(u *session.Session) {
			u.Cost += asst.Cost
			u.Tokens = accumulate(u.Tokens, tokens)
		}); err == nil {
			*s = *fresh
		}
	}
	if ctx.Err() != nil {
		asst.Error = session.NewNamedError("MessageAbortedError", map[string]any{"message": "Message aborted"})
	} else if streamErr != nil {
		data := map[string]any{"message": streamErr.Message, "isRetryable": streamErr.IsRetryable}
		if streamErr.StatusCode != 0 {
			data["statusCode"] = streamErr.StatusCode
		}
		asst.Error = session.NewNamedError("APIError", data)
	}
	if asst.Error == nil && strings.TrimSpace(sb.String()) != "" {
		asst.Summary = true
		// divider part: the web UI renders it as a "Session compacted" line
		divider := session.Part{ID: util.ID("prt"), SessionID: s.ID, MessageID: asst.ID, Type: "compaction"}
		_ = i.Mgr.SavePart(s, divider)
		i.Hub.Publish("message.part.updated", map[string]any{"sessionID": s.ID, "part": divider, "time": util.Now()})
		i.Hub.Publish("session.compacted", map[string]any{"sessionID": s.ID})
	}
	_ = i.Mgr.SaveMessage(s, asst)
	i.Hub.Publish("message.updated", map[string]any{"sessionID": s.ID, "info": asst})
	return session.WithParts{Info: asst, Parts: []session.Part{part}}, nil
}

// appendResumeMessage persists the synthetic user message that asks the model
// to continue from the summary (opencode getMessages overflow branch). It is
// what the auto-compacted request replays after the summary message.
func (i *Instance) appendResumeMessage(s *session.Session, resolved *provider.Resolved, model provider.Model, agentName string) {
	um := session.UserMessage{
		ID:        util.ID("msg"),
		SessionID: s.ID,
		Role:      "user",
		Time:      session.UserMsgTime{Created: util.Now()},
		Agent:     agentName,
		Model:     session.UserModel{ProviderID: resolved.ID, ModelID: model.ID},
	}
	_ = i.Mgr.SaveMessage(s, um)
	i.Hub.Publish("message.updated", map[string]any{"sessionID": s.ID, "info": um})
	p := session.Part{
		ID: util.ID("prt"), SessionID: s.ID, MessageID: um.ID, Type: "text",
		Synthetic: true, Text: resumeText, Time: &session.PartTime{Start: util.Now()},
	}
	_ = i.Mgr.SavePart(s, p)
	i.Hub.Publish("message.part.updated", map[string]any{"sessionID": s.ID, "part": p, "time": util.Now()})
}

// autoCompact mirrors opencode's per-step overflow check in getMessages: when
// the newest assistant usage shows the context over the usable window,
// summarize and continue from the summary. Called before each step of the
// prompt loop; a failed summary leaves history untouched.
func (i *Instance) autoCompact(ctx context.Context, s *session.Session, resolved *provider.Resolved, model provider.Model, agentName string) {
	cfg := compactionCfg(i.Directory)
	if cfg != nil && cfg.Auto != nil && !*cfg.Auto {
		return
	}
	msgs, err := i.Mgr.LoadMessages(s)
	if err != nil {
		return
	}
	var last *session.AssistantMessage
	for j := len(msgs) - 1; j >= 0; j-- {
		if a, ok := msgs[j].Info.(session.AssistantMessage); ok {
			last = &a
			break
		}
	}
	if last == nil {
		return
	}
	if !isOverflow(last.Tokens, model, reservedOf(cfg)) {
		return
	}
	res, err := i.compactLocked(ctx, s, resolved, model, agentName)
	if err != nil {
		return
	}
	if a, ok := res.Info.(session.AssistantMessage); ok && a.Summary {
		i.appendResumeMessage(s, resolved, model, agentName)
	}
}

// Prune clears old tool outputs (opencode SessionCompaction.prune): walking
// backwards from the newest message, completed tool results older than the
// last two turns and beyond the pruneProtect budget are marked compacted —
// but only when the pass frees at least pruneMinimum tokens. BuildHistory
// renders compacted outputs as prunedToolOutput.
func (i *Instance) Prune(s *session.Session) {
	msgs, err := i.Mgr.LoadMessages(s)
	if err != nil {
		return
	}
	var toPrune []session.Part
	var pruned, total float64
	turns := 0
loop:
	for j := len(msgs) - 1; j >= 0; j-- {
		if _, ok := msgs[j].Info.(session.UserMessage); ok {
			turns++
		}
		if turns < 2 {
			continue
		}
		if a, ok := msgs[j].Info.(session.AssistantMessage); ok && a.Summary {
			break // never reach across a compaction boundary
		}
		for k := len(msgs[j].Parts) - 1; k >= 0; k-- {
			p := msgs[j].Parts[k]
			if p.Type != "tool" || p.State == nil || p.State.Status != "completed" {
				continue
			}
			if p.State.Time != nil && p.State.Time.Compacted != nil {
				break loop // already-pruned territory
			}
			est := estimateTokens(p.State.Output)
			total += est
			if total > pruneProtect {
				pruned += est
				toPrune = append(toPrune, p)
			}
		}
	}
	if pruned <= pruneMinimum {
		return
	}
	now := util.Now()
	for _, p := range toPrune {
		if p.State.Time == nil {
			p.State.Time = &session.ToolTime{Start: now}
		}
		p.State.Time.Compacted = &now
		_ = i.Mgr.SavePart(s, p)
		i.Hub.Publish("message.part.updated", map[string]any{"sessionID": s.ID, "part": p, "time": now})
	}
}
