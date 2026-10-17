package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

var httpClient = &http.Client{}

// streamIdleTimeout is how long a provider stream may make no progress (no
// response headers, no body bytes) before it is abandoned. Without it a hung
// proxy / vLLM / silently-dropped connection freezes the agent loop forever:
// the consumer blocks on the event channel, the tool part stays completed,
// the session stays "busy", and no error is ever surfaced. The window only
// needs to cover dead silence — any byte resets it — so slow generations and
// long queues are unaffected. Local runtimes (ollama/lmstudio) opt out
// because a cold model load looks exactly like silence.
var streamIdleTimeout = 3 * time.Minute

// stalledError builds the retryable error returned when the idle watchdog
// fires. A constructor (not a shared value) so tests can shorten the window
// and still get an accurate message.
func stalledError() *StreamError {
	return &StreamError{
		Message:     "provider stream stalled: no data received for " + streamIdleTimeout.String(),
		IsRetryable: true,
	}
}

// idleWatchdog cancels sctx when no progress is reported within idle.
// Progress is signalled by the caller: once when the response headers arrive
// and before every body read. idle <= 0 disables the watchdog.
func idleWatchdog(sctx context.Context, cancel context.CancelFunc, idle time.Duration) (progress func(), stop func()) {
	if idle <= 0 {
		return func() {}, func() {}
	}
	reset := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case <-reset:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idle)
			case <-timer.C:
				cancel()
				return
			case <-done:
				return
			}
		}
	}()
	progress = func() {
		select {
		case reset <- struct{}{}:
		default:
		}
	}
	stop = func() { close(done) }
	return progress, stop
}

func postSSE(ctx context.Context, url string, headers map[string]string, body any, idle time.Duration, handler func(event string, data []byte) (bool, error)) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	progress, stop := idleWatchdog(sctx, cancel, idle)
	defer stop()

	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(sctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		if sctx.Err() != nil && ctx.Err() == nil {
			return stalledError()
		}
		if ctx.Err() != nil {
			return err
		}
		return &httpError{status: 0, body: err.Error()}
	}
	defer resp.Body.Close()
	progress() // headers arrived — the body read window starts now
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &httpError{status: resp.StatusCode, body: strings.TrimSpace(string(bb))}
	}
	// every Scan blocks on the socket; re-arm the watchdog before each one so
	// a body that stops mid-stream is abandoned too
	scanner := bufio.NewScanner(&progressReader{r: resp.Body, progress: progress})
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var event string
	var dataLines [][]byte
	flush := func() (bool, error) {
		if len(dataLines) == 0 {
			return true, nil
		}
		data := bytes.Join(dataLines, []byte("\n"))
		dataLines = nil
		ev := event
		event = ""
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			return false, nil
		}
		return handler(ev, data)
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			cont, err := flush()
			if err != nil || !cont {
				return err
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			event = strings.TrimSpace(string(line[6:]))
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			dataLines = append(dataLines, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
			continue
		}
	}
	// the watchdog fired mid-body (or the connection died): scanner just
	// stops without an error of its own, so the stall must be checked here —
	// returning nil would make the producer emit a bogus finish event
	if serr := scanner.Err(); serr != nil {
		if sctx.Err() != nil && ctx.Err() == nil {
			return stalledError()
		}
		return serr
	}
	if sctx.Err() != nil && ctx.Err() == nil {
		return stalledError()
	}
	_, err = flush()
	return err
}

// progressReader re-arms the idle watchdog on every read attempt: a read that
// returns data counts as progress, and one that blocks past the window lets
// the watchdog cancel the request (which unblocks the read with an error).
type progressReader struct {
	r        io.Reader
	progress func()
}

func (p *progressReader) Read(b []byte) (int, error) {
	p.progress()
	n, err := p.r.Read(b)
	if n > 0 {
		p.progress()
	}
	return n, err
}

// non-streaming JSON POST
func postJSON(ctx context.Context, url string, headers map[string]string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c := &http.Client{Timeout: 60 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	bb, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return &httpError{status: resp.StatusCode, body: strings.TrimSpace(string(bb))}
	}
	if out != nil {
		return json.Unmarshal(bb, out)
	}
	return nil
}
