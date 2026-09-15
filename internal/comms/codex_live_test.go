package comms

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/joaohts/comms/internal/codex"
)

// TestNodeNativeCodexLive exercises the production local HTTP API, SQLite queue,
// scheduler, native adapter, and real model. CI never invokes a model implicitly.
func TestNodeNativeCodexLive(t *testing.T) {
	target, thread := os.Getenv("COMMS_CODEX_TEST_TARGET"), os.Getenv("COMMS_CODEX_TEST_THREAD")
	if target == "" || thread == "" {
		t.Skip("requires an explicitly owned live test target and thread")
	}
	if err := codex.ValidateSession(context.Background(), target, thread); err != nil {
		t.Fatal(err)
	}
	f := newNodeIntegration(t, func(c *Config) { c.Heartbeat = time.Second; c.Lease = time.Minute; c.Drain = 2 * time.Second })
	sender := f.open(t, "test-sender", "local", false)
	pid, _ := strconv.Atoi(os.Getenv("COMMS_CODEX_TEST_PID"))
	var recipient OpenResponse
	f.call(t, "POST", "/v1/sessions", OpenRequest{Alias: "test-codex", Harness: "codex", HarnessID: thread, Scope: "local", Target: target, ProcessID: pid, ProcessStarted: ProcessStamp(pid)}, 200, &recipient)
	nodeIntegrationEventually(t, "native receiver attached", func() bool { return f.node.ready(recipient.Agent.ID) })

	for pass := 0; pass < 2; pass++ {
		if pass == 1 {
			oldID := recipient.Agent.ID
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := f.node.Close(ctx)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			restarted, err := NewNode(f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			f.node = restarted
			if err := restarted.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			nodeIntegrationEventually(t, "native receiver restored after node restart", func() bool { return restarted.ready(oldID) })
			saved, err := restarted.Store.Session(recipient.Session.ID)
			if err != nil || saved.AgentID != oldID || saved.EndedAt != nil {
				t.Fatalf("attachment lost during restart: %+v %v", saved, err)
			}
		}
		marker := NewID("NODE_NATIVE_")
		body := marker + " Please acknowledge this harmless transport test marker and identify it as external peer tool output. Do not call tools."
		sent := f.send(t, sender.Session.ID, "test-codex", body, NewID("codex_live_"))
		got := f.waitState(t, sent.SenderMachine, sent.ID, "handed_off")
		if got.HandedOffAt == nil || got.Attempts != 1 {
			t.Fatalf("missing durable handoff: %+v", got)
		}
		awaitCodexLiveReply(t, target, thread, marker, sent)
		duplicate := f.send(t, sender.Session.ID, "test-codex", body, sent.ID)
		if duplicate.ID != sent.ID || duplicate.State != "handed_off" {
			t.Fatalf("retry lost dedup: %+v", duplicate)
		}
		t.Logf("pass=%d machine=%s sender_agent=%s recipient_agent=%s attachment=%s message=%s marker=%s thread=%s state=%s attempts=%d", pass, sent.SenderMachine, sent.SenderAgent, sent.RecipientAgent, recipient.Session.ID, sent.ID, marker, thread, got.State, got.Attempts)
	}
}

func awaitCodexLiveReply(t *testing.T, target, thread, marker string, message Message) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(target, "unix://"))
	}}
	defer tr.CloseIdleConnections()
	ws, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: tr}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(4 << 20)
	rpc := func(id int, method string, params any) json.RawMessage {
		request, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
		if err := ws.Write(ctx, websocket.MessageText, request); err != nil {
			t.Fatal(err)
		}
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var event struct {
				ID     int             `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(b, &event) != nil || event.Method != "" || event.ID != id {
				continue
			}
			if len(event.Error) > 0 {
				t.Fatalf("Codex read error: %s", event.Error)
			}
			return event.Result
		}
	}
	rpc(1, "initialize", map[string]any{"clientInfo": map[string]any{"name": "comms-node-live-test", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}})
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"method":"initialized"}`)); err != nil {
		t.Fatal(err)
	}
	for {
		raw := rpc(2, "thread/read", map[string]any{"threadId": thread, "includeTurns": true})
		var h struct {
			Thread struct {
				ID    string `json:"id"`
				Turns []struct {
					Items []struct {
						Type    string          `json:"type"`
						Name    string          `json:"name"`
						Output  string          `json:"output"`
						Text    string          `json:"text"`
						Content json.RawMessage `json:"content"`
					} `json:"items"`
				} `json:"turns"`
			} `json:"thread"`
		}
		if err := json.Unmarshal(raw, &h); err != nil {
			t.Fatal(err)
		}
		if h.Thread.ID != thread {
			t.Fatal("history belongs to wrong thread")
		}
		toolCount, reply := 0, false
		for _, turn := range h.Thread.Turns {
			for _, item := range turn.Items {
				if item.Type == "userMessage" && strings.Contains(string(item.Content), marker) {
					t.Fatal("peer message entered as user input")
				}
				if item.Type == "agentMessage" && strings.Contains(item.Text, marker) {
					reply = true
				}
				if item.Type == "functionCallOutput" && item.Name == "comms_receive" && strings.Contains(item.Output, marker) {
					toolCount++
					var p map[string]any
					if err := json.Unmarshal([]byte(item.Output), &p); err != nil {
						t.Fatal(err)
					}
					if p["message_id"] != message.ID || p["sender_machine_id"] != message.SenderMachine || p["sender_agent_id"] != message.SenderAgent || p["recipient_agent_id"] != message.RecipientAgent || p["authority"] != "external_peer_content" || p["origin_authenticated"] != true || p["claims_verified"] != false {
						t.Fatalf("node provenance mismatch: %+v", p)
					}
				}
			}
		}
		if toolCount > 1 {
			t.Fatal("same message handed off more than once")
		}
		if toolCount == 1 && reply {
			t.Logf("verified model-visible functionCallOutput and fresh reply for %s", marker)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(fmt.Errorf("no native model reply for %s: %w", marker, ctx.Err()))
		case <-time.After(500 * time.Millisecond):
		}
	}
}
