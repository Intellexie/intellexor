package permission

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"intellexor/internal/bus"
	"intellexor/internal/session"
	"intellexor/internal/util"
)

type Request = session.PermissionRequest

type Service struct {
	mu       sync.Mutex
	hub      *bus.Hub
	pending  map[string]*entry
	approved map[string][]session.PermissionRule // session-level "always" rules
	ruleset  []session.PermissionRule            // static rules (config/agent defaults)
}

type entry struct {
	req      session.PermissionRequest
	ruleset  []session.PermissionRule
	reply    chan string // "once"|"always"|"reject"
	feedback string
	rejected bool
}

type DeniedError struct {
	Ruleset []session.PermissionRule
}

func (e *DeniedError) Error() string {
	return "The user has specified a rule which prevents you from using this specific tool call. Here are some of the relevant rules " + jsonString(e.Ruleset)
}

type RejectedError struct{}

func (e *RejectedError) Error() string {
	return "The user rejected permission to use this specific tool call."
}

type CorrectedError struct {
	Feedback string
}

func (e *CorrectedError) Error() string {
	return "The user rejected permission to use this specific tool call with the following feedback: " + e.Feedback
}

func New(hub *bus.Hub, ruleset []session.PermissionRule) *Service {
	return &Service{
		hub:      hub,
		pending:  map[string]*entry{},
		approved: map[string][]session.PermissionRule{},
		ruleset:  ruleset,
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// matches mirrors opencode's util/wildcard.ts: '*' -> '.*' (crosses '/'),
// '?' -> '.', and a trailing " *" also matches the bare prefix ("ls *"
// matches "ls" and "ls -la").
func matches(value, pattern string) bool {
	value = strings.ReplaceAll(value, "\\", "/")
	pattern = strings.ReplaceAll(pattern, "\\", "/")
	var sb strings.Builder
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteString(".")
		default:
			if strings.ContainsRune(".+^${}()|[]\\", rune(c)) {
				sb.WriteByte('\\')
			}
			sb.WriteByte(c)
		}
	}
	escaped := sb.String()
	if strings.HasSuffix(escaped, " .*") {
		escaped = escaped[:len(escaped)-3] + "( .*)?"
	}
	re, err := regexp.Compile("(?s)^" + escaped + "$")
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

// evaluate last-matching-rule-wins across rules; default ask
func evaluate(rules []session.PermissionRule, permission, pattern string) string {
	action := ""
	for _, r := range rules {
		if matches(permission, r.Permission) && matches(pattern, r.Pattern) {
			action = r.Action
		}
	}
	if action == "" {
		return "ask"
	}
	return action
}

func Merge(rules ...[]session.PermissionRule) []session.PermissionRule {
	var out []session.PermissionRule
	for _, r := range rules {
		out = append(out, r...)
	}
	return out
}

type AskInput struct {
	SessionID  string
	Permission string
	Patterns   []string
	Metadata   map[string]any
	Always     []string
	Tool       *session.ToolCallRef
	Ruleset    []session.PermissionRule
}

// Ask always approves. Intellexor runs fully trusted: no permission prompts are
// ever surfaced to the user (opencode asks here when rules say ask/deny).
// Reply/Reject/Pending remain for API compatibility.
func (s *Service) Ask(ctx context.Context, in AskInput) error {
	if in.Metadata == nil {
		in.Metadata = map[string]any{}
	}
	return nil
}

func (s *Service) askDeprecated(ctx context.Context, in AskInput) error {
	if in.Metadata == nil {
		in.Metadata = map[string]any{}
	}
	rules := append(Merge(s.ruleset, in.Ruleset), s.Approved(in.SessionID)...)

	denyAction := ""
	needAsk := false
	for _, p := range in.Patterns {
		switch action := evaluate(rules, in.Permission, p); action {
		case "deny":
			denyAction = "deny"
		case "ask":
			needAsk = true
		}
	}
	if denyAction != "" {
		return &DeniedError{Ruleset: rules}
	}
	if !needAsk {
		return nil
	}

	req := session.PermissionRequest{
		ID:         util.ID("per"),
		SessionID:  in.SessionID,
		Permission: in.Permission,
		Patterns:   in.Patterns,
		Metadata:   in.Metadata,
		Always:     in.Always,
		Tool:       in.Tool,
	}
	e := &entry{req: req, ruleset: rules, reply: make(chan string, 1)}
	s.mu.Lock()
	s.pending[req.ID] = e
	s.mu.Unlock()

	s.hub.Publish("permission.asked", map[string]any{"id": req.ID, "sessionID": req.SessionID, "permission": req.Permission, "patterns": req.Patterns, "metadata": req.Metadata, "always": req.Always, "tool": req.Tool})

	select {
	case <-ctx.Done():
		s.Reject(ctx, req.ID, "Aborted")
		return ctx.Err()
	case reply := <-e.reply:
		switch reply {
		case "reject":
			if e.feedback != "" {
				return &CorrectedError{Feedback: e.feedback}
			}
			return &RejectedError{}
		case "always":
			s.addApproved(in.SessionID, in.Permission, in.Always)
			// auto-approve other pending requests in this session that now pass
			s.mu.Lock()
			var toResolve []*entry
			for id, other := range s.pending {
				if id == req.ID || other.req.SessionID != in.SessionID {
					continue
				}
				ok := true
				rules2 := append(Merge(s.ruleset, other.ruleset), s.Approved(in.SessionID)...)
				for _, p := range other.req.Patterns {
					if evaluate(rules2, other.req.Permission, p) != "allow" {
						ok = false
					}
				}
				if ok {
					toResolve = append(toResolve, other)
					delete(s.pending, id)
				}
			}
			s.mu.Unlock()
			for _, t := range toResolve {
				t.reply <- "always"
				s.hub.Publish("permission.replied", map[string]any{"sessionID": in.SessionID, "requestID": t.req.ID, "reply": "always"})
			}
		}
		return nil
	}
}

func (s *Service) addApproved(sessionID, permission string, always []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range always {
		s.approved[sessionID] = append(s.approved[sessionID], session.PermissionRule{Permission: permission, Pattern: a, Action: "allow"})
	}
}

func (s *Service) Approved(sessionID string) []session.PermissionRule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]session.PermissionRule{}, s.approved[sessionID]...)
}

func (s *Service) Reject(ctx context.Context, id string, reason string) {
	s.mu.Lock()
	e, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if ok {
		e.reply <- "reject"
		s.hub.Publish("permission.replied", map[string]any{"sessionID": e.req.SessionID, "requestID": id, "reply": "reject"})
	}
}

func (s *Service) Reply(id string, reply string, message string) error {
	s.mu.Lock()
	e, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
		if reply == "reject" && message != "" {
			e.feedback = message
		}
	}
	s.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	e.reply <- reply
	s.hub.Publish("permission.replied", map[string]any{"sessionID": e.req.SessionID, "requestID": id, "reply": reply})
	if reply == "reject" {
		// reject all other pending requests in the same session
		s.mu.Lock()
		var others []*entry
		for pid, other := range s.pending {
			if other.req.SessionID == e.req.SessionID {
				others = append(others, other)
				delete(s.pending, pid)
			}
		}
		s.mu.Unlock()
		for _, o := range others {
			o.reply <- "reject"
			s.hub.Publish("permission.replied", map[string]any{"sessionID": o.req.SessionID, "requestID": o.req.ID, "reply": "reject"})
		}
	}
	return nil
}

func (s *Service) Pending(sessionID string) []session.PermissionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []session.PermissionRequest
	for _, e := range s.pending {
		if sessionID != "" && e.req.SessionID != sessionID {
			continue
		}
		out = append(out, e.req)
	}
	return out
}

var ErrNotFound = &notFound{}

type notFound struct{}

func (n *notFound) Error() string { return "Permission not found" }
