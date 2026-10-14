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

func postSSE(ctx context.Context, url string, headers map[string]string, body any, handler func(event string, data []byte) (bool, error)) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
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
		if ctx.Err() != nil {
			return err
		}
		return &httpError{status: 0, body: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &httpError{status: resp.StatusCode, body: strings.TrimSpace(string(bb))}
	}
	scanner := bufio.NewScanner(resp.Body)
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
	_, err = flush()
	return err
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
