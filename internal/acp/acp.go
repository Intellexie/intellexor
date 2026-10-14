// Package acp implements the Agent Client Protocol (ACP) agent surface of
// opencode: JSON-RPC 2.0, newline-delimited frames. Transport is stdio by
// default (like `opencode acp`) or TCP when --port is given (docker mode).
package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"intellexor/internal/server"
	"intellexor/internal/session"
)

// ---------------- JSON-RPC framing ----------------

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ---------------- ACP param shapes ----------------

type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
}

type conn struct {
	srv *server.Server

	wmu sync.Mutex
	w   io.Writer

	pmu     sync.Mutex
	pending map[int64]chan *rpcMsg
	nextID  int64

	smu      sync.Mutex
	sessions map[string]*session.Session
	cwd      string

	closedMu sync.Mutex
	closed   bool
	closeCh  chan struct{}
}

func newConn(srv *server.Server, cwd string, w io.Writer) *conn {
	return &conn{srv: srv, w: w, pending: map[int64]chan *rpcMsg{}, sessions: map[string]*session.Session{}, cwd: cwd, closeCh: make(chan struct{})}
}

func (c *conn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return err
	}
	if f, ok := c.w.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
	return nil
}

func (c *conn) isClosed() bool {
	c.closedMu.Lock()
	defer c.closedMu.Unlock()
	return c.closed
}

// Notify sends an agent->client notification (no id).
func (c *conn) Notify(method string, params any) {
	if c.isClosed() {
		return
	}
	_ = c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// Call sends an agent->client request and awaits the response.
func (c *conn) Call(method string, params any) (json.RawMessage, *rpcError) {
	c.pmu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan *rpcMsg, 1)
	c.pending[id] = ch
	c.pmu.Unlock()

	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	select {
	case <-c.closeCh:
		return nil, &rpcError{Code: -32000, Message: "connection closed"}
	case msg := <-ch:
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	}
}

// ---------------- serving ----------------

// Serve runs the ACP agent. With port <= 0 it speaks newline-delimited
// JSON-RPC over stdio (standard ACP transport). With a port it listens on
// TCP and multiplexes on the same socket like opencode: HTTP requests
// (browsers) are handled by the web UI / API server, anything else
// (ACP clients) is treated as an ndjson JSON-RPC stream.
func Serve(srv *server.Server, cwd string, host string, port int) error {
	if port <= 0 {
		c := newConn(srv, cwd, os.Stdout)
		c.loop(os.Stdin)
		return nil
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Intellexor acp listening on %s (cwd %s) — web UI + ACP on the same port\n", ln.Addr(), cwd)

	httpSrv := &http.Server{Handler: srv.Handler()}
	q := &connQueue{ch: make(chan net.Conn), addr: ln.Addr()}
	go httpSrv.Serve(q)

	for {
		nc, err := ln.Accept()
		if err != nil {
			return err
		}
		go func(nc net.Conn) {
			br := bufio.NewReader(nc)
			// sniff the first byte: JSON-RPC frames start with '{'
			// (possibly after whitespace); HTTP request lines start with
			// a method name like GET/POST/PUT/...
			nc.SetReadDeadline(time.Now().Add(30 * time.Second))
			b, err := br.Peek(1)
			nc.SetReadDeadline(time.Time{})
			if err != nil {
				nc.Close()
				return
			}
			switch b[0] {
			case '{', ' ', '\t', '\r', '\n':
				c := newConn(srv, cwd, nc)
				c.loop(br)
				nc.Close()
			default:
				// http.Server takes ownership (and closes) the conn;
				// wrap so the peeked byte isn't lost
				select {
				case q.ch <- &peekedConn{Conn: nc, r: br}:
				case <-time.After(30 * time.Second):
					nc.Close()
				}
			}
		}(nc)
	}
}

// peekedConn serves the buffered peek bytes (plus the rest of the stream)
// to the HTTP server.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// connQueue is a net.Listener that hands pre-classified HTTP conns to
// http.Server.
type connQueue struct {
	ch     chan net.Conn
	addr   net.Addr
	closed sync.Once
	done   chan struct{}
}

func (l *connQueue) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connQueue) Close() error {
	l.closed.Do(func() { close(l.done) })
	return nil
}

func (l *connQueue) Addr() net.Addr { return l.addr }

func (c *conn) loop(r io.Reader) {
	defer c.shutdown()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg rpcMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			_ = c.write(map[string]any{"jsonrpc": "2.0", "id": nil, "error": &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if msg.Method == "" {
			// response to our call
			if msg.ID != nil {
				c.pmu.Lock()
				ch, ok := c.pending[*msg.ID]
				delete(c.pending, *msg.ID)
				c.pmu.Unlock()
				if ok {
					m := msg
					ch <- &m
				}
			}
			continue
		}
		if msg.ID == nil {
			// notification from client
			c.notification(msg.Method, msg.Params)
			continue
		}
		go func(msg rpcMsg) {
			result, rerr := c.request(msg.Method, msg.Params)
			resp := map[string]any{"jsonrpc": "2.0", "id": *msg.ID}
			if rerr != nil {
				resp["error"] = rerr
			} else {
				resp["result"] = result
			}
			_ = c.write(resp)
		}(msg)
	}
}

func (c *conn) shutdown() {
	c.closedMu.Lock()
	c.closed = true
	c.closedMu.Unlock()
	c.pmu.Lock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.pmu.Unlock()
	close(c.closeCh)
}

func (c *conn) notification(method string, params json.RawMessage) {
	switch method {
	case "session/cancel":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(params, &p)
		if sess, ok := c.sessionByID(p.SessionID); ok {
			c.srv.Instance(sess.Directory).Abort(sess.ID)
		}
	case "cancel":
		// whole-connection cancel: abort all sessions of this connection
		c.smu.Lock()
		list := make([]*session.Session, 0, len(c.sessions))
		for _, s := range c.sessions {
			list = append(list, s)
		}
		c.smu.Unlock()
		for _, s := range list {
			c.srv.Instance(s.Directory).Abort(s.ID)
		}
	}
}

func (c *conn) sessionByID(id string) (*session.Session, bool) {
	c.smu.Lock()
	s, ok := c.sessions[id]
	c.smu.Unlock()
	if ok {
		return s, true
	}
	// maybe created in another connection (load/resume)
	if s, err := c.srv.FindSession(id); err == nil {
		c.smu.Lock()
		c.sessions[s.ID] = s
		c.smu.Unlock()
		return s, true
	}
	return nil, false
}
