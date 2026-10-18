package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Intellexie/intellexor/internal/cron"
	"github.com/Intellexie/intellexor/internal/store"
)

// CronTool lets the agent manage the project's cron jobs: prompts that
// automatically start a fresh agent session on a schedule. Jobs are stored
// per project (keyed like the board and sessions) and every mutation is
// broadcast via OnUpdate so connected web clients refresh; the runs
// themselves are driven by the background sweep in internal/app.
type CronTool struct {
	// OnUpdate is called after every successful mutation so the server can
	// broadcast cron.updated to connected web clients.
	OnUpdate func()
}

func (CronTool) Name() string { return "cron" }

func (CronTool) Description() string {
	return `Manage the project's cron jobs: a job starts a fresh agent session with a fixed prompt on a schedule (each run appears as a normal session).

## When to use
- Use action "list" to see every job with its ID, spec, pause state, next run and recent runs.
- Use action "add" to schedule recurring work: a self-contained prompt that a fresh session receives on each run.
- Use action "preview" to validate a spec and get its next fire time before creating or updating a job.
- Use "update" to change a job's title, prompt or spec; "pause"/"resume" to stop and re-arm one; "delete" to remove it.

## Schedule specs
- Standard 5-field cron expression: minute hour day-of-month month day-of-week (server local time). Supports *, lists, ranges, steps and 3-letter month/day names (e.g. "30 9 * * Mon-Fri").
- Macros: @hourly, @daily, @midnight, @weekly, @monthly, @yearly, @annually.
- "@every <duration>" (e.g. "@every 30m", "@every 2h") fires relative to now.

## Actions
- "list": returns every job.
- "add": creates a job with "title", "prompt" and "spec". The prompt should be self-contained — each run starts a fresh session with no memory of previous runs.
- "update": edits job "id"; pass only the fields to change (title, prompt, spec). A new spec re-arms the job from now.
- "pause" / "resume": stop or re-arm job "id". Resuming does not fire immediately, even if the schedule came due while paused.
- "delete": removes job "id".
- "preview": validates "spec" and returns its next fire time.`
}

func (CronTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []any{"list", "add", "update", "pause", "resume", "delete", "preview"},
				"description": "The cron operation to perform",
			},
			"id":     map[string]any{"type": "string", "description": "Job ID (update, pause, resume, delete)"},
			"title":  map[string]any{"type": "string", "description": "Job title (add; optional for update)"},
			"prompt": map[string]any{"type": "string", "description": "Prompt the scheduled session receives (add; optional for update)"},
			"spec":   map[string]any{"type": "string", "description": "Schedule expression (add, update, preview)"},
		},
		"required": []any{"action"},
	}
}

func (t CronTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		Action string `json:"action"`
		ID     string `json:"id"`
		Title  string `json:"title"`
		Prompt string `json:"prompt"`
		Spec   string `json:"spec"`
	}
	_ = json.Unmarshal(args, &p)
	if c.Ask != nil {
		if err := c.Ask(AskInput{Permission: "cron", Patterns: []string{"*"}, Always: []string{"*"}, Metadata: map[string]any{}}); err != nil {
			return Result{}, err
		}
	}
	projectID := store.ProjectID(c.Directory)

	switch p.Action {
	case "list":
		jobs, err := cron.Get(projectID)
		if err != nil {
			return Result{}, err
		}
		out, _ := json.MarshalIndent(jobs, "", "  ")
		return Result{
			Title:    fmt.Sprintf("%d cron jobs", len(jobs)),
			Metadata: map[string]any{"jobs": jobs},
			Output:   string(out),
		}, nil

	case "add":
		if strings.TrimSpace(p.Prompt) == "" {
			return Result{}, errors.New("add requires a prompt (the instruction each scheduled run receives)")
		}
		if strings.TrimSpace(p.Spec) == "" {
			return Result{}, errors.New(`add requires a spec (e.g. "@daily", "0 9 * * Mon-Fri", "@every 30m")`)
		}
		title := strings.TrimSpace(p.Title)
		if title == "" {
			title = defaultJobTitle(p.Prompt)
		}
		job, err := cron.Add(projectID, title, p.Prompt, p.Spec)
		if err != nil {
			return Result{}, err
		}
		t.notify()
		return Result{
			Title:    "Cron job added: " + job.Title,
			Metadata: map[string]any{"job": job},
			Output:   fmt.Sprintf("Cron job %s (%q) created: runs %q, next run %s.", job.ID, job.Title, job.Spec, formatMillis(job.NextRun)),
		}, nil

	case "update":
		if p.ID == "" {
			return Result{}, errors.New("update requires a job ID (see action list)")
		}
		var title, prompt, spec *string
		if p.Title != "" {
			title = &p.Title
		}
		if p.Prompt != "" {
			prompt = &p.Prompt
		}
		if p.Spec != "" {
			spec = &p.Spec
		}
		if title == nil && prompt == nil && spec == nil {
			return Result{}, errors.New("nothing to update: pass title, prompt and/or spec")
		}
		job, err := cron.Update(projectID, p.ID, title, prompt, spec)
		if err == cron.ErrNotFound {
			return Result{}, fmt.Errorf("cron job not found: %s (use action list to see job IDs)", p.ID)
		}
		if err != nil {
			return Result{}, err
		}
		t.notify()
		return Result{
			Title:    "Cron job updated: " + job.Title,
			Metadata: map[string]any{"job": job},
			Output:   fmt.Sprintf("Cron job %s (%q) updated: runs %q, next run %s.", job.ID, job.Title, job.Spec, formatMillis(job.NextRun)),
		}, nil

	case "pause", "resume":
		if p.ID == "" {
			return Result{}, fmt.Errorf("%s requires a job ID (see action list)", p.Action)
		}
		job, err := cron.SetPaused(projectID, p.ID, p.Action == "pause")
		if err == cron.ErrNotFound {
			return Result{}, fmt.Errorf("cron job not found: %s (use action list to see job IDs)", p.ID)
		}
		if err != nil {
			return Result{}, err
		}
		t.notify()
		if p.Action == "pause" {
			return Result{
				Title:    "Paused " + job.Title,
				Metadata: map[string]any{"job": job},
				Output:   fmt.Sprintf("Cron job %s (%q) paused: no further runs until resumed.", job.ID, job.Title),
			}, nil
		}
		return Result{
			Title:    "Resumed " + job.Title,
			Metadata: map[string]any{"job": job},
			Output:   fmt.Sprintf("Cron job %s (%q) resumed: next run %s.", job.ID, job.Title, formatMillis(job.NextRun)),
		}, nil

	case "delete":
		if p.ID == "" {
			return Result{}, errors.New("delete requires a job ID (see action list)")
		}
		jobs, err := cron.Get(projectID)
		if err != nil {
			return Result{}, err
		}
		title := ""
		for _, j := range jobs {
			if j.ID == p.ID {
				title = j.Title
				break
			}
		}
		if err := cron.Delete(projectID, p.ID); err != nil {
			if err == cron.ErrNotFound {
				return Result{}, fmt.Errorf("cron job not found: %s (use action list to see job IDs)", p.ID)
			}
			return Result{}, err
		}
		t.notify()
		return Result{
			Title:    "Deleted " + title,
			Metadata: map[string]any{"deletedID": p.ID},
			Output:   fmt.Sprintf("Cron job %s (%q) deleted.", p.ID, title),
		}, nil

	case "preview":
		if strings.TrimSpace(p.Spec) == "" {
			return Result{}, errors.New("preview requires a spec")
		}
		next, err := cron.Next(p.Spec, time.Now())
		if err != nil {
			return Result{}, err
		}
		return Result{
			Title:    "Next run " + next.Format(time.RFC3339),
			Metadata: map[string]any{"next": next.UnixMilli()},
			Output:   fmt.Sprintf("Spec %q is valid; next fire time %s (unix ms: %d).", p.Spec, next.Format("2006-01-02 15:04"), next.UnixMilli()),
		}, nil

	default:
		return Result{}, fmt.Errorf("unknown action %q (valid: list, add, update, pause, resume, delete, preview)", p.Action)
	}
}

func (t CronTool) notify() {
	if t.OnUpdate != nil {
		t.OnUpdate()
	}
}

// defaultJobTitle derives a job title from the prompt when none is given:
// its first line, clipped — the same idea as the session title fallback.
func defaultJobTitle(prompt string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(prompt), "\n", 2)[0])
	r := []rune(line)
	if len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return line
}

// formatMillis renders a unix-milliseconds timestamp in server local time;
// 0 (never run / unscheduled) renders as a dash.
func formatMillis(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}
