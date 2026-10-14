package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type BashTool struct{}

func (BashTool) Name() string { return "bash" }

func (BashTool) Description() string {
	return `Executes a given bash command in a persistent shell session with optional timeout, ensuring proper handling and security measures.

All commands run in the current working directory by default. Use the workdir parameter if you need to run a command in a different directory.

Before using this tool, it is recommended to use other specialized tools whenever they are more direct and appropriate for the task.

Usage notes:
  - The command argument is required.
  - You can specify an optional timeout in milliseconds. If not specified, commands will time out after 120000ms (2 minutes).
  - If the output exceeds 2000 lines or 50KB, it will be truncated and the full output will be written to a file.

Avoid using this tool with find, grep, cat, head, or tail commands, unless explicitly instructed or when these commands are truly necessary for the task. Instead, always prefer using the dedicated tools for these commands.`
}

func (BashTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "The command to execute"},
			"timeout": map[string]any{"type": "number", "description": "Optional timeout in milliseconds"},
			"workdir": map[string]any{"type": "string", "description": "The working directory to run the command in. Defaults to the current directory."},
		},
		"required": []any{"command"},
	}
}

const maxMetadataLength = 30_000

func splitCommands(command string) []string {
	var parts []string
	cur := strings.Builder{}
	inSingle, inDouble := false, false
	for i := 0; i < len(command); i++ {
		c := command[i]
		if c == '\'' && !inDouble {
			inSingle = !inSingle
		} else if c == '"' && !inSingle {
			inDouble = !inDouble
		}
		if !inSingle && !inDouble {
			if c == '&' && i+1 < len(command) && command[i+1] == '&' {
				parts = append(parts, cur.String())
				cur.Reset()
				i++
				continue
			}
			if c == '|' && i+1 < len(command) && command[i+1] == '|' {
				parts = append(parts, cur.String())
				cur.Reset()
				i++
				continue
			}
			if c == ';' || c == '|' {
				parts = append(parts, cur.String())
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(c)
	}
	parts = append(parts, cur.String())
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

var arity = map[string]int{"npm": 3, "pnpm": 3, "yarn": 3, "bun": 3, "docker": 2, "git": 2, "cargo": 2, "go": 2, "mix": 2, "brew": 2, "pip": 2, "poetry": 2, "uv": 2, "dotnet": 2, "yadm": 2}

func alwaysPattern(command string) []string {
	var out []string
	for _, cmd := range splitCommands(command) {
		fields := strings.Fields(cmd)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "cd" {
			continue
		}
		n := 1
		if v, ok := arity[fields[0]]; ok && len(fields) >= v {
			n = v
		}
		prefix := strings.Join(fields[:n], " ")
		out = append(out, prefix+" *")
	}
	return out
}

func (BashTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		Command string `json:"command"`
		Timeout int64  `json:"timeout"`
		Workdir string `json:"workdir"`
	}
	_ = json.Unmarshal(args, &p)
	if p.Command == "" {
		return Result{}, errors.New("command is required")
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 120_000
	}
	if c.Ask != nil {
		if err := c.Ask(AskInput{Permission: "bash", Patterns: splitCommands(p.Command), Always: alwaysPattern(p.Command), Metadata: map[string]any{"command": p.Command}}); err != nil {
			return Result{}, err
		}
	}

	workdir := c.Directory
	if p.Workdir != "" {
		workdir = p.Workdir
	}
	shell := "/bin/bash"

	execCtx, cancel := context.WithTimeout(c.Ctx, time.Duration(timeout+100)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(execCtx, shell, "-c", p.Command)
	cmd.Dir = workdir
	// Own process group so abort/timeout can kill the whole tree. Without it, a
	// command that spawns background children (dev servers, test watchers,
	// `... &`) keeps the stdout pipe open after the shell is killed and
	// cmd.Wait blocks on pipe EOF forever — the session would stay busy and
	// abort would appear to do nothing.
	setPgroup(cmd)

	var mu sync.Mutex
	var buf bytes.Buffer
	var cut bool
	const keepMax = MaxBytes * 2

	tickDone := make(chan struct{})
	preview := ""
	if c.Meta != nil {
		go func() {
			t := time.NewTicker(300 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-tickDone:
					return
				case <-t.C:
					mu.Lock()
					p := buf.String()
					if len(p) > maxMetadataLength {
						p = "...\n\n" + p[len(p)-maxMetadataLength:]
					}
					preview = p
					mu.Unlock()
					c.Meta("", map[string]any{"output": preview})
				}
			}
		}()
	}

	stdout := &syncWriter{mu: &mu, buf: &buf, cut: &cut, keepMax: keepMax}
	cmd.Stdout = stdout
	cmd.Stderr = stdout

	if err := cmd.Start(); err != nil {
		close(tickDone)
		return Result{Title: p.Command, Metadata: map[string]any{"exit": 1}, Output: "(no output)"}, nil
	}
	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	var err error
	select {
	case err = <-doneCh:
	case <-execCtx.Done():
		// abort or timeout: kill the entire process group. The direct child
		// alone is not enough — backgrounded descendants inherit the stdout
		// pipe and would block Wait on EOF indefinitely. If a straggler still
		// holds it after the kill, give up waiting so the agent loop (and any
		// abort) can always make progress.
		if cmd.Process != nil {
			killTree(cmd)
		}
		select {
		case err = <-doneCh:
		case <-time.After(2 * time.Second):
			err = fmt.Errorf("command did not exit after process-group kill")
		}
	}
	close(tickDone)

	mu.Lock()
	output := buf.String()
	truncated := cut
	mu.Unlock()

	var exit *int
	if cmd.ProcessState != nil {
		e := cmd.ProcessState.ExitCode()
		exit = &e
	}

	outStr := tailTrim(output)
	if strings.TrimSpace(outStr) == "" {
		outStr = "(no output)"
	}
	if truncated {
		outStr = "...output truncated...\n\n" + outStr
	}
	aborted := c.Ctx.Err() != nil
	timedOut := execCtx.Err() == context.DeadlineExceeded && !aborted
	if aborted {
		outStr += "\n\nUser aborted the command"
	} else if timedOut {
		outStr += "\n\n<shell_metadata>\nshell tool terminated command after exceeding timeout " + itoaB(timeout) + " ms. Command exceeded its timeout and was terminated. If this command requires more time to complete, retry with a larger timeout value in milliseconds.\n</shell_metadata>"
	}

	md := map[string]any{"output": preview}
	if preview == "" {
		p := outStr
		if len(p) > maxMetadataLength {
			p = p[len(p)-maxMetadataLength:]
		}
		md["output"] = p
	}
	if exit != nil {
		md["exit"] = *exit
	} else if err != nil {
		md["exit"] = 1
	}
	if truncated {
		md["truncated"] = true
	}
	if err != nil && !aborted && !timedOut && exit != nil && *exit != 0 {
		// non-zero exit still returns output; error string surfaces exit
		return Result{Title: p.Command, Metadata: md, Output: outStr + "\n\nExit code: " + itoaB(int64(*exit))}, nil
	}
	return Result{Title: p.Command, Metadata: md, Output: outStr}, nil
}

func itoaB(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

type syncWriter struct {
	mu      *sync.Mutex
	buf     *bytes.Buffer
	cut     *bool
	keepMax int
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if w.buf.Len() > w.keepMax {
		drop := w.buf.Len() - w.keepMax
		b := w.buf.Bytes()
		nb := make([]byte, len(b)-drop)
		copy(nb, b[drop:])
		w.buf.Reset()
		w.buf.Write(nb)
		*w.cut = true
	}
	return len(p), nil
}

func tailTrim(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > MaxLines {
		lines = lines[len(lines)-MaxLines:]
		s = strings.Join(lines, "\n")
	}
	if len(s) > MaxBytes {
		s = s[len(s)-MaxBytes:]
	}
	return s
}
