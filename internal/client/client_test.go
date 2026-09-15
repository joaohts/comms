package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func localServer(t *testing.T, h http.Handler) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "cc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: h}
	go server.Serve(listener)
	c := New(sock)
	t.Cleanup(func() { c.Close(); server.Close() })
	return c
}

func TestJSONRequestAndStableAPIError(t *testing.T) {
	c := localServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/failure" {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"code":"identity_attached","error":"already attached"}`)
			return
		}
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("wrong method/content type")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(body)
	}))
	var reply map[string]string
	if err := c.Do(context.Background(), "POST", "/echo", map[string]string{"body": "Olá\n$() `literal`"}, &reply); err != nil {
		t.Fatal(err)
	}
	if reply["body"] != "Olá\n$() `literal`" {
		t.Fatal(reply)
	}
	err := c.Do(context.Background(), "GET", "/failure", nil, nil)
	var api *Error
	if !errors.As(err, &api) || api.Code != "identity_attached" || api.Status != 409 {
		t.Fatalf("wrong API error: %#v", err)
	}
}

func TestStreamCancellationAndNDJSON(t *testing.T) {
	c := localServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "\n{\"type\":\"heartbeat\"}\n{\"type\":\"message\"}\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	count := 0
	err := c.Stream(ctx, "/stream", func(raw json.RawMessage) error {
		count++
		if count == 2 {
			cancel()
		}
		return nil
	})
	if count != 2 {
		t.Fatalf("got %d events", count)
	}
	if err == nil {
		t.Fatal("expected stream cancellation")
	}
}

func TestMissingSocketIsActionable(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "missing.sock"))
	defer c.Close()
	err := c.Do(context.Background(), "GET", "/v1/status", nil, nil)
	var api *Error
	if !errors.As(err, &api) || api.Code != "node_unavailable" {
		t.Fatalf("wrong error: %v", err)
	}
}
