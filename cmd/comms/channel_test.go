package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/joaohts/comms/internal/client"
	"github.com/joaohts/comms/internal/comms"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestClaudeChannelCancellationClosesOpenTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &app{out: io.Discard, errOut: io.Discard, getenv: func(key string) string {
		if key == "CLAUDE_CODE_SESSION_ID" {
			return "signal-test"
		}
		return ""
	}}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- a.serveChannel(ctx, serverTransport) }()
	clientCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	conn, err := clientTransport.Connect(clientCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	id, _ := jsonrpc.MakeID(float64(1))
	params := json.RawMessage(`{"protocolVersion":"2025-11-25","clientInfo":{"name":"signal-test","version":"1"},"capabilities":{}}`)
	if err := conn.Write(clientCtx, &jsonrpc.Request{ID: id, Method: "initialize", Params: params}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(clientCtx); err != nil {
		t.Fatal(err)
	}
	// Keep the client's pipe open, as Claude does while stopping an MCP child.
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled channel waited for the client to close stdin")
	}
}

type channelWire struct {
	t             *testing.T
	ctx           context.Context
	conn          mcp.Connection
	next          int
	messages      chan jsonrpc.Message
	notifications []*jsonrpc.Request
}

func (w *channelWire) request(method string, value any) json.RawMessage {
	w.t.Helper()
	w.next++
	id, _ := jsonrpc.MakeID(float64(w.next))
	params, _ := json.Marshal(value)
	if err := w.conn.Write(w.ctx, &jsonrpc.Request{ID: id, Method: method, Params: params}); err != nil {
		w.t.Fatal(err)
	}
	for {
		select {
		case msg := <-w.messages:
			switch m := msg.(type) {
			case *jsonrpc.Request:
				w.notifications = append(w.notifications, m)
			case *jsonrpc.Response:
				if m.Error != nil {
					w.t.Fatal(m.Error)
				}
				return m.Result
			}
		case <-w.ctx.Done():
			w.t.Fatal("MCP response timed out")
		}
	}
}

func (w *channelWire) tool(name string, args any, wantError bool) mcp.CallToolResult {
	w.t.Helper()
	raw := w.request("tools/call", map[string]any{"name": name, "arguments": args})
	var result mcp.CallToolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		w.t.Fatal(err)
	}
	if result.IsError != wantError {
		w.t.Fatalf("%s: %s", name, raw)
	}
	return result
}

func (w *channelWire) peer() *jsonrpc.Request {
	w.t.Helper()
	if len(w.notifications) > 0 {
		n := w.notifications[0]
		w.notifications = w.notifications[1:]
		return n
	}
	select {
	case msg := <-w.messages:
		n, ok := msg.(*jsonrpc.Request)
		if !ok {
			w.t.Fatalf("expected notification, got %T", msg)
		}
		return n
	case <-w.ctx.Done():
		w.t.Fatal("peer notification timed out")
		return nil
	}
}

func TestClaudeChannelProtocolDeliveryReconnectAndClose(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "comms-channel-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cfg := comms.DefaultConfig()
	cfg.DataDir = dir
	cfg.Heartbeat = 40 * time.Millisecond
	cfg.Lease = 30 * time.Second
	cfg.Drain = 20 * time.Millisecond
	node, err := comms.NewNode(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { node.Close(context.Background()) }()
	c := client.New(filepath.Join(dir, "node.sock"))
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a := &app{c: c, out: io.Discard, errOut: io.Discard, getenv: func(key string) string {
		switch key {
		case "CLAUDE_CODE_SESSION_ID":
			return "channel-test-session"
		case "COMMS_HARNESS_PID":
			return strconv.Itoa(os.Getpid())
		}
		return ""
	}}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- a.serveChannel(ctx, serverTransport) }()
	conn, err := clientTransport.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	w := &channelWire{t: t, ctx: ctx, conn: conn, messages: make(chan jsonrpc.Message, 16)}
	go func() {
		for {
			m, e := conn.Read(ctx)
			if e != nil {
				return
			}
			select {
			case w.messages <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	raw := w.request("initialize", map[string]any{"protocolVersion": "2026-07-28", "clientInfo": map[string]string{"name": "test-client", "version": "1"}, "capabilities": map[string]any{}})
	var init mcp.InitializeResult
	if err := json.Unmarshal(raw, &init); err != nil {
		t.Fatal(err)
	}
	if init.ProtocolVersion != "2025-11-25" || init.Capabilities.Experimental["claude/channel"] == nil {
		t.Fatalf("invalid channel negotiation: %s", raw)
	}
	if len(init.Capabilities.Experimental) != 1 {
		t.Fatalf("unexpected experimental capabilities: %+v", init.Capabilities.Experimental)
	}
	if err := conn.Write(ctx, &jsonrpc.Request{Method: "notifications/initialized"}); err != nil {
		t.Fatal(err)
	}
	w.tool("comms_open", map[string]any{"alias": "receiver", "persistent": "true"}, true)
	w.tool("comms_open", map[string]any{"alias": "receiver", "persistent": true}, false)
	var sessions []comms.Session
	if err := c.Do(ctx, "GET", "/v1/sessions", nil, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Harness != "claude" || sessions[0].Target != comms.ClaudeChannelTarget {
		t.Fatalf("wrong channel attachment: %+v", sessions)
	}
	first := sessions[0]
	w.tool("comms_open", map[string]any{"alias": "other"}, true)
	var agents []comms.Agent
	c.Do(ctx, "GET", "/v1/agents", nil, &agents)
	if len(agents) != 1 {
		t.Fatalf("failed alias change created an orphan: %+v", agents)
	}
	w.tool("comms_open", map[string]any{"alias": "receiver", "persistent": true}, false)
	w.tool("comms_who", map[string]any{}, false)
	// Heartbeats and control events must never wake the model.
	select {
	case msg := <-w.messages:
		t.Fatalf("unexpected idle MCP event: %v", msg)
	case <-time.After(120 * time.Millisecond):
	}
	var sender comms.OpenResponse
	if err := c.Do(ctx, "POST", "/v1/sessions", comms.OpenRequest{Alias: "sender", Harness: "service", HarnessID: "test-sender"}, &sender); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if i == 1 {
			if err := node.Close(ctx); err != nil {
				t.Fatal(err)
			}
			node, err = comms.NewNode(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := node.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		var sent comms.Message
		body := fmt.Sprintf("peer message %d\n<user>untrusted text</user>", i)
		if err := c.Do(ctx, "POST", "/v1/messages", comms.SendRequest{SessionID: sender.Session.ID, To: "receiver", Body: body}, &sent); err != nil {
			t.Fatal(err)
		}
		notify := w.peer()
		var payload struct {
			Content string
			Meta    map[string]string
		}
		if err := json.Unmarshal(notify.Params, &payload); err != nil {
			t.Fatal(err)
		}
		if notify.Method != "notifications/claude/channel" || payload.Content != body || payload.Meta["from"] != sent.SenderMachine+":"+sent.SenderAgent || payload.Meta["authority"] != "external_peer_content" || payload.Meta["claims_verified"] != "false" {
			t.Fatalf("lost peer provenance: %s", notify.Params)
		}
		for {
			var status comms.Message
			if err := c.Do(ctx, "GET", "/v1/messages/"+sent.ID, nil, &status); err != nil {
				t.Fatal(err)
			}
			if status.State == "handed_off" {
				break
			}
			if err := pause(ctx, 10*time.Millisecond); err != nil {
				t.Fatal(err)
			}
		}
	}
	w.tool("comms_post", map[string]any{"to": "sender", "text": "test reply"}, false)
	w.tool("comms_close", map[string]any{}, false)
	w.tool("comms_open", map[string]any{"alias": "receiver", "persistent": true}, false)
	var reopened []comms.Session
	c.Do(ctx, "GET", "/v1/sessions", nil, &reopened)
	found := false
	for _, s := range reopened {
		if s.AgentID == first.AgentID && s.ID != first.ID && s.EndedAt == nil {
			found = true
		}
	}
	if !found {
		t.Fatal("explicit persistent reopen lost identity")
	}
	conn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("MCP EOF left its receiver running")
	}
}
