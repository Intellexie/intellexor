//go:build !desktop

package main

import (
	"fmt"
	"os"
)

// runDesktop is compiled out of plain builds (no -tags desktop): behave
// exactly like --headless so scripts, containers and tests keep working.
func runDesktop(env desktopEnv) {
	if !env.quiet {
		fmt.Fprintln(os.Stderr, "note: built without desktop support (go build -tags \"desktop,production\"); running headless")
	}
	serveForever(env.srv, env.wd, env.ln)
}
