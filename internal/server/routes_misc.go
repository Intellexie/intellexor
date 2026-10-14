package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Intellexie/intellexor/internal/auth"
	"github.com/Intellexie/intellexor/internal/config"
	"github.com/Intellexie/intellexor/internal/question"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
)

func mergedConfig(dir string) *config.Info { return config.Merge(dir) }

func projectIDOf(dir string) string { return store.ProjectID(dir) }

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	type agentOut struct {
		Name        string           `json:"name"`
		Description string           `json:"description,omitempty"`
		Mode        string           `json:"mode"`
		Native      bool             `json:"native"`
		Hidden      bool             `json:"hidden"`
		Color       string           `json:"color,omitempty"`
		Permission  []map[string]any `json:"permission"`
	}
	var out []agentOut
	for _, a := range in.Agents() {
		perms := []map[string]any{}
		for _, p := range a.Permission {
			perms = append(perms, map[string]any{"permission": p.Permission, "pattern": p.Pattern, "action": p.Action})
		}
		out = append(out, agentOut{Name: a.Name, Description: a.Description, Mode: a.Mode, Native: true, Hidden: a.Hidden, Color: a.Color, Permission: perms})
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) listCommands(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	merged := mergedConfig(in.Directory)
	out := []map[string]any{}
	for _, name := range sortedBuiltinNames() {
		if _, clash := merged.Commands[name]; clash {
			continue
		}
		b := builtinCommands()[name]
		out = append(out, map[string]any{
			"name": b.name, "description": b.description, "source": "command", "template": b.template, "hints": []any{},
		})
	}
	for name, c := range merged.Commands {
		out = append(out, map[string]any{
			"name": name, "description": c.Description, "agent": c.Agent, "model": c.Model,
			"source": "command", "template": c.Template, "subtask": c.Subtask, "hints": []any{},
		})
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) listPermission(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	reqs := in.Perms.Pending("")
	if reqs == nil {
		reqs = []session.PermissionRequest{}
	}
	writeJSON(w, 200, reqs)
	return nil
}

func (s *Server) replyPermission(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	var body struct {
		Reply   string `json:"reply"`
		Message string `json:"message"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	if body.Reply != "once" && body.Reply != "always" && body.Reply != "reject" {
		return badRequest("reply must be once, always or reject")
	}
	if err := in.Perms.Reply(r.PathValue("requestID"), body.Reply, body.Message); err != nil {
		return notFound("Permission not found: " + r.PathValue("requestID"))
	}
	writeTrue(w)
	return nil
}

func (s *Server) replySessionPermission(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	var body struct {
		Response string `json:"response"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	if err := in.Perms.Reply(r.PathValue("permissionID"), body.Response, ""); err != nil {
		return notFound("Permission not found: " + r.PathValue("permissionID"))
	}
	writeTrue(w)
	return nil
}

func (s *Server) listQuestions(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	reqs := in.Quest.Pending()
	if reqs == nil {
		reqs = []question.Request{}
	}
	writeJSON(w, 200, reqs)
	return nil
}

func (s *Server) replyQuestion(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	var body struct {
		Answers [][]string `json:"answers"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	if err := in.Quest.Reply(r.PathValue("requestID"), body.Answers); err != nil {
		return notFound("Question not found: " + r.PathValue("requestID"))
	}
	writeTrue(w)
	return nil
}

func (s *Server) rejectQuestion(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	if err := in.Quest.Reject(r.PathValue("requestID")); err != nil {
		return notFound("Question not found: " + r.PathValue("requestID"))
	}
	writeTrue(w)
	return nil
}

func (s *Server) putAuth(w http.ResponseWriter, r *http.Request) *apiError {
	providerID := r.PathValue("providerID")
	var body auth.Info
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	if err := auth.Put(providerID, body); err != nil {
		return badRequest(err.Error())
	}
	writeTrue(w)
	return nil
}

func (s *Server) deleteAuth(w http.ResponseWriter, r *http.Request) *apiError {
	if err := auth.Delete(r.PathValue("providerID")); err != nil {
		return badRequest(err.Error())
	}
	writeTrue(w)
	return nil
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	writeJSON(w, 200, mergedConfig(in.Directory))
	return nil
}

func (s *Server) patchConfig(w http.ResponseWriter, r *http.Request) *apiError {
	var body map[string]any
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	path := filepath.Join(store.ConfigDir(), "intellexor.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	existing := map[string]any{}
	_ = config.Global()
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal([]byte(config.StripJSONC(string(b))), &existing)
	}
	for k, v := range body {
		existing[k] = v
	}
	b, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return badRequest(err.Error())
	}
	config.InvalidateGlobal()
	writeJSON(w, 200, body)
	return nil
}

func (s *Server) getConfigProviders(w http.ResponseWriter, r *http.Request) *apiError {
	providersHandler(s)(w, r)
	return nil
}

func (s *Server) disposeInstance(w http.ResponseWriter, r *http.Request) *apiError {
	writeTrue(w)
	return nil
}

func (s *Server) vcsHandler(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	out := map[string]any{}
	if !isGitRepo(in.Directory) {
		writeJSON(w, 200, out)
		return nil
	}
	if b, err := git(in.Worktree, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		out["branch"] = strings.TrimSpace(b)
	}
	if b, err := git(in.Worktree, "rev-parse", "--abbrev-ref", "origin/HEAD"); err == nil {
		s := strings.TrimSpace(b)
		s = strings.TrimPrefix(s, "origin/")
		if s != "HEAD" && s != "" {
			out["default_branch"] = s
		}
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) vcsStatus(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	out := []map[string]any{}
	if !isGitRepo(in.Directory) {
		writeJSON(w, 200, out)
		return nil
	}
	b, _ := git(in.Worktree, "status", "--porcelain")
	for _, line := range strings.Split(strings.TrimSpace(b), "\n") {
		if len(line) < 4 {
			continue
		}
		x, y := line[0], line[2]
		status := "modified"
		if x == '?' && y == '?' {
			status = "added"
		} else if x == 'D' || y == 'D' {
			status = "deleted"
		} else if x == 'A' || y == 'A' {
			status = "added"
		}
		out = append(out, map[string]any{
			"file":      filepath.Join(in.Worktree, strings.TrimSpace(line[3:])),
			"additions": 0,
			"deletions": 0,
			"status":    status,
		})
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) vcsDiff(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	out := []map[string]any{}
	if !isGitRepo(in.Directory) {
		writeJSON(w, 200, out)
		return nil
	}
	ctx := r.URL.Query().Get("context")
	args := []string{"diff", "--unified=" + ctxOr(ctx)}
	if r.URL.Query().Get("mode") == "branch" {
		args = append(args, "HEAD...origin/HEAD")
	}
	b, _ := git(in.Worktree, args...)
	for _, f := range splitFileDiffs(b) {
		add, del := countDiff(f)
		out = append(out, map[string]any{"file": filepath.Join(in.Worktree, filepathFromPatch(f)), "patch": f, "additions": add, "deletions": del})
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) vcsDiffRaw(w http.ResponseWriter, r *http.Request) {
	in := s.Instance(directoryOf(r))
	b, _ := git(in.Worktree, "diff")
	w.Header().Set("Content-Type", "text/x-diff; charset=utf-8")
	_, _ = w.Write([]byte(b))
}

func ctxOr(v string) string {
	if v == "" {
		return "3"
	}
	return v
}

func isGitRepo(dir string) bool {
	cur := dir
	for {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return true
		}
		p := filepath.Dir(cur)
		if p == cur {
			return false
		}
		cur = p
	}
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

func splitFileDiffs(diff string) []string {
	var files []string
	var cur []string
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git") && len(cur) > 0 {
			files = append(files, strings.Join(cur, "\n"))
			cur = nil
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 && strings.TrimSpace(strings.Join(cur, "\n")) != "" {
		files = append(files, strings.Join(cur, "\n"))
	}
	return files
}

func countDiff(patch string) (int, int) {
	add, del := 0, 0
	for _, l := range strings.Split(patch, "\n") {
		if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
			add++
		}
		if strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---") {
			del++
		}
	}
	return add, del
}

func filepathFromPatch(patch string) string {
	for _, l := range strings.Split(patch, "\n") {
		if strings.HasPrefix(l, "+++ ") {
			p := strings.TrimSpace(strings.TrimPrefix(l, "+++ "))
			p = strings.TrimPrefix(p, "b/")
			return p
		}
	}
	return ""
}

func (s *Server) listMCP(w http.ResponseWriter, r *http.Request) *apiError {
	writeJSON(w, 200, map[string]any{})
	return nil
}

func (s *Server) okTrue(w http.ResponseWriter, r *http.Request) *apiError {
	writeTrue(w)
	return nil
}

func (s *Server) fileContent(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	p := r.URL.Query().Get("path")
	if p == "" {
		return badRequest("path is required")
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(in.Directory, p)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return notFound("File not found: " + abs)
	}
	content := string(b)
	writeJSON(w, 200, map[string]any{"type": "text", "content": content})
	return nil
}

func (s *Server) fileHandler(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	p := r.URL.Query().Get("path")
	if p == "" {
		p = in.Directory
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return notFound("Directory not found: " + p)
	}
	var out []map[string]any
	for _, e := range ents {
		typ := "file"
		if e.IsDir() {
			typ = "directory"
		}
		ignored := e.Name() == ".git" || e.Name() == "node_modules"
		out = append(out, map[string]any{
			"name": e.Name(), "path": filepath.Join(p, e.Name()), "absolute": filepath.Join(p, e.Name()),
			"type": typ, "ignored": ignored,
		})
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, 200, out)
	return nil
}
