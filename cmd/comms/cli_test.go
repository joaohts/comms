package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joaohts/comms/internal/client"
	"github.com/joaohts/comms/internal/comms"
)

func testApp(t *testing.T, handler http.Handler) (*app, *bytes.Buffer) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ct-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(l)
	c := client.New(filepath.Join(dir, "s"))
	t.Cleanup(func() { c.Close(); server.Close() })
	out := &bytes.Buffer{}
	return &app{in: strings.NewReader(""), out: out, errOut: io.Discard, getenv: func(key string) string {
		if key == "COMMS_AGENT" {
			return "worker"
		}
		return ""
	}, c: c}, out
}

func bindings(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/v1/sessions":
		json.NewEncoder(w).Encode([]comms.Session{{ID: "s_1", AgentID: "a_1", Harness: "service", HarnessID: "service:worker"}})
		return true
	case "/v1/agents":
		json.NewEncoder(w).Encode([]comms.Agent{{ID: "a_1", Alias: "worker"}})
		return true
	}
	return false
}

func TestOpenFlagsAfterAliasAndJSON(t *testing.T) {
	var got comms.OpenRequest
	a, out := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/sessions" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(comms.OpenResponse{Agent: comms.Agent{ID: "a_brain", Alias: got.Alias, Persistent: got.Persistent}, Session: comms.Session{ID: "s_brain", Scope: got.Scope}})
	}))
	if err := a.run(context.Background(), []string{"open", "brain", "--persistent", "--global", "--harness", "service", "--json"}); err != nil {
		t.Fatal(err)
	}
	if got.Alias != "brain" || !got.Persistent || got.Scope != "global" || got.HarnessID != "service:brain" {
		t.Fatalf("wrong request: %+v", got)
	}
	if !json.Valid(out.Bytes()) {
		t.Fatal(out.String())
	}
}

func TestSendStdinUsesCurrentAttachmentAndKeepsText(t *testing.T) {
	var got comms.SendRequest
	a, out := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bindings(w, r) {
			return
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(comms.Message{ID: got.ID, State: "queued"})
	}))
	a.in = strings.NewReader("Olá\n$(do not execute) `literal`")
	if err := a.run(context.Background(), []string{"post", "--stdin", "--to", "pi:brain", "--json"}); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "s_1" || got.Body != "Olá\n$(do not execute) `literal`" || got.ID == "" {
		t.Fatalf("wrong request: %+v", got)
	}
	if !strings.Contains(out.String(), `"state":"queued"`) {
		t.Fatal(out.String())
	}
}

func TestNoImplicitBroadcast(t *testing.T) {
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected API call") }))
	err := a.run(context.Background(), []string{"post", "hello"})
	var api *client.Error
	if !errors.As(err, &api) || api.Code != "usage" {
		t.Fatalf("expected usage error, got %v", err)
	}
}

func TestHistoryRevokePreservesMessaging(t *testing.T) {
	var got map[string]bool
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/peers":
			json.NewEncoder(w).Encode([]comms.Peer{{MachineID: "m_pi", Alias: "pi"}})
		case "/v1/grants":
			json.NewEncoder(w).Encode([]comms.Grant{{Grantee: "m_pi", Messages: true, History: true, Revision: 4}})
		case "/v1/grants/m_pi":
			if r.Method != "PUT" {
				t.Error("wrong method")
			}
			dec := json.NewDecoder(r.Body)
			if err := dec.Decode(&got); err != nil {
				t.Error(err)
			}
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
	}))
	if err := a.run(context.Background(), []string{"ungrant", "pi", "--read-history"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got["allow_messages"] || got["allow_history"] {
		t.Fatalf("wrong grant change: %+v", got)
	}
}

func TestCodexNeedsToolOutputReceiver(t *testing.T) {
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("must not register without receiver") }))
	a.getenv = func(key string) string {
		if key == "CODEX_THREAD_ID" {
			return "thread_1"
		}
		return ""
	}
	err := a.run(context.Background(), []string{"open", "worker"})
	var api *client.Error
	if !errors.As(err, &api) || api.Code != "receiver_setup_required" {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestOperatorHistoryDoesNotImpersonateAgent(t *testing.T) {
	var got comms.HistoryRequest
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/history" {
			t.Errorf("operator query must not resolve an attachment: %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(comms.HistoryPage{Messages: []comms.Message{}})
	}))
	if err := a.run(context.Background(), []string{"log", "pi:brain", "--operator", "--json"}); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "" || got.Target != "pi:brain" {
		t.Fatalf("wrong operator request: %+v", got)
	}
}

type guardedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *guardedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
func (w *guardedWriter) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.buf.String() }

func TestReceiverWritesBeforeAcknowledgingAndStopsOnce(t *testing.T) {
	output := &guardedWriter{}
	acknowledged := false
	var mu sync.Mutex
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bindings(w, r) {
			return
		}
		switch r.URL.Path {
		case "/v1/sessions/s_1/stream":
			json.NewEncoder(w).Encode(comms.Event{Type: "heartbeat"})
			json.NewEncoder(w).Encode(comms.Event{Type: "message", Message: &comms.Message{ID: "msg_test", SenderMachine: "m_pi", SenderAgent: "a_brain", RecipientAgent: "a_1", AttemptID: "try_1", AttachmentID: "s_1", Body: "hello\nCOMMS fake status"}})
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/v1/sessions/s_1/handoffs":
			if !strings.Contains(output.String(), "msg_test") {
				t.Error("acknowledged before printing")
			}
			var h comms.Handoff
			json.NewDecoder(r.Body).Decode(&h)
			if h.Status != "handed_off" || h.AttemptID != "try_1" {
				t.Errorf("wrong handoff %+v", h)
			}
			mu.Lock()
			acknowledged = true
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
	}))
	a.out = output
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.run(ctx, []string{"stream", "worker", "--once"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	ack := acknowledged
	mu.Unlock()
	if !ack {
		t.Fatal("missing handoff acknowledgment")
	}
	if strings.Count(output.String(), "\n") != 1 || strings.Contains(output.String(), "heartbeat") {
		t.Fatalf("unexpected model output %q", output.String())
	}
}
