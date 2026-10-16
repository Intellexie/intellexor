package server

import (
	"embed"
	"sort"

	"github.com/Intellexie/intellexor/internal/config"
)

// Built-in slash commands, mirroring opencode's Command.Service defaults:
// "init" and "review" always exist, with config-defined commands layered on top.

//go:embed templates
var builtinTemplates embed.FS

type builtinCommand struct {
	name        string
	description string
	template    string
}

func builtinCommands() map[string]builtinCommand {
	m := map[string]builtinCommand{}
	// "compact" has no template: sessionCommand runs it as an action
	// (session summarization), the other built-ins load their prompt file.
	for _, b := range []struct{ name, desc, file string }{
		{"init", "guided AGENTS.md setup", "templates/initialize.txt"},
		{"review", "review changes [commit|branch|pr], defaults to uncommitted", "templates/review.txt"},
		{"compact", "summarize the session to free context window", ""},
	} {
		if b.file == "" {
			m[b.name] = builtinCommand{name: b.name, description: b.desc}
			continue
		}
		raw, err := builtinTemplates.ReadFile(b.file)
		if err != nil {
			continue
		}
		m[b.name] = builtinCommand{name: b.name, description: b.desc, template: string(raw)}
	}
	return m
}

// resolveCommand returns the template + agent for a command name, preferring
// config-defined commands over built-ins.
func resolveCommand(name string, commands map[string]config.CommandCfg) (template, agent string, ok bool) {
	if cfg, found := commands[name]; found {
		return cfg.Template, cfg.Agent, cfg.Template != ""
	}
	if b, found := builtinCommands()[name]; found && b.template != "" {
		return b.template, "", true
	}
	return "", "", false
}

func sortedBuiltinNames() []string {
	names := make([]string, 0, len(builtinCommands()))
	for n := range builtinCommands() {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
