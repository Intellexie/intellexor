package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"

	"intellexor/internal/acp"
	"intellexor/internal/server"
	"intellexor/internal/session"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "acp" {
		acpMain(os.Args[2:])
		return
	}
	serveMain()
}

func serveMain() {
	host := flag.String("host", "127.0.0.1", "host to bind")
	port := flag.Int("port", 4096, "port to bind (0 = random)")
	dir := flag.String("dir", "", "working directory (defaults to cwd)")
	quiet := flag.Bool("quiet", false, "suppress startup output")
	flag.Parse()

	wd, _ := os.Getwd()
	if *dir != "" {
		wd = *dir
	}

	srv := server.New(wd, session.Version)
	httpSrv := &http.Server{Handler: srv.Handler()}

	ln, err := listen(*host, *port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to listen:", err)
		os.Exit(1)
	}
	url := fmt.Sprintf("http://%s/", ln.Addr().String())
	if !*quiet {
		fmt.Printf("Intellexor serving %s\n", wd)
		fmt.Printf("  web  %s\n", url)
		fmt.Printf("  api  %sdoc\n", url)
	}
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "server error:", err)
		os.Exit(1)
	}
}

// acpMain runs the ACP agent: ndjson JSON-RPC over stdio, or TCP when
// --port/--host/env PORT is set (docker mode).
func acpMain(args []string) {
	fs := flag.NewFlagSet("acp", flag.ExitOnError)
	host := fs.String("host", "127.0.0.1", "host to bind (TCP mode)")
	port := fs.Int("port", 0, "TCP port (0 = stdio; env ACP_PORT/PORT overrides)")
	dir := fs.String("cwd", "", "working directory (defaults to cwd)")
	fs.Parse(args)

	wd, _ := os.Getwd()
	if *dir != "" {
		wd = *dir
	}

	p := *port
	if p == 0 {
		if v := os.Getenv("ACP_PORT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				p = n
			}
		} else if v := os.Getenv("PORT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				p = n
			}
		}
	}
	h := *host
	if p > 0 && os.Getenv("ACP_HOST") != "" {
		h = os.Getenv("ACP_HOST")
	}

	if err := acp.Serve(server.New(wd, session.Version), wd, h, p); err != nil {
		fmt.Fprintln(os.Stderr, "acp error:", err)
		os.Exit(1)
	}
}

func listen(host string, port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err == nil || port != 4096 {
		return ln, err
	}
	// 4096 busy: fall back to a free port like opencode does
	return net.Listen("tcp", host+":0")
}
