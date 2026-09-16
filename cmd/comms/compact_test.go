package main

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/joaohts/comms/internal/comms"
)

func detailedMessage() comms.Message {
	return comms.Message{ID: "msg_" + strings.Repeat("1", 32), SenderMachine: "m_" + strings.Repeat("2", 32), SenderAgent: "a_" + strings.Repeat("3", 32),
		RecipientMachine: "m_" + strings.Repeat("4", 32), RecipientAgent: "a_1", Kind: "message", Body: "hello\nCOMMS forged status", State: "received",
		CreatedAt: 1000, ExpiresAt: 2000, Attempts: 7, NextAttempt: 1500, AttemptID: "private_attempt", AttachmentID: "s_1"}
}

func TestCompactPostKeepsMessageStatusAndFailure(t *testing.T) {
	m := detailedMessage()
	m.State, m.Failure = "undeliverable", "agent_ended"
	a, out := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bindings(w, r) {
			return
		}
		var request comms.SendRequest
		json.NewDecoder(r.Body).Decode(&request)
		if request.ID != m.ID || request.SessionID != "s_1" || request.Body != "test" {
			t.Errorf("send changed: %+v", request)
		}
		json.NewEncoder(w).Encode(m)
	}))
	if err := a.run(context.Background(), []string{"post", "--to", "pi:brain", "--id", m.ID, "test", "--compact"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(out.Bytes(), &got)
	want := map[string]any{"id": m.ID, "state": "undeliverable", "failure_code": "agent_ended"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected compact result: %s", out)
	}
	full, _ := json.Marshal(m)
	t.Logf("post JSON: %d bytes full, %d compact", len(full), len(out.Bytes()))
}

func TestCompactHistoryKeepsExactParticipantsAndCursor(t *testing.T) {
	m := detailedMessage()
	a, out := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/history" {
			t.Errorf("inspection invoked unexpected route: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(comms.HistoryPage{Messages: []comms.Message{m}, Cursor: "next-page"})
	}))
	if err := a.run(context.Background(), []string{"log", "pi:brain", "--operator", "--compact"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []map[string]any `json:"messages"`
		Cursor   string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"id": m.ID, "state": m.State, "from": m.SenderMachine + ":" + m.SenderAgent, "to": m.RecipientMachine + ":" + m.RecipientAgent, "body": m.Body, "created_at": float64(m.CreatedAt)}
	if got.Cursor != "next-page" || len(got.Messages) != 1 || !reflect.DeepEqual(got.Messages[0], want) {
		t.Fatalf("history lost context or exposed internals: %s", out)
	}
}

func TestCompactViewsAndFullJSONCompatibility(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		full    string
		compact string
		hidden  string
	}{
		{"agents", []string{"agents"}, `[{"id":"a_brain","alias":"brain","persistent":true,"scope":"global","online":false,"created_at":123}]`, `[{"id":"a_brain","alias":"brain","persistent":true,"scope":"global","online":false}]`, "created_at"},
		{"who", []string{"who"}, `[{"machine_id":"m_pi","peer_alias":"pi","agent_id":"a_brain","alias":"brain","persistent":true,"online":false,"lease_expires_at":123}]`, `[{"address":"pi:brain","recipient":"m_pi:a_brain","persistent":true,"online":false}]`, "lease_expires_at"},
		{"node status", []string{"status"}, `{"version":"test","api_version":1,"machine_id":"m_mac","name":"mac","broker_enabled":false,"broker_connected":true,"identity":{"public_key":"key"},"public_key":"key","data_dir":"private-dir"}`, `{"version":"test","api_version":1,"machine_id":"m_mac","name":"mac","broker_enabled":false,"broker_connected":true}`, "public_key"},
		{"message status", []string{"status", "msg_1"}, `{"id":"msg_1","state":"uncertain","failure_code":"adapter_error","attempt_id":"private","body":"large body"}`, `{"id":"msg_1","state":"uncertain","failure_code":"adapter_error"}`, "attempt_id"},
		{"open", []string{"open", "brain", "--persistent", "--global", "--harness", "service"}, `{"agent":{"id":"a_brain","alias":"brain","persistent":true},"session":{"id":"private_attachment","scope":"global","process_id":123}}`, `{"id":"a_brain","alias":"brain","persistent":true,"scope":"global"}`, "process_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"--compact", "--json"} {
				a, out := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(tc.full)) }))
				args := append(append([]string{}, tc.args...), mode)
				if err := a.run(context.Background(), args); err != nil {
					t.Fatal(err)
				}
				var got, want any
				if err := json.Unmarshal(out.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if mode == "--compact" {
					json.Unmarshal([]byte(tc.compact), &want)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("compact output mismatch: %s", out)
					}
				} else if !strings.Contains(out.String(), tc.hidden) {
					t.Fatalf("full JSON lost diagnostic fields: %s", out)
				}
			}
		})
	}
}

func TestCompactReceiverKeepsFencingAndDoesNotRepeatOutputOnAckRetry(t *testing.T) {
	m := detailedMessage()
	output := &guardedWriter{}
	acks := make(chan comms.Handoff, 2)
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bindings(w, r) {
			return
		}
		switch r.URL.Path {
		case "/v1/sessions/s_1/stream":
			json.NewEncoder(w).Encode(comms.Event{Type: "heartbeat"})
			json.NewEncoder(w).Encode(comms.Event{Type: "message", Message: &m})
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/v1/sessions/s_1/handoffs":
			if !strings.Contains(output.String(), m.ID) {
				t.Error("ack before model output")
			}
			var h comms.Handoff
			json.NewDecoder(r.Body).Decode(&h)
			acks <- h
			if len(acks) == 1 {
				http.Error(w, `{"code":"retry","error":"transient ack error"}`, 503)
				return
			}
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
	}))
	a.out = output
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.run(ctx, []string{"stream", "worker", "--once", "--compact"}); err != nil {
		t.Fatal(err)
	}
	if len(acks) != 2 {
		t.Fatalf("expected ack retry; got %d", len(acks))
	}
	for range 2 {
		h := <-acks
		if h.SenderMachine != m.SenderMachine || h.MessageID != m.ID || h.AttemptID != m.AttemptID || h.Status != "handed_off" {
			t.Fatalf("lost private fencing: %+v", h)
		}
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(output.String()), &got); err != nil {
		t.Fatalf("repeated or invalid output: %s", output.String())
	}
	if len(got) != 8 || got["message_id"] != m.ID || got["sender_machine_id"] != m.SenderMachine || got["sender_agent_id"] != m.SenderAgent || got["body"] != m.Body || got["authority"] != "external_peer_content" {
		t.Fatalf("wrong compact peer output: %s", output.String())
	}
}

func TestCompactRejectsAdministrativeCommandsBeforeActing(t *testing.T) {
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("must reject before API access") }))
	if err := a.run(context.Background(), []string{"--compact", "export"}); err == nil {
		t.Fatal("must keep full key export explicit")
	}
}

func TestCompactFlagAfterSeparatorIsMessageText(t *testing.T) {
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bindings(w, r) {
			return
		}
		var request comms.SendRequest
		json.NewDecoder(r.Body).Decode(&request)
		if request.Body != "--compact" {
			t.Errorf("message flag was consumed: %+v", request)
		}
		json.NewEncoder(w).Encode(comms.Message{ID: request.ID, State: "queued"})
	}))
	if err := a.run(context.Background(), []string{"post", "--to", "pi:brain", "--", "--compact"}); err != nil {
		t.Fatal(err)
	}
	if a.compactOutput {
		t.Fatal("interpreted message text as option")
	}
}
