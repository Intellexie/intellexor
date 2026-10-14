package app

import "intellexor/internal/session"

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

var activeTools = []string{"bash", "read", "glob", "grep", "edit", "write", "task", "webfetch", "todowrite", "question", "apply_patch"}

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
