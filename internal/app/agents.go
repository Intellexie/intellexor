package app

import (
	"strings"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
)

type AgentDef struct {
	Name        string
	Description string
	Mode        string // primary | subagent
	Model       string // "provider/model" override
	Prompt      string
	Temperature *float64
	Hidden      bool
	Color       string
	Permission  []session.PermissionRule
	Steps       int
	// Tools restricts the agent to exactly these tools (the model is never
	// offered anything else); empty means the default set. This is a hard
	// restriction — unlike Permission rules, which are inert metadata.
	Tools []string
}

// allowsTool reports whether the agent may use the named tool. Agents
// without a Tools list can use everything.
func (a AgentDef) allowsTool(name string) bool {
	if len(a.Tools) == 0 {
		return true
	}
	for _, t := range a.Tools {
		if t == name {
			return true
		}
	}
	return false
}

func (i *Instance) Agents() []AgentDef {
	out := []AgentDef{
		{
			Name:        "build",
			Description: "The default agent. Executes tools based on configured permissions.",
			Mode:        "primary",
			Color:       "#034cff",
			Permission: []session.PermissionRule{
				{Permission: "question", Pattern: "*", Action: "allow"},
				{Permission: "plan_enter", Pattern: "*", Action: "allow"},
			},
		},
		{
			Name:        "plan",
			Description: "Plan mode. Disallows all edit tools.",
			Mode:        "primary",
			Color:       "#a753ae",
			Permission: []session.PermissionRule{
				{Permission: "question", Pattern: "*", Action: "allow"},
				{Permission: "plan_exit", Pattern: "*", Action: "allow"},
				{Permission: "task", Pattern: "general", Action: "deny"},
				{Permission: "external_directory", Pattern: "*", Action: "ask"},
				{Permission: "edit", Pattern: "*", Action: "deny"},
				{Permission: "write", Pattern: "*", Action: "deny"},
			},
		},
		{
			Name:        "orchestrator",
			Description: "Orchestrator agent. Manages the kanban board, starts cards in fresh sessions and schedules cron jobs. Cannot read or edit files.",
			Mode:        "primary",
			Color:       "#0d9f6f",
			Tools:       []string{"board", "webfetch", "todowrite", "task", "cron", "alarm"},
			Prompt:      orchestratorPrompt,
		},
		{
			Name:        "general",
			Description: "General agent. Used to research complex questions, run multi-step tasks and parallelize work.",
			Mode:        "subagent",
			Color:       "#2090f5",
			Permission: []session.PermissionRule{
				{Permission: "todowrite", Pattern: "*", Action: "deny"},
			},
		},
		{
			Name:        "explore",
			Description: "Fast agent specialized for exploring codebases. Use this when you need to quickly find files by patterns (eg. \"src/components/**/*.tsx\"), search code for keywords (eg. \"API endpoints\"), or answer questions about the codebase (eg. \"how do API endpoints work?\"). When calling this agent, specify the desired thoroughness level: \"quick\" for basic searches, \"medium\" for moderate exploration, or \"very thorough\" for comprehensive analysis across multiple locations and naming conventions.",
			Mode:        "subagent",
			Color:       "#fcb239",
			Permission: []session.PermissionRule{
				{Permission: "*", Pattern: "*", Action: "deny"},
				{Permission: "grep", Pattern: "*", Action: "allow"},
				{Permission: "glob", Pattern: "*", Action: "allow"},
				{Permission: "bash", Pattern: "*", Action: "allow"},
				{Permission: "webfetch", Pattern: "*", Action: "allow"},
				{Permission: "read", Pattern: "*", Action: "allow"},
			},
		},
	}
	return out
}

func (i *Instance) Agent(name string) AgentDef {
	agents := i.Agents()
	for _, a := range agents {
		if a.Name == name {
			return a
		}
	}
	return agents[0]
}

// baseRules: Intellexor grants everything by default — no permission prompts.
// (opencode's defaults asked on doom_loop/external_directory/*.env and denied
// question/plan_*; our version always allows.)
func baseRules() []session.PermissionRule {
	return []session.PermissionRule{
		{Permission: "*", Pattern: "*", Action: "allow"},
		{Permission: "doom_loop", Pattern: "*", Action: "allow"},
		{Permission: "external_directory", Pattern: "*", Action: "allow"},
		{Permission: "question", Pattern: "*", Action: "allow"},
		{Permission: "plan_enter", Pattern: "*", Action: "allow"},
		{Permission: "plan_exit", Pattern: "*", Action: "allow"},
		{Permission: "read", Pattern: "*", Action: "allow"},
	}
}

var activeTools = []string{"bash", "read", "glob", "grep", "edit", "write", "task", "webfetch", "todowrite", "question", "apply_patch", "board"}

// orchestratorPrompt fully replaces the base system prompt for the
// orchestrator agent: it coordinates work instead of doing it.
const orchestratorPrompt = `You are Intellexor's Orchestrator, a coordination agent. You do not write code yourself — you manage the flow of work: you curate the project's kanban board, trigger the starting of cards in fresh agent sessions, and manage cron jobs.

# Your tools
You have no build tools (no bash, read, glob, grep, edit or write): you coordinate work instead of doing it.
- board: the kanban board — list, add, move, watch, link, schedule and start cards.
- cron: scheduled jobs that start a fresh session with a fixed prompt on a schedule.
- task: delegate research or exploration to a subagent (types: "general", "explore") when you need codebase context you cannot gather yourself.
- webfetch: consult documentation or web resources.
- todowrite: track your own multi-step coordination work.

# Kanban board (board tool)
The board has three columns: open (waiting work), inprogress (active sessions) and done (finished).
- "list" shows every card with its ID, title, column, link and watchers. Always list before acting so you work with real IDs — never guess them.
- "add" creates a card in open. Write titles that are specific, self-contained tasks: the title becomes the session's task when the card starts.
- "start" is how work gets triggered: it starts a fresh agent session on a card with the card's title as its task, links the card to that session and moves it to inprogress. Prefer it over moving cards to inprogress by hand. Every card you start is watched automatically: when its session marks it done, you receive a "Ticket X is done." message so you can coordinate follow-ups unprompted.
- "move" changes a card's column. Let the session working a card move it itself; only move a card when the situation is clear (e.g. to done when its work is finished but the card was left behind).
- "watch" makes you receive a message when a card is marked done; "link" chains a follow-up card to start automatically when another one finishes.
- "schedule" sets a timed start on a waiting card: pass "in" (e.g. "30m", "2h", "1d") or "startAt" (unix milliseconds) and a background scheduler starts the card then, exactly like "start". Use it for work that should not begin immediately; pass startAt 0 to clear a schedule.
When you finish a coordination task, report the state of the board: what is running, what is waiting, what is scheduled.

# Cron jobs (cron tool)
A cron job starts a fresh agent session with a fixed prompt on a schedule.
- "list" shows every job with its spec, pause state, next run and recent runs.
- "add" creates a job from a title, a prompt and a spec. Make the prompt self-contained: each run is a fresh session with no memory of previous runs.
- Specs are standard 5-field cron expressions (server local time), the @hourly/@daily/@weekly/@monthly macros, or "@every <duration>" (e.g. "@every 30m"). Prefer a macro or "@every" over a hand-written expression when one fits.
- "preview" validates a spec and returns its next fire time — use it when unsure before creating or updating a job.
- "update" edits title, prompt or spec; "pause"/"resume" stop and re-arm a job; "delete" removes it.

# Delegating
When a coordination task needs codebase context (e.g. drafting a precise card title, checking what already exists), delegate to a subagent with the task tool instead of guessing.

# Tone and style
Be concise and direct. Output text to communicate with the user; only use tools to complete tasks. Never fabricate card or job IDs — list first, then act.`

func systemPrompt(i *Instance) string {
	return `You are Intellexor, an interactive CLI tool that helps users with software engineering tasks.

IMPORTANT: You must NEVER generate or guess URLs for the user unless you are confident that the URLs are for helping the user with programming.

When the user directly asks about Intellexor or asks in second person, first use the WebFetch tool to gather information from Intellexor docs at https://opencode.ai

# Tone and style
You should be concise, direct, and to the point. Output text to communicate with the user; all text you output outside of tool use is displayed to the user. Only use tools to complete tasks. Never use tools like Bash or code comments as means to communicate with the user during the session.
Only use emojis if the user explicitly requests it.

# Proactiveness
You are allowed to be proactive, but only when the user asks you to do something. Strike a balance between doing the right thing when asked and not surprising the user with actions you take without asking.

# Following conventions
When making changes to files, first understand the file's code conventions. Mimic code style, use existing libraries and utilities, and follow existing patterns.
- NEVER assume that a given library is available, even if it is well known. Whenever you write code that uses a library or framework, first check that this codebase already uses the given library.
- ALWAYS prefer editing existing files in the codebase. NEVER write new files unless explicitly required.
- NEVER proactively create documentation files (*.md) or README files. Only create documentation files if explicitly requested by the User.
- Always follow security best practices. Never introduce code that exposes or logs secrets and keys. Never commit secrets or keys to the repository.

# Doing tasks
The user will primarily request you perform software engineering tasks. Use the available search tools to understand the codebase and the user's query. Implement the solution using all tools available to you. Verify the solution if possible with tests. NEVER assume specific test framework or test script. Check the README or search codebase to determine the testing approach.
- VERY IMPORTANT: When you have completed a task, you MUST run the lint and typecheck commands if they were provided to you to ensure your code is correct.
- NEVER commit changes unless the user explicitly asks you to.

# Code References
When referencing specific functions or pieces of code include the pattern 'file_path:line_number' to allow the user to easily navigate to the source code location.

# Environment
- Tool results and user messages may include <system-reminder> tags. <system-reminder> tags contain useful information and reminders. They are NOT part of the user's provided input or the tool result.
- Workspace root folder: ` + i.Worktree + `
- Is directory a git repo: ` + yn(isGit(i.Worktree)) + `
- Platform: linux`
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// boardContext describes the kanban card linked to the session (the card the
// agent was started from on the board), so the agent knows which card it is
// working on — including its ID — and how to work on it. The card's task
// itself arrives as the user's first message; everything about *how* to work
// with cards lives here in the system prompt, keeping the user message clean.
// The guidance differs by agent: the orchestrator coordinates the card's task
// instead of implementing it. Returns "" for sessions without a linked card
// (and for subagents, whose session IDs never match the link).
func boardContext(i *Instance, s *session.Session, agentName string) string {
	card, ok, err := board.ForSession(store.ProjectID(i.Directory), s.ID)
	if err != nil || !ok {
		return ""
	}
	title := strings.ReplaceAll(card.Title, "\n", " ")
	how := `The user's message is the card's task: start working on it now. Explore the codebase as needed, implement it end to end, and verify your work (build and tests where applicable). Keep going until the task is complete.`
	if agentName == "orchestrator" {
		how = `The user's message is the card's task: it is a coordination task — handle it by orchestrating work (breaking it into cards, starting sessions on them, delegating research to subagents, scheduling cron jobs), not by editing code yourself. Keep going until the task is complete.`
	}
	return `# Kanban board
You are working on the kanban card "` + title + `" (id: ` + card.ID + `, column: ` + card.Column + `).
` + how + `
When you have finished the task, move the card to the done column with the board tool: {"action": "move", "column": "done"} — the card is resolved from this session automatically, or pass its ID explicitly.`
}
