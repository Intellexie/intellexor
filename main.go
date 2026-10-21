package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	// Embed the IANA tz database so time.LoadLocation resolves device
	// timezones (X-Timezone) even in scratch/distroless containers that
	// ship no /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/Intellexie/intellexor/internal/acp"
	"github.com/Intellexie/intellexor/internal/config"
	"github.com/Intellexie/intellexor/internal/server"
	"github.com/Intellexie/intellexor/internal/session"
)

func main() {
	serveMain(os.Args[1:])
}

func serveMain(args []string) {
	fs := flag.NewFlagSet("intellexor", flag.ExitOnError)
	host := fs.String("host", "127.0.0.1", "host to bind")
	port := fs.Int("port", 4096, "port to bind (0 = random)")
	dir := fs.String("dir", "", "working directory (defaults to cwd)")
	quiet := fs.Bool("quiet", false, "suppress startup output")
	headless := fs.Bool("headless", false, "run as a plain server without the desktop window")
	fs.BoolVar(headless, "h", false, "alias for -headless")
	fs.Parse(args)
	if fs.NArg() > 0 {
		// e.g. the removed "acp" subcommand — fail loudly instead of
		// silently serving with defaults.
		fmt.Fprintf(os.Stderr, "intellexor: unexpected argument %q (ACP is always on; there is no acp subcommand)\n", fs.Arg(0))
		os.Exit(2)
	}

	wd, _ := os.Getwd()
	if *dir != "" {
		wd = *dir
	}
	// Eager load so broken config files are reported at startup, not on the
	// first request (instances are created lazily).
	_ = config.Merge(wd)

	srv := server.New(wd, session.Version)

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
		fmt.Printf("  acp  %s (web UI + ACP on the same port)\n", url)
	}

	if *headless {
		serveForever(srv, wd, ln)
		return
	}
	runDesktop(desktopEnv{srv: srv, ln: ln, wd: wd, quiet: *quiet})
}

// serveForever runs the multiplexed server — web UI/API and ACP share one
// socket, connections are classified by the first byte — until the
// listener closes.
func serveForever(srv *server.Server, wd string, ln net.Listener) {
	if err := acp.Multiplex(srv, wd, ln); err != nil && !errors.Is(err, net.ErrClosed) {
		fmt.Fprintln(os.Stderr, "server error:", err)
		os.Exit(1)
	}
}

// desktopEnv carries what the desktop window needs to run alongside the
// multiplexed server.
type desktopEnv struct {
	srv   *server.Server
	ln    net.Listener
	wd    string
	quiet bool
}

func listen(host string, port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err == nil || port != 4096 {
		return ln, err
	}
	// 4096 busy: fall back to a free port like opencode does
	return net.Listen("tcp", host+":0")
}
