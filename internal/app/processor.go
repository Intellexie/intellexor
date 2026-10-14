package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Intellexie/intellexor/internal/permission"
	"github.com/Intellexie/intellexor/internal/provider"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/tool"
	"github.com/Intellexie/intellexor/internal/util"
)

type processor struct {
	i   *Instance
	s   *session.Session
	msg *session.AssistantMessage

	text       map[string]*strings.Builder
	textPart   map[string]*session.Part
	reasoning  map[string]*strings.Builder
	reasonPart map[string]*session.Part

	toolsCalled int
	failed      bool
	aborted     bool

	doomSig   string
	doomCount int

	fileDiffs []session.FileDiff

	ruleset []session.PermissionRule
	model   provider.Model
}

func newProcessor(i *Instance, s *session.Session, m *session.AssistantMessage) *processor {
	return &processor{
		i: i, s: s, msg: m,
		text: map[string]*strings.Builder{}, textPart: map[string]*session.Part{},
		reasoning: map[string]*strings.Builder{}, reasonPart: map[string]*session.Part{},
	}
}

func (p *processor) publishPart(part session.Part) {
	_ = p.i.Mgr.SavePart(p.s, part)
	p.i.Hub.Publish("message.part.updated", map[string]any{"sessionID": p.s.ID, "part": part, "time": util.Now()})
}

func (p *processor) runStep(ctx context.Context, resolved *provider.Resolved, model provider.Model, agentName string, agent AgentDef, extraSystem string) {
	p.ruleset = agent.Permission
	p.model = model
	i := p.i
	client := resolved.Client()

	// step-start part
	p.publishPart(session.Part{
		ID: util.ID("prt"), SessionID: p.s.ID, MessageID: p.msg.ID, Type: "step-start",
	})

	history := BuildHistory(i, p.s)
	system := systemPrompt(i)
	if agent.Prompt != "" {
		system = agent.Prompt
	}
	if extraSystem != "" {
		system += "\n\n" + extraSystem
	}
	if u := instructionText(i.Directory); u != "" {
		system += "\n\n" + u
	}

	names := activeToolNames(i.Directory, agent)
	names = toolNamesForModel(model.ID, names)
	req := provider.Request{
		Model:     model.ID,
		System:    system,
		Messages:  history,
		Tools:     registryFor(i).Defs(names),
		MaxTokens: int(model.Limit.Output),
	}
	if model.Reasoning {
		req.Reasoning = true
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = 8192
	}
	if len(model.Options) > 0 {
		if req.Extra == nil {
			req.Extra = map[string]any{}
		}
		for k, v := range model.Options {
			req.Extra[k] = v
		}
	}
	if model.Temperature && agent.Temperature != nil {
		req.Temperature = agent.Temperature
	}

	ch, err := client.Stream(ctx, req, func(attempt int, serr *provider.StreamError, next time.Time) {
		at := attempt + 1
		nextMs := next.UnixMilli()
		p.i.SetStatus(p.s.ID, StatusInfo{"type": "retry", "attempt": at, "message": serr.Message, "next": nextMs})
		errData := map[string]any{
			"message": serr.Message, "isRetryable": serr.IsRetryable,
		}
		if serr.StatusCode != 0 {
			errData["statusCode"] = serr.StatusCode
		}
		if serr.ResponseBody != "" {
			errData["responseBody"] = serr.ResponseBody
		}
		part := session.Part{
			ID: util.ID("prt"), SessionID: p.s.ID, MessageID: p.msg.ID, Type: "retry",
			Attempt: at,
			Error:   session.NewNamedError("APIError", errData),
			Time:    &session.PartTime{Start: util.Now()},
		}
		p.publishPart(part)
	})
	if err != nil {
		p.failWith(&provider.StreamError{Message: err.Error()})
		return
	}

	for ev := range ch {
		if ctx.Err() != nil {
			break
		}
		switch ev.Type {
		case "text-start":
			b := &strings.Builder{}
			p.text[ev.ID] = b
			part := session.Part{
				ID: util.ID("prt"), SessionID: p.s.ID, MessageID: p.msg.ID, Type: "text",
				Text: "", Time: &session.PartTime{Start: util.Now()},
			}
			p.textPart[ev.ID] = &part
			p.publishPart(part)
		case "text-delta":
			if b, ok := p.text[ev.ID]; ok {
				b.WriteString(ev.Text)
			}
			if pt, ok := p.textPart[ev.ID]; ok {
				p.i.Hub.Publish("message.part.delta", map[string]any{
					"sessionID": p.s.ID, "messageID": p.msg.ID, "partID": pt.ID, "field": "text", "delta": ev.Text,
				})
			}
		case "text-end":
			pt := p.textPart[ev.ID]
			b := p.text[ev.ID]
			if pt != nil {
				end := util.Now()
				pt.Text = b.String()
				pt.Time = &session.PartTime{Start: pt.Time.Start, End: &end}
				p.publishPart(*pt)
				delete(p.textPart, ev.ID)
				delete(p.text, ev.ID)
			}
		case "reasoning-start":
			b := &strings.Builder{}
			p.reasoning[ev.ID] = b
			part := session.Part{
				ID: util.ID("prt"), SessionID: p.s.ID, MessageID: p.msg.ID, Type: "reasoning",
				Text: "", Time: &session.PartTime{Start: util.Now()},
			}
			p.reasonPart[ev.ID] = &part
			p.publishPart(part)
		case "reasoning-delta":
			if b, ok := p.reasoning[ev.ID]; ok {
				b.WriteString(ev.Text)
			}
			if pt, ok := p.reasonPart[ev.ID]; ok {
				p.i.Hub.Publish("message.part.delta", map[string]any{
					"sessionID": p.s.ID, "messageID": p.msg.ID, "partID": pt.ID, "field": "text", "delta": ev.Text,
				})
			}
		case "reasoning-end":
			pt := p.reasonPart[ev.ID]
			b := p.reasoning[ev.ID]
			if pt != nil {
				end := util.Now()
				pt.Text = b.String()
				pt.Time = &session.PartTime{Start: pt.Time.Start, End: &end}
				p.publishPart(*pt)
				delete(p.reasonPart, ev.ID)
				delete(p.reasoning, ev.ID)
			}
		case "tool-call":
			p.runTool(ctx, ev, agentName)
		case "error":
			p.failWith(ev.Err)
			return
		case "finish":
			p.finishStep(ev)
		}
	}
	if ctx.Err() != nil && !p.failed {
		p.failAborted()
	}
}

func (p *processor) runTool(ctx context.Context, ev provider.StreamEvent, agentName string) {
	i := p.i
	callID := ev.ID
	if callID == "" {
		callID = util.ID("call")
	}

	inputMap := map[string]any{}
	_ = json.Unmarshal(ev.Input, &inputMap)

	state := &session.ToolState{
		Status: "pending",
		Input:  inputMap,
		Raw:    string(ev.Input),
	}
	part := session.Part{
		ID: util.ID("prt"), SessionID: p.s.ID, MessageID: p.msg.ID, Type: "tool",
		CallID: callID, Tool: ev.Name, State: state,
	}
	p.publishPart(part)

	// doom loop detection
	sig := ev.Name + "\x00" + string(ev.Input)
	if sig == p.doomSig {
		p.doomCount++
	} else {
		p.doomSig = sig
		p.doomCount = 1
	}
	if p.doomCount == 3 {
		err := i.Perms.Ask(ctx, permission.AskInput{
			SessionID: p.s.ID, Permission: "doom_loop",
			Patterns: []string{ev.Name}, Always: []string{ev.Name},
			Metadata: map[string]any{"tool": ev.Name, "input": inputMap},
			Ruleset:  i.AskChain(p.ruleset),
		})
		if err != nil {
			p.finishToolError(&part, err, nil)
			return
		}
	}

	start := util.Now()
	state = &session.ToolState{Status: "running", Input: inputMap, Time: &session.ToolTime{Start: start}}
	part.State = state
	p.publishPart(part)

	tctx := &tool.Ctx{
		Ctx:       ctx,
		SessionID: p.s.ID,
		MessageID: p.msg.ID,
		CallID:    callID,
		Directory: i.Directory,
		Worktree:  i.Worktree,
		Agent:     agentName,
		Meta: func(title string, metadata map[string]any) {
			st := part.State
			if st != nil && st.Status == "running" {
				if title != "" {
					st.Title = title
				}
				if metadata != nil {
					if st.Metadata == nil {
						st.Metadata = map[string]any{}
					}
					for k, v := range metadata {
						st.Metadata[k] = v
					}
				}
				p.publishPart(part)
			}
		},
	}
	tctx.Ask = func(in tool.AskInput) error {
		return i.Perms.Ask(ctx, permission.AskInput{
			SessionID: p.s.ID, Permission: in.Permission,
			Patterns: in.Patterns, Always: in.Always, Metadata: in.Metadata,
			Ruleset: permission.Merge(i.AskChain(p.ruleset), s2r(p.s.Permission)),
		})
	}
	tctx.Subagent = func(in tool.SubagentInput) (string, string, error) {
		return i.runSubagent(ctx, p.s, in)
	}
	tctx.Backup = func(path string) error {
		return i.Mgr.BackupFile(p.s, p.msg.ID, path)
	}

	res, err := registryFor(i).RunWithValidation(tctx, ev.Name, ev.Input)
	end := util.Now()
	p.toolsCalled++

	if err != nil {
		p.finishToolError(&part, err, &res)
		return
	}

	if fd := extractFileDiff(res.Metadata); fd != nil {
		p.fileDiffs = append(p.fileDiffs, *fd)
	}
	for _, a := range res.Attachments {
		p.publishPart(a)
	}

	md := res.Metadata
	if md == nil {
		md = map[string]any{}
	}
	title := res.Title
	attachments := make([]any, 0, len(res.Attachments))
	for _, a := range res.Attachments {
		attachments = append(attachments, a)
	}
	part.State = &session.ToolState{
		Status: "completed", Input: inputMap, Output: res.Output, Title: title,
		Metadata: md, Time: &session.ToolTime{Start: start, End: &end},
		Attachments: attachments,
	}
	p.publishPart(part)
}

func (p *processor) finishToolError(part *session.Part, err error, res *tool.Result) {
	end := util.Now()
	msg := err.Error()
	md := map[string]any{}
	if pe, ok := err.(*permissionCorrected); ok {
		msg = pe.Error()
	}
	// a tool may return partial results with its error — keep that metadata
	// (e.g. the task tool surfaces the child session id of a failed subagent)
	if res != nil && res.Metadata != nil {
		for k, v := range res.Metadata {
			md[k] = v
		}
	}
	if p.aborted {
		md["interrupted"] = true
	}
	part.State = &session.ToolState{
		Status: "error", Input: part.State.Input, Error: msg, Metadata: md,
		Time: &session.ToolTime{Start: util.Now(), End: &end},
	}
	p.publishPart(*part)
}

type permissionCorrected struct{ msg string }

func (e *permissionCorrected) Error() string { return e.msg }

func extractFileDiff(md map[string]any) *session.FileDiff {
	if md == nil {
		return nil
	}
	if fd, ok := md["filediff"].(session.FileDiff); ok {
		return &fd
	}
	return nil
}

func (p *processor) finishStep(ev provider.StreamEvent) {
	i := p.i
	model := p.model
	usage := ev.Usage
	if usage == nil {
		usage = &provider.Usage{}
	}
	tokens := session.Tokens{
		Input:     usage.Input,
		Output:    usage.Output,
		Reasoning: usage.Reasoning,
		Cache:     session.CacheTokens{Read: usage.CacheRead, Write: usage.CacheWrite},
	}
	var total float64 = usage.Input + usage.Output + usage.Reasoning + usage.CacheRead + usage.CacheWrite
	tokens.Total = &total
	cost := usageCost(model, *usage)

	reason := ev.Finish
	if reason == "" {
		reason = "stop"
	}

	end := util.Now()
	_ = end
	p.publishPart(session.Part{
		ID: util.ID("prt"), SessionID: p.s.ID, MessageID: p.msg.ID, Type: "step-finish",
		Reason: reason, Cost: &cost, Tokens: &tokens,
	})

	p.msg.Finish = reason
	p.msg.Cost += cost
	p.msg.Tokens = tokens
	_ = i.Mgr.SaveMessage(p.s, *p.msg)
	i.Hub.Publish("message.updated", map[string]any{"sessionID": p.s.ID, "info": *p.msg})

	// session totals
	p.s.Cost += cost
	p.s.Tokens = accumulate(p.s.Tokens, tokens)
	// Context is the size of the conversation context (what the next request
	// will contain) — the last step's own usage. Tokens above is cumulative
	// across all steps (a billing metric); feeding it to the context-usage
	// circle made usage race to ~100% on small conversations, because every
	// step re-counts the prompt and every cached request re-counts the whole
	// cached prefix.
	p.s.Context = total
	_ = i.Mgr.Save(p.s)
	// clients rebuild the context-usage circle from session.updated; without
	// this the circle stays at 0% until a full page reload
	i.Hub.Publish("session.updated", map[string]any{"sessionID": p.s.ID, "info": *p.s})

	if len(p.fileDiffs) > 0 {
		p.flushDiffs()
	}
}

func accumulate(total session.Tokens, add session.Tokens) session.Tokens {
	total.Input += add.Input
	total.Output += add.Output
	total.Reasoning += add.Reasoning
	total.Cache.Read += add.Cache.Read
	total.Cache.Write += add.Cache.Write
	t := total.Input + total.Output + total.Reasoning + total.Cache.Read + total.Cache.Write
	total.Total = &t
	return total
}

func (p *processor) flushDiffs() {
	i := p.i
	var existing []session.FileDiff
	_ = i.Mgr.ReadKV("session_diff", p.s.ID, &existing)
	byFile := map[string]session.FileDiff{}
	var order []string
	for _, d := range existing {
		byFile[d.File] = d
		order = append(order, d.File)
	}
	for _, d := range p.fileDiffs {
		if d.File == "" {
			continue
		}
		if prev, ok := byFile[d.File]; ok {
			d.Patch = prev.Patch + "\n" + d.Patch
			if d.Status == "" {
				d.Status = prev.Status
			}
		} else {
			order = append(order, d.File)
		}
		byFile[d.File] = d
	}
	var merged []session.FileDiff
	for _, f := range order {
		merged = append(merged, byFile[f])
	}
	_ = i.Mgr.WriteKV("session_diff", p.s.ID, merged)
	p.i.Hub.Publish("session.diff", map[string]any{"sessionID": p.s.ID, "diff": merged})

	files := len(merged)
	add, del := 0, 0
	for _, m := range merged {
		add += m.Additions
		del += m.Deletions
	}
	p.s.Summary = &session.SessionSummary{Files: files, Additions: add, Deletions: del}
}

func (p *processor) failWith(e *provider.StreamError) {
	i := p.i
	p.failed = true
	p.flushOpenParts()
	if e == nil {
		e = &provider.StreamError{Message: "Unknown error"}
	}
	data := map[string]any{"message": e.Message, "isRetryable": e.IsRetryable}
	if e.StatusCode != 0 {
		data["statusCode"] = e.StatusCode
	}
	if e.ResponseBody != "" {
		data["responseBody"] = e.ResponseBody
	}
	p.msg.Error = session.NewNamedError("APIError", data)
	p.msg.Finish = "error"
	end := util.Now()
	p.msg.Time.Completed = end
	_ = i.Mgr.SaveMessage(p.s, *p.msg)
	i.Hub.Publish("message.updated", map[string]any{"sessionID": p.s.ID, "info": *p.msg})
	i.Hub.Publish("session.error", map[string]any{"sessionID": p.s.ID, "error": *p.msg.Error})
}

func (p *processor) failAborted() {
	p.aborted = true
	p.failed = true
	i := p.i
	p.flushOpenParts()
	p.msg.Error = session.NewNamedError("MessageAbortedError", map[string]any{"message": "Message aborted"})
	end := util.Now()
	p.msg.Time.Completed = end
	_ = i.Mgr.SaveMessage(p.s, *p.msg)
	i.Hub.Publish("message.updated", map[string]any{"sessionID": p.s.ID, "info": *p.msg})
}

// flushOpenParts persists text/reasoning parts that were still streaming when
// the step ended abnormally (abort, API error). Deltas are only ever published
// as events — the part is written to disk at text-end — so without this an
// interrupted part comes back from storage with no text at all.
func (p *processor) flushOpenParts() {
	for id, pt := range p.textPart {
		p.flushOne(pt, p.text[id])
	}
	for id, pt := range p.reasonPart {
		p.flushOne(pt, p.reasoning[id])
	}
	p.text = map[string]*strings.Builder{}
	p.textPart = map[string]*session.Part{}
	p.reasoning = map[string]*strings.Builder{}
	p.reasonPart = map[string]*session.Part{}
}

func (p *processor) flushOne(pt *session.Part, b *strings.Builder) {
	if pt == nil || b == nil {
		return
	}
	end := util.Now()
	pt.Text = b.String()
	pt.Time = &session.PartTime{Start: pt.Time.Start, End: &end}
	p.publishPart(*pt)
}

func finishStep(i *Instance, s *session.Session, asst *session.AssistantMessage, p *processor) {
	if asst.Time.Completed == 0 && !p.failed {
		asst.Time.Completed = util.Now()
	}
	_ = i.Mgr.SaveMessage(s, *asst)
	i.Hub.Publish("message.updated", map[string]any{"sessionID": s.ID, "info": *asst})
}

func usageCost(m provider.Model, u provider.Usage) float64 {
	c := 0.0
	ci := m.Cost.Input
	co := m.Cost.Output
	cr := m.Cost.CacheRead
	if cr == 0 && ci > 0 {
		cr = ci * 0.1
	}
	cw := m.Cost.CacheWrite
	if cw == 0 && ci > 0 {
		cw = ci * 1.25
	}
	c += (u.Input / 1e6) * ci
	c += (u.Output / 1e6) * co
	c += (u.CacheRead / 1e6) * cr
	c += (u.CacheWrite / 1e6) * cw
	return c
}

func s2r(rules []session.PermissionRule) []session.PermissionRule { return rules }
