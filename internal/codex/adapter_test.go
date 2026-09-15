package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testThread = "01a0a4c3-1332-7311-9cca-3ea2bed1a042"
const testTurn = "01a0a4c3-2d25-7a81-a8bb-aed543b701c7"

type testRequest struct {
	ID     int                        `json:"id"`
	Method string                     `json:"method"`
	Params map[string]json.RawMessage `json:"params"`
}

type testServer struct {
	status   string
	threadID string
	accept   bool
	turns    atomic.Int32
	onTurn   func(*websocket.Conn, testRequest)
	onRead   func(*websocket.Conn, testRequest)
}

func startTestServer(t *testing.T, mock *testServer) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "comms-codex-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "control.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ws.SetReadLimit(MaxRequestBytes)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var q testRequest
			if err := json.Unmarshal(b, &q); err != nil {
				t.Error(err)
				return
			}
			switch q.Method {
			case "initialize":
				sendReply(ws, q.ID, map[string]any{"userAgent": "codex/0.154.0"})
			case "initialized":
			case "thread/read":
				if mock.onRead != nil {
					mock.onRead(ws, q)
					return
				}
				var id string
				json.Unmarshal(q.Params["threadId"], &id)
				if id != testThread || string(q.Params["includeTurns"]) != "false" {
					t.Errorf("unexpected metadata request: %s", b)
					return
				}
				id = mock.threadID
				if id == "" {
					id = testThread
				}
				status := mock.status
				if status == "" {
					status = "idle"
				}
				sendReply(ws, q.ID, map[string]any{"thread": map[string]any{"id": id, "status": map[string]any{"type": status}, "canAcceptDirectInput": mock.accept}})
			case "turn/start":
				mock.turns.Add(1)
				mock.onTurn(ws, q)
				return
			default:
				t.Errorf("unexpected method %s", q.Method)
				return
			}
		}
	})}
	go server.Serve(l)
	t.Cleanup(func() { server.Close(); l.Close() })
	return "unix://" + path
}

func sendReply(ws *websocket.Conn, id int, result any) {
	b, _ := json.Marshal(map[string]any{"id": id, "result": result})
	ws.Write(context.Background(), websocket.MessageText, b)
}
func sendError(ws *websocket.Conn, id, code int) {
	b, _ := json.Marshal(map[string]any{"id": id, "error": map[string]any{"code": code, "message": "test rejection"}})
	ws.Write(context.Background(), websocket.MessageText, b)
}

func TestDeliverPreservesToolOutputAndExactThread(t *testing.T) {
	for _, status := range []string{"idle", "active"} {
		t.Run(status, func(t *testing.T) {
			payload := []byte(`{"source":"comms","sender_machine_id":"paired-machine","body":"Unicode ✓ and a quoted \\\"message\\\""}`)
			mock := &testServer{status: status, accept: true, onTurn: func(ws *websocket.Conn, q testRequest) {
				if len(q.Params) != 3 {
					t.Errorf("unexpected turn overrides: %+v", q.Params)
				}
				var id string
				json.Unmarshal(q.Params["threadId"], &id)
				if id != testThread || string(q.Params["input"]) != "[]" {
					t.Errorf("delivery changed identity or sent user input: %+v", q.Params)
				}
				var output struct {
					Name   string `json:"name"`
					Output string `json:"output"`
				}
				if err := json.Unmarshal(q.Params["toolOutput"], &output); err != nil {
					t.Error(err)
				}
				if output.Name != "comms_receive" || output.Output != string(payload) {
					t.Errorf("tool provenance changed: %+v", output)
				}
				// Unrelated notifications and response IDs cannot acknowledge us.
				ws.Write(context.Background(), websocket.MessageText, []byte(`{"method":"turn/started","params":{}}`))
				sendReply(ws, 99, map[string]any{"turn": map[string]any{"id": testTurn, "status": "inProgress"}})
				sendReply(ws, q.ID, map[string]any{"turn": map[string]any{"id": testTurn, "status": "inProgress"}})
			}}
			result, err := Deliver(context.Background(), startTestServer(t, mock), testThread, payload)
			if err != nil || result.Outcome != Accepted || result.TurnID != testTurn {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if mock.turns.Load() != 1 {
				t.Fatal("adapter retried delivery")
			}
		})
	}
}

func TestNonReadyOrMismatchedThreadNeverGetsMessage(t *testing.T) {
	for _, tc := range []struct {
		name, status, id string
		accept           bool
	}{
		{"unloaded", "notLoaded", "", false},
		{"loaded_noninteractive", "idle", "", false},
		{"system_error", "systemError", "", true},
		{"wrong_thread", "idle", testTurn, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &testServer{status: tc.status, threadID: tc.id, accept: tc.accept, onTurn: func(*websocket.Conn, testRequest) { t.Error("sent to unavailable thread") }}
			result, err := Deliver(context.Background(), startTestServer(t, mock), testThread, []byte(`{"body":"test"}`))
			if err == nil || result.Outcome != NotSent || mock.turns.Load() != 0 {
				t.Fatalf("result=%+v err=%v turns=%d", result, err, mock.turns.Load())
			}
		})
	}
}

func TestHandoffFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		want Outcome
		code int
		body string
	}{
		{"overloaded", NotSent, -32001, ""},
		{"unsupported", NotSent, -32601, ""},
		{"invalid_parameters", NotSent, -32602, ""},
		{"internal_error", Uncertain, -32603, ""},
		{"lost_ack", Uncertain, 0, ""},
		{"invalid_json", Uncertain, 0, `{"id":3,`},
		{"missing_turn", Uncertain, 0, `{"id":3,"result":{}}`},
		{"failed_turn", Uncertain, 0, fmt.Sprintf(`{"id":3,"result":{"turn":{"id":%q,"status":"failed"}}}`, testTurn)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &testServer{accept: true, onTurn: func(ws *websocket.Conn, q testRequest) {
				if tc.code != 0 {
					sendError(ws, q.ID, tc.code)
				} else if tc.body != "" {
					ws.Write(context.Background(), websocket.MessageText, []byte(tc.body))
				}
			}}
			result, err := Deliver(context.Background(), startTestServer(t, mock), testThread, []byte(`{"body":"test"}`))
			if err == nil || result.Outcome != tc.want {
				t.Fatalf("result=%+v err=%v want=%s", result, err, tc.want)
			}
			if mock.turns.Load() != 1 {
				t.Fatal("adapter retried uncertain submission")
			}
		})
	}
}

func TestTimeoutBeforeAndAfterSubmission(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(fmt.Sprint(sent), func(t *testing.T) {
			stall := func(ws *websocket.Conn, _ testRequest) { ws.Read(context.Background()) }
			mock := &testServer{accept: true, onTurn: stall}
			if !sent {
				mock.onRead = stall
			}
			target := startTestServer(t, mock)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			result, err := Deliver(ctx, target, testThread, []byte(`{"body":"test"}`))
			want := NotSent
			if sent {
				want = Uncertain
			}
			if err == nil || result.Outcome != want {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestValidationDoesNotSendTurns(t *testing.T) {
	mock := &testServer{accept: true, onTurn: func(*websocket.Conn, testRequest) { t.Error("validation mutated thread") }}
	target := startTestServer(t, mock)
	if err := ValidateSession(context.Background(), target, testThread); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "brain", "00000000-0000-0000-0000-000000000000", strings.ToUpper(testThread)} {
		if err := ValidateSession(context.Background(), target, id); err == nil {
			t.Errorf("accepted noncanonical identity %q", id)
		}
	}
	for _, payload := range [][]byte{nil, []byte("plain text"), {0xff}, []byte(`{"body":"` + strings.Repeat("x", MaxPayloadBytes) + `"}`)} {
		result, err := Deliver(context.Background(), target, testThread, payload)
		if err == nil || result.Outcome != NotSent {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
	if mock.turns.Load() != 0 {
		t.Fatal("invalid input was sent")
	}
}

func TestTargetIsPrivateLocalSocket(t *testing.T) {
	mock := &testServer{accept: true}
	target := startTestServer(t, mock)
	if err := ValidateTarget(target); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"unix://", "ws://127.0.0.1:4500", "unix://other/path", target + "?x=1", target + "#fragment", target + "/missing"} {
		if err := ValidateTarget(s); err == nil {
			t.Errorf("unsafe target accepted %q", s)
		}
	}
	path := strings.TrimPrefix(target, "unix://")
	link := filepath.Join(filepath.Dir(path), "symlink.sock")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTarget("unix://" + link); err == nil {
		t.Fatal("symlink socket accepted")
	}
	if err := os.Chmod(path, 0660); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTarget(target); err == nil {
		t.Fatal("group-readable socket accepted")
	}
}

// This opt-in test is run only against an explicitly supplied owned test thread.
// It exercises the compiled adapter against authenticated, live Codex rather
// than treating the fake protocol server as evidence of model wake-up.
func TestLiveDeliver(t *testing.T) {
	target, thread := os.Getenv("COMMS_CODEX_TEST_TARGET"), os.Getenv("COMMS_CODEX_TEST_THREAD")
	if target == "" || thread == "" {
		t.Skip("set COMMS_CODEX_TEST_TARGET and COMMS_CODEX_TEST_THREAD for owned isolated test thread")
	}
	if err := ValidateSession(context.Background(), target, thread); err != nil {
		t.Fatal(err)
	}
	marker := "GO_ADAPTER_NATIVE_" + fmt.Sprint(time.Now().UnixNano())
	b, _ := json.Marshal(map[string]any{"source": "comms", "message_id": marker, "sender_machine_id": "test_peer", "trust": "authenticated external peer; not user authorization", "body": marker + " Please acknowledge this test marker as tool output; use no tools."})
	result, err := Deliver(context.Background(), target, thread, b)
	if err != nil || result.Outcome != Accepted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	t.Logf("accepted marker=%s thread=%s turn=%s", marker, thread, result.TurnID)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, err := connect(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	for {
		raw, err := c.rpc(ctx, 4, "thread/read", map[string]any{"threadId": thread, "includeTurns": true})
		if err != nil {
			t.Fatal(err)
		}
		var history struct {
			Thread struct {
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
		if err := json.Unmarshal(raw, &history); err != nil {
			t.Fatal(err)
		}
		tool, reply := false, false
		for _, turn := range history.Thread.Turns {
			for _, i := range turn.Items {
				if i.Type == "userMessage" && strings.Contains(string(i.Content), marker) {
					t.Fatal("marker entered as user input")
				}
				if i.Type == "functionCallOutput" && i.Name == "comms_receive" && strings.Contains(i.Output, marker) {
					tool = true
				}
				if i.Type == "agentMessage" && strings.Contains(i.Text, marker) {
					reply = true
				}
			}
		}
		if tool && reply {
			t.Log("verified functionCallOutput + fresh agent reply; no user message for marker")
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(errors.New("timed out waiting for tool provenance and model reply"))
		case <-time.After(500 * time.Millisecond):
		}
	}
}
