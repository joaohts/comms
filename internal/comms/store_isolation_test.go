package comms

import (
	"context"
	"testing"
	"time"
)

func TestHistoryReadersCannotOccupyWriterConnection(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	s, e := OpenStore(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	one, e := s.ReadDB.Conn(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer one.Close()
	two, e := s.ReadDB.Conn(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer two.Close()
	done := make(chan error, 1)
	go func() {
		_, e := s.OpenAgent(OpenRequest{Alias: "writer", Harness: "service", HarnessID: "writer"})
		done <- e
	}()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("saturated history readers blocked identity/queue writer")
	}
}

func TestExplicitUncertainResolution(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	s, e := OpenStore(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	a, e := s.OpenAgent(OpenRequest{Alias: "target", Harness: "service", HarnessID: "target"})
	if e != nil {
		t.Fatal(e)
	}
	for _, status := range []string{"handed_off", "retry", "undeliverable"} {
		t.Run(status, func(t *testing.T) {
			m := Message{ID: NewID("msg_"), SenderMachine: s.Identity.MachineID, SenderAgent: a.Agent.ID, RecipientMachine: s.Identity.MachineID, RecipientAgent: a.Agent.ID, Kind: "message", Body: "possibly delivered", State: "uncertain", CreatedAt: Now(), ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), AttemptID: NewID("d_"), AttachmentID: a.Session.ID, Hash: "fixture"}
			if _, _, e := s.Insert(m, false); e != nil {
				t.Fatal(e)
			}
			if e := s.ResolveUncertain(m.SenderMachine, m.ID, status, "", ""); e != nil {
				t.Fatal(e)
			}
			got, e := s.Message(m.SenderMachine, m.ID)
			if e != nil {
				t.Fatal(e)
			}
			want := status
			if status == "retry" {
				want = "received"
			}
			if got.State != want {
				t.Fatalf("got %s want %s", got.State, want)
			}
			if status == "retry" && got.AttemptID != "" {
				t.Fatal("old attempt was not fenced")
			}
		})
	}
}
