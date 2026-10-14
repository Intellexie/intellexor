package provider

import (
	"io"
	"net/http"
	"time"
)

type httpFetchClient struct {
	timeout time.Duration
}

func (h *httpFetchClient) get(url string) ([]byte, error) {
	cl := &http.Client{Timeout: h.timeout}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "intellexor")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}
