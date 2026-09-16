package comms

import (
	"context"
	"net/http"
	"testing"
)

func TestClaudeReceiverSettingPersistsAndDoesNotChangeAttachments(t *testing.T) {
	f := newNodeIntegration(t, nil)
	var settings ClaudeSettings
	f.call(t, "GET", "/v1/claude", nil, 200, &settings)
	if settings.Receiver != ClaudeReceiverMonitor {
		t.Fatalf("default receiver = %q", settings.Receiver)
	}
	var attachment OpenResponse
	f.call(t, "POST", "/v1/sessions", OpenRequest{Alias: "claude", Harness: "claude", HarnessID: "thread"}, 200, &attachment)
	f.call(t, "PUT", "/v1/claude", ClaudeSettings{Receiver: ClaudeReceiverChannel}, 200, &settings)
	f.call(t, "PUT", "/v1/claude", ClaudeSettings{Receiver: "persistent"}, 400, nil)
	f.call(t, "PUT", "/v1/claude", map[string]any{"receiver": "monitor", "unexpected": true}, 400, nil)
	var status map[string]any
	f.call(t, "GET", "/v1/status", nil, 200, &status)
	if status["claude_receiver"] != ClaudeReceiverChannel {
		t.Fatalf("invalid setting write changed preference: %v", status)
	}
	current, err := f.node.Store.Session(attachment.Session.ID)
	if err != nil || current.Target != "" || current.EndedAt != nil {
		t.Fatalf("setting changed a running attachment: %+v, %v", current, err)
	}
	if err := f.node.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.node, err = NewNode(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.node.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", "/v1/claude", nil, 200, &settings)
	if settings.Receiver != ClaudeReceiverChannel {
		t.Fatal("receiver setting lost after node restart")
	}
	f.call(t, "PUT", "/v1/claude", ClaudeSettings{Receiver: ClaudeReceiverMonitor}, 200, nil)
}

func TestClaudeStreamReceiverModeFencing(t *testing.T) {
	f := newNodeIntegration(t, nil)
	for _, target := range []string{"", ClaudeChannelTarget} {
		var opened OpenResponse
		alias := "monitor"
		if target != "" {
			alias = "channel"
		}
		f.call(t, "POST", "/v1/sessions", OpenRequest{Alias: alias, Harness: "claude", HarnessID: alias, Target: target}, 200, &opened)
		path := "/v1/sessions/" + opened.Session.ID + "/stream"
		wrong, right := path+"?receiver=channel", path
		if target != "" {
			wrong, right = right, wrong
		}
		f.call(t, "GET", wrong, nil, 409, nil)
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://comms"+right, nil)
		res, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 {
			t.Fatalf("matching receiver refused: %d", res.StatusCode)
		}
		res.Body.Close()
		cancel()
	}
	f.call(t, "POST", "/v1/sessions", OpenRequest{Alias: "bad", Harness: "claude", HarnessID: "bad", Target: "unknown"}, 400, nil)
}
