// Package client speaks to the owner-only Unix-socket comms API.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"error"`
	Status  int    `json:"-"`
}

func (e *Error) Error() string { return e.Message }

type Client struct {
	Socket string
	http   *http.Client
}

func New(socket string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return &Client{Socket: socket, http: &http.Client{Transport: transport}}
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var data io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		data = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://local"+path, data)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Code: "node_unavailable", Message: fmt.Sprintf("cannot reach comms node at %s: %v; start it with comms serve or install the user service", c.Socket, err)}
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return res, nil
	}
	defer res.Body.Close()
	var api Error
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&api); err != nil || api.Code == "" {
		api.Code = "http_error"
		api.Message = fmt.Sprintf("local API returned HTTP %d", res.StatusCode)
	}
	api.Status = res.StatusCode
	return nil, &api
}

func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<20))
		return err
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out); errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// Stream leaves reconnection and acknowledgment policy to the caller. The
// callback returns only after it has handled the complete JSON event.
func (c *Client) Stream(ctx context.Context, path string, event func(json.RawMessage) error) error {
	res, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	scan := bufio.NewScanner(res.Body)
	scan.Buffer(make([]byte, 4096), 3<<20)
	for scan.Scan() {
		line := bytes.TrimSpace(scan.Bytes())
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			return &Error{Code: "bad_stream", Message: "node emitted an invalid JSON event"}
		}
		if err := event(append(json.RawMessage(nil), line...)); err != nil {
			return err
		}
	}
	if err := scan.Err(); err != nil {
		return err
	}
	return io.EOF
}
